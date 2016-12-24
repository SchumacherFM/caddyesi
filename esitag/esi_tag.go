package esitag

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/SchumacherFM/caddyesi/bufpool"
	"github.com/SchumacherFM/caddyesi/helpers"
	"github.com/corestoreio/errors"
	"golang.org/x/sync/errgroup"
)

// TemplateIdentifier if some strings contain these characters then a
// template.Template will be created. For now a resource key or an URL is
// supported.
const TemplateIdentifier = "{{"

// Conditioner does not represent your favorite shampoo but it gives you the
// possibility to define an expression which gets executed for every request to
// include the ESI resource or not.
type Conditioner interface {
	OK(r *http.Request) bool
}

type condition struct {
	*template.Template
}

func (c condition) OK(r *http.Request) bool {
	// todo
	return false
}

// Tag identifies an ESI tag by its start and end position in the HTML byte
// stream for replacing. If the HTML changes there needs to be a refresh call to
// re-parse the HTML.
type Tag struct {
	// Data from the micro service gathered in a goroutine.
	Data  []byte
	Start int // start position in the stream
	End   int // end position in the stream
}

// Entity represents a single fully parsed ESI tag
type Entity struct {
	RawTag            []byte
	Tag               Tag
	Resources         // Any 3rd party servers
	TTL               time.Duration
	Timeout           time.Duration
	OnError           string
	ForwardHeaders    []string
	ForwardHeadersAll bool
	ReturnHeaders     []string
	ReturnHeadersAll  bool
	// Key defines a key in a KeyValue server to fetch the value from.
	Key string
	// KeyTemplate gets created when the Key field contains the template
	// identifier. Then the Key field would be empty.
	KeyTemplate *template.Template
	Conditioner // todo
}

// SplitAttributes splits an ESI tag by its attributes
func SplitAttributes(raw string) ([]string, error) {
	// include src='https://micro.service/checkout/cart={{ .r "x"}}' timeout="9ms" onerror="nocart.html" forwardheaders="Cookie,Accept-Language,Authorization"

	var lastQuote rune
	f := func(c rune) bool {
		// I have no idea why my code is working ;-|
		switch {
		case c == lastQuote:
			lastQuote = 0
			return false
		case lastQuote != 0:
			return false
		case unicode.In(c, unicode.Quotation_Mark):
			lastQuote = c
			return false
		default:
			return unicode.IsSpace(c) || c == '='
		}
	}

	ret := strings.FieldsFunc(raw, f)
	if len(ret) == 0 {
		return []string{}, nil
	}

	ret = ret[1:] // first index is always the word "include", so drop it
	if len(ret)%2 == 1 {
		return nil, errors.NewNotValidf("[esitag] Imbalanced attributes in %#v", ret)
	}
	for i := 0; i < len(ret); i = i + 2 {
		val := ret[i+1]
		if l := len(val); l-1 > 1 {
			val = val[1 : len(val)-1] // drop first and last character, should be a quotation mark
		}
		ret[i+1] = strings.TrimSpace(val)
	}

	return ret, nil
}

// ParseRaw parses the RawTag field and fills the remaining fields of the
// struct.
func (et *Entity) ParseRaw() error {
	if len(et.RawTag) == 0 {
		return nil
	}
	et.Resources.Logf = log.Printf
	et.Resources.Items = make([]*Resource, 0, 2)

	matches, err := SplitAttributes(string(et.RawTag))
	if err != nil {
		return errors.Wrap(err, "[esitag] Parse SplitAttributes")
	}

	srcCounter := 0
	for j := 0; j < len(matches); j = j + 2 {

		attr := matches[j]
		value := matches[j+1]

		switch attr {
		case "src":
			if err := et.parseResource(srcCounter, value); err != nil {
				return errors.Wrapf(err, "[caddyesi] Failed to parse src %q in tag %q", value, et.RawTag)
			}
			srcCounter++
		case "key":
			if err := et.parseKey(value); err != nil {
				return errors.Wrapf(err, "[caddyesi] Failed to parse src %q in tag %q", value, et.RawTag)
			}
		case "condition":
			if err := et.parseCondition(value); err != nil {
				return errors.Wrapf(err, "[caddyesi] Failed to parse condition %q in tag %q", value, et.RawTag)
			}
		case "onerror":
			et.OnError = value
		case "timeout":
			var err error
			et.Timeout, err = time.ParseDuration(value)
			if err != nil {
				return errors.NewNotValidf("[caddyesi] ESITag.ParseRaw. Cannot parse duration in timeout: %s => %q\nTag: %q", err, value, et.RawTag)
			}
		case "ttl":
			var err error
			et.TTL, err = time.ParseDuration(value)
			if err != nil {
				return errors.NewNotValidf("[caddyesi] ESITag.ParseRaw. Cannot parse duration in ttl: %s => %q\nTag: %q", err, value, et.RawTag)
			}
		case "forwardheaders":
			if value == "all" {
				et.ForwardHeadersAll = true
			} else {
				et.ForwardHeaders = helpers.CommaListToSlice(value)
			}
		case "returnheaders":
			if value == "all" {
				et.ReturnHeadersAll = true
			} else {
				et.ReturnHeaders = helpers.CommaListToSlice(value)
			}
		default:
			// if an attribute starts with x we'll ignore it because the
			// developer might want to temporarily disable an attribute.
			if len(attr) > 1 && attr[0] != 'x' {
				return errors.NewNotSupportedf("[esitag] Unsupported attribute name %q with value %q", attr, value)
			}
		}
	}
	if len(et.Resources.Items) == 0 || srcCounter == 0 {
		return errors.NewEmptyf("[caddyesi] ESITag.ParseRaw. src (Items: %d/Src: %d) cannot be empty in Tag which requires at least one resource: %q", len(et.Resources.Items), srcCounter, et.RawTag)
	}
	return nil
}

