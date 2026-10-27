package respond

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// The one rule for a request body Velox defines (every route but the
// Stripe and Postmark webhooks and the LiteLLM callback, whose senders add
// fields when they like):
//
//   - the body is one JSON value; anything else is a 400 "invalid JSON body";
//   - a key the request type does not declare, at any depth, is a 422 with
//     param set to the key; a key matches its field exactly, in its case;
//   - a key the server sets (ReadOnly) is a 422 with param set to the key;
//   - a route that takes no body refuses one with any key (NoBody).
//
// encoding/json drops unknown keys silently: `trial_period_days` instead of
// `trial_days` created an active, charged subscription with a 201, and a
// PATCH with a misspelled field answered 200 and changed nothing. The
// refusal is a 422, not a 400, because the idempotency middleware caches a
// 400: a corrected retry under the same Idempotency-Key would get the
// refusal replayed.

// DecodeOption changes what DecodeStrict and DecodeBody accept.
type DecodeOption func(*decodeRule)

type decodeRule struct {
	emptyOK  bool
	readOnly []string
}

// EmptyBodyOK lets a route whose body is optional take no body at all (or
// only whitespace, or `null`): v keeps the values it had. A body that is
// there follows the rule.
func EmptyBodyOK(r *decodeRule) { r.emptyOK = true }

// ReadOnly refuses top-level keys the request type declares but the server
// sets, for a route that decodes the type it returns (id, tenant_id,
// created_at). Without it they were accepted and ignored.
func ReadOnly(keys ...string) DecodeOption {
	return func(r *decodeRule) { r.readOnly = append(r.readOnly, keys...) }
}

// BodyError is why DecodeBody refused a body. Field is the refused key; it
// is empty when the body is not valid JSON for the type.
type BodyError struct {
	Field    string
	ReadOnly bool
	Err      error
}

func (e *BodyError) Error() string {
	switch {
	case e.ReadOnly:
		return fmt.Sprintf("%s is set by the server and cannot be sent", e.Field)
	case e.Field != "":
		return fmt.Sprintf("unknown field %q", e.Field)
	default:
		return "invalid JSON body"
	}
}

func (e *BodyError) Unwrap() error { return e.Err }

// DecodeStrict decodes the request body into v under the rule above. It
// writes the refusal and returns false; the handler returns on false.
func DecodeStrict(w http.ResponseWriter, r *http.Request, v any, opts ...DecodeOption) bool {
	if err := DecodeBody(r, v, opts...); err != nil {
		WriteBodyError(w, r, err)
		return false
	}
	return true
}

// NoBody is the rule for a route that takes no body: it lists no field, so
// a body with any field is a 422 naming it. No body, `{}` and `null` are
// fine. Call it before the handler does anything else; it writes the
// refusal and returns false, and the handler returns on false.
func NoBody(w http.ResponseWriter, r *http.Request) bool {
	return DecodeStrict(w, r, &struct{}{}, EmptyBodyOK)
}

// DecodeBody is DecodeStrict for a handler that answers some refusals its
// own way (the usage batch's 413). It returns a *BodyError; pass anything
// it does not handle to WriteBodyError.
func DecodeBody(r *http.Request, v any, opts ...DecodeOption) error {
	var rule decodeRule
	for _, o := range opts {
		o(&rule)
	}
	return decodeBody(r, v, rule)
}

// WriteBodyError writes the response for a DecodeBody error: 422 naming the
// field for a refused key, 400 "invalid JSON body" otherwise.
func WriteBodyError(w http.ResponseWriter, r *http.Request, err error) {
	var be *BodyError
	if errors.As(err, &be) && be.Field != "" {
		ValidationField(w, r, be.Field, be.Error())
		return
	}
	BadRequest(w, r, "invalid JSON body")
}

func decodeBody(r *http.Request, v any, rule decodeRule) error {
	if r.Body == nil {
		if rule.emptyOK {
			return nil
		}
		return &BodyError{Err: io.EOF}
	}
	// Read the one value first, so trailing data after it is refused and
	// the read-only check sees the same bytes the decode does.
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) && rule.emptyOK {
			return nil
		}
		return &BodyError{Err: err}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &BodyError{Err: errors.New("data after the JSON value")}
	}
	return decodeValue(raw, v, rule)
}

// DecodeValue is the rule for one JSON value already read from a body, such
// as one event of the usage batch: a key v does not declare, in any case but
// its own, is a *BodyError naming it.
func DecodeValue(raw json.RawMessage, v any, opts ...DecodeOption) error {
	var rule decodeRule
	for _, o := range opts {
		o(&rule)
	}
	return decodeValue(raw, v, rule)
}

func decodeValue(raw json.RawMessage, v any, rule decodeRule) error {
	if string(raw) == "null" && rule.emptyOK {
		return nil
	}
	if len(rule.readOnly) > 0 {
		var top map[string]json.RawMessage
		// A body that is not an object fails the decode below.
		if json.Unmarshal(raw, &top) == nil {
			for _, ro := range rule.readOnly {
				for k := range top {
					// encoding/json matches keys without regard to case.
					if strings.EqualFold(k, ro) {
						return &BodyError{Field: k, ReadOnly: true}
					}
				}
			}
		}
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(v); err != nil {
		if key, ok := unknownField(err); ok {
			return &BodyError{Field: key, Err: err}
		}
		return &BodyError{Err: err}
	}
	// encoding/json matches a key to a field without regard to case, so
	// {"Email": ...} set email. The reference lists one spelling.
	if key := inexactKey(raw, reflect.TypeOf(v)); key != "" {
		return &BodyError{Field: key}
	}
	return nil
}

var (
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// inexactKey returns the first key in raw, at any depth, that names a field
// of t in another case. raw has already decoded into t, so every key matches
// a field in some case. A value t decodes its own way (an Unmarshaler, raw
// JSON, any) holds no field names, and neither do a map's keys.
func inexactKey(raw json.RawMessage, t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(textUnmarshaler) {
		return ""
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return "" // null
		}
		fields := map[string]reflect.Type{}
		jsonFields(t, fields)
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			ft, ok := fields[k]
			if !ok {
				return k
			}
			if key := inexactKey(obj[k], ft); key != "" {
				return key
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return "" // []byte is a base64 string
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return ""
		}
		for _, it := range items {
			if key := inexactKey(it, t.Elem()); key != "" {
				return key
			}
		}
	case reflect.Map:
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return ""
		}
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			if key := inexactKey(obj[k], t.Elem()); key != "" {
				return key
			}
		}
	}
	return ""
}

// jsonFields adds the JSON names of t's fields, as encoding/json names
// them, to out: the tag's name, else the Go name; "-" is skipped; an
// untagged embedded struct's fields are promoted, and a shallower field of
// the same name wins.
func jsonFields(t reflect.Type, out map[string]reflect.Type) {
	var embedded []reflect.Type
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				embedded = append(embedded, et)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	for _, et := range embedded {
		promoted := map[string]reflect.Type{}
		jsonFields(et, promoted)
		for name, ft := range promoted {
			if _, ok := out[name]; !ok {
				out[name] = ft
			}
		}
	}
}

// unknownField pulls the key out of encoding/json's `json: unknown field "x"`,
// the only form DisallowUnknownFields reports.
func unknownField(err error) (string, bool) {
	quoted, ok := strings.CutPrefix(err.Error(), "json: unknown field ")
	if !ok {
		return "", false
	}
	key, uerr := strconv.Unquote(quoted)
	if uerr != nil {
		return "", false
	}
	return key, true
}
