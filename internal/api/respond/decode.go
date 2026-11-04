package respond

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	kjson "sigs.k8s.io/json"
)

// One rule covers every request body that Hardcount defines. The exceptions
// are the Stripe and Postmark webhooks and the LiteLLM callback, whose
// senders add fields when they like. The rule:
//
//   - The body is one JSON value. Anything else is a 400 "invalid JSON body".
//   - A key the request type does not declare, at any depth, is a 422 with
//     param set to the key. A key matches its field exactly, in its case.
//   - A key the server sets (ReadOnly) is a 422 with param set to the key.
//   - A route that takes no body refuses a body with any key (NoBody).
//
// encoding/json drops unknown keys without a word. Without this rule,
// `trial_period_days` instead of `trial_days` would create an active,
// charged subscription with a 201. A PATCH with a misspelled field would
// answer 200 and change nothing. The refusal is a 422, not a 400, because
// the idempotency middleware caches a 400. A corrected retry under the same
// Idempotency-Key would then get the refusal replayed.

// DecodeOption changes what DecodeStrict and DecodeBody accept.
type DecodeOption func(*decodeRule)

type decodeRule struct {
	emptyOK  bool
	readOnly []string
}

// EmptyBodyOK lets a route whose body is optional take no body at all, only
// whitespace, or `null`. v then keeps the values it had. A body that is
// present follows the rule.
func EmptyBodyOK(r *decodeRule) { r.emptyOK = true }

// ReadOnly refuses top-level keys that the request type declares but the
// server sets (id, tenant_id, created_at). It is for a route that decodes
// the type it returns. Without this option, those keys are accepted and
// ignored.
func ReadOnly(keys ...string) DecodeOption {
	return func(r *decodeRule) { r.readOnly = append(r.readOnly, keys...) }
}

// BodyError says why DecodeBody refused a body. Field is the refused key. It
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

// DecodeStrict decodes the request body into v under the rule above. On a
// refusal it writes the response and returns false. The handler must
// return on false.
func DecodeStrict(w http.ResponseWriter, r *http.Request, v any, opts ...DecodeOption) bool {
	if err := DecodeBody(r, v, opts...); err != nil {
		WriteBodyError(w, r, err)
		return false
	}
	return true
}

// NoBody is the rule for a route that takes no body. It lists no field, so
// a body with any field is a 422 naming that field. No body, `{}` and
// `null` are accepted. Call it before the handler does anything else. On a
// refusal it writes the response and returns false. The handler must
// return on false.
func NoBody(w http.ResponseWriter, r *http.Request) bool {
	return DecodeStrict(w, r, &struct{}{}, EmptyBodyOK)
}

// DecodeBody is DecodeStrict for a handler that answers some refusals its
// own way, such as the usage batch's 413. It returns a *BodyError. Pass any
// error the handler does not answer itself to WriteBodyError.
func DecodeBody(r *http.Request, v any, opts ...DecodeOption) error {
	var rule decodeRule
	for _, o := range opts {
		o(&rule)
	}
	return decodeBody(r, v, rule)
}

// WriteBodyError writes the response for a DecodeBody error: a 422 naming
// the field for a refused key, otherwise a 400 "invalid JSON body".
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

// DecodeValue applies the rule to one JSON value already read from a body,
// such as one event of the usage batch. A key that v does not declare with
// exactly that spelling and case is a *BodyError naming it.
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
	// {"Email": ...} sets email, but the reference lists one spelling.
	// sigs.k8s.io/json, Kubernetes' strict decoder, matches exactly. Here it
	// only checks the keys. It reads whole numbers in an `any` as int64, so
	// v keeps the encoding/json decode above.
	if key := inexactKey(raw, v); key != "" {
		return &BodyError{Field: key}
	}
	return nil
}

// inexactKey returns a key in raw, at any depth, that names a field of v's
// type only in another case. raw has already decoded into v, so every key
// matches a field in some case.
func inexactKey(raw json.RawMessage, v any) string {
	fresh := reflect.New(reflect.TypeOf(v).Elem()).Interface()
	strictErrs, err := kjson.UnmarshalStrict(raw, fresh, kjson.DisallowUnknownFields)
	if err != nil || len(strictErrs) == 0 {
		return ""
	}
	var fe kjson.FieldError
	if !errors.As(strictErrs[0], &fe) {
		return ""
	}
	// FieldPath looks like items[0].Name. Param is the key itself (Name), as
	// for an unknown key.
	path := fe.FieldPath()
	return path[strings.LastIndexAny(path, ".]")+1:]
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