func (et *Entity) parseCondition(s string) error {
	tpl, err := template.New("condition_tpl").Parse(s)
	if err != nil {
		return errors.NewFatalf("[caddyesi] ESITag.ParseRaw. Failed to parse %q as template with error: %s\nTag: %q", s, err, et.RawTag)
	}
	et.Conditioner = condition{Template: tpl}
	return nil
}

func (et *Entity) parseResource(idx int, val string) (err error) {
	r := &Resource{
		Index: idx,
		IsURL: strings.Contains(val, "://"),
		URL:   val,
	}
	if r.IsURL && strings.Contains(val, TemplateIdentifier) {
		r.URLTemplate, err = template.New("resource_tpl").Parse(val)
		if err != nil {
			return errors.NewFatalf("[caddyesi] ESITag.ParseRaw. Failed to parse %q as template with error: %s\nTag: %q", val, err, et.RawTag)
		}
		r.URL = ""
	}
	et.Items = append(et.Items, r)
	return nil
}

func (et *Entity) parseKey(val string) (err error) {
	et.Key = val
	if strings.Contains(val, TemplateIdentifier) {
		et.KeyTemplate, err = template.New("key_tpl").Parse(val)
		if err != nil {
			return errors.NewFatalf("[caddyesi] ESITag.ParseRaw. Failed to parse %q as template with error: %s\nTag: %q", val, err, et.RawTag)
		}
		et.Key = "" // unset Key because we have a template
	}
	return nil
}

// Entities represents a list of ESI tags found in one HTML page.
type Entities []*Entity

// ParseRaw parses all ESI tags
func (et Entities) ParseRaw() error {
	for i := range et {
		if err := et[i].ParseRaw(); err != nil {
			return errors.Wrapf(err, "[caddyesi] Entities ParseRaw failed at index %d", i)
		}
	}
	return nil
}

// String for debugging only!
func (et Entities) String() string {
	buf := bufpool.Get()
	defer bufpool.Put(buf)

	for i, e := range et {
		raw := e.RawTag
		e.RawTag = nil
		_, _ = fmt.Fprintf(buf, "%d: %#v\n", i, e)
		_, _ = fmt.Fprintf(buf, "%d: RawTag: %q\n\n", i, raw)
	}
	return buf.String()
}

// QueryResources runs in parallel to query all available backend services /
// resources which are available in the current page. The returned Tag slice
// does not guarantee to be ordered. If the request gets canceled via its
// context then all resource requests gets canceled too.
func (et Entities) QueryResources(r *http.Request) ([]Tag, error) {

	tags := make([]Tag, 0, len(et))
	g, ctx := errgroup.WithContext(r.Context())
	cTag := make(chan Tag)
	for _, e := range et {
		e := e
		g.Go(func() error {
			data, err := e.Resources.DoRequest(e.Timeout, r)
			if err != nil {
				return errors.Wrapf(err, "[esitag] QueryResources.Resources.DoRequest failed for Tag %q", e.RawTag)
			}
			t := e.Tag
			t.Data = data

			select {
			case cTag <- t:
			case <-ctx.Done():
				return errors.Wrap(ctx.Err(), "[esitag] Context Done!")
			}
			return nil
		})
	}
	go func() {
		g.Wait()
		close(cTag)
	}()

	for t := range cTag {
		tags = append(tags, t)
	}

	// Check whether any of the goroutines failed. Since g is accumulating the
	// errors, we don't need to send them (or check for them) in the individual
	// results sent on the channel.
	if err := g.Wait(); err != nil {
		return nil, errors.Wrap(err, "[esitag]")
	}

	return tags, nil
}
