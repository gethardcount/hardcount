package respond

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type strictItem struct {
	PlanID string `json:"plan_id"`
}

type strictBody struct {
	ID    string       `json:"id"`
	Code  string       `json:"code"`
	Items []strictItem `json:"items"`
}

func TestDecodeStrict_TheRule(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		opts       []DecodeOption
		wantOK     bool
		wantStatus int
		wantParam  string
		wantMsg    string
		wantCode   string // the decoded code on success
	}{
		{name: "known keys", body: `{"code":"a","items":[{"plan_id":"p"}]}`, wantOK: true, wantCode: "a"},
		{name: "unknown top-level key", body: `{"code":"a","billing_tme":"calendar"}`, wantStatus: http.StatusUnprocessableEntity, wantParam: "billing_tme", wantMsg: `unknown field "billing_tme"`},
		{name: "unknown nested key", body: `{"code":"a","items":[{"plan_id":"p","price":"x"}]}`, wantStatus: http.StatusUnprocessableEntity, wantParam: "price", wantMsg: `unknown field "price"`},
		{name: "malformed JSON", body: `{"code":`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "wrong type", body: `{"code":7}`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "empty body", body: ``, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "two values", body: `{"code":"a"} {"code":"b"}`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "trailing brace", body: `{"code":"a"}}`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "key in another case", body: `{"Code":"a"}`, wantStatus: http.StatusUnprocessableEntity, wantParam: "Code", wantMsg: `unknown field "Code"`},
		{name: "nested key in another case", body: `{"code":"a","items":[{"PLAN_ID":"p"}]}`, wantStatus: http.StatusUnprocessableEntity, wantParam: "PLAN_ID", wantMsg: `unknown field "PLAN_ID"`},
		{name: "trailing whitespace", body: "{\"code\":\"a\"}\n  ", wantOK: true, wantCode: "a"},

		{name: "optional: empty body", body: ``, opts: []DecodeOption{EmptyBodyOK}, wantOK: true, wantCode: "keep"},
		{name: "optional: whitespace", body: " \n", opts: []DecodeOption{EmptyBodyOK}, wantOK: true, wantCode: "keep"},
		{name: "optional: null", body: `null`, opts: []DecodeOption{EmptyBodyOK}, wantOK: true, wantCode: "keep"},
		{name: "optional: a body follows the rule", body: `{"cod":"a"}`, opts: []DecodeOption{EmptyBodyOK}, wantStatus: http.StatusUnprocessableEntity, wantParam: "cod", wantMsg: `unknown field "cod"`},
		{name: "optional: malformed", body: `{`, opts: []DecodeOption{EmptyBodyOK}, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},

		{name: "read-only key", body: `{"id":"x","code":"a"}`, opts: []DecodeOption{ReadOnly("id")}, wantStatus: http.StatusUnprocessableEntity, wantParam: "id", wantMsg: "id is set by the server and cannot be sent"},
		{name: "read-only key in another case", body: `{"ID":"x"}`, opts: []DecodeOption{ReadOnly("id")}, wantStatus: http.StatusUnprocessableEntity, wantParam: "ID", wantMsg: "ID is set by the server and cannot be sent"},
		{name: "read-only key absent", body: `{"code":"a"}`, opts: []DecodeOption{ReadOnly("id")}, wantOK: true, wantCode: "a"},
		{name: "read-only and a non-object body", body: `[1]`, opts: []DecodeOption{ReadOnly("id")}, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			v := strictBody{Code: "keep"}
			ok := DecodeStrict(rec, req, &v, tc.opts...)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v (body %s)", ok, tc.wantOK, rec.Body.String())
			}
			if tc.wantOK {
				if rec.Body.Len() != 0 {
					t.Fatalf("success wrote a response: %s", rec.Body.String())
				}
				if v.Code != tc.wantCode {
					t.Errorf("code: got %q, want %q", v.Code, tc.wantCode)
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
			body := decodeError(t, rec)
			if body.Param != tc.wantParam {
				t.Errorf("param: got %q, want %q", body.Param, tc.wantParam)
			}
			if body.Message != tc.wantMsg {
				t.Errorf("message: got %q, want %q", body.Message, tc.wantMsg)
			}
		})
	}
}

type edgeBody struct {
	Code  string       `json:"code"`
	Days  int          `json:"days"`
	Items []strictItem `json:"items"`
	Any   any          `json:"any"`
}

// TestDecodeStrict_EdgesOfJSON pins the answer an API caller gets where a
// JSON decoder makes its own choices: duplicate keys, invalid UTF-8, which
// of two faults it reports, keys that differ from a field's name only by
// case folding or an underscore, keys a JSON Pointer must escape, and
// numbers that do not fit. A refusal is its status, error code and param. An
// accepted body is the value it decodes to.
func TestDecodeStrict_EdgesOfJSON(t *testing.T) {
	const bad, unknown = "invalid_request", "validation_error"
	cases := []struct {
		name       string
		body       string
		wantStatus int // 0: accepted
		wantCode   string
		wantParam  string
		want       edgeBody
	}{
		{name: "duplicate key: the last value wins", body: `{"code":"a","code":"b"}`, want: edgeBody{Code: "b"}},
		{name: "duplicate nested key: the last value wins", body: `{"items":[{"plan_id":"p","plan_id":"q"}]}`, want: edgeBody{Code: "keep", Items: []strictItem{{PlanID: "q"}}}},
		{name: "duplicate key inside an any value", body: `{"any":{"x":1,"x":2}}`, want: edgeBody{Code: "keep", Any: map[string]any{"x": 2.0}}},
		{name: "invalid UTF-8 in a value becomes U+FFFD", body: "{\"code\":\"a\xffb\"}", want: edgeBody{Code: "a�b"}},
		{name: "a lone surrogate escape becomes U+FFFD", body: `{"code":"\ud800"}`, want: edgeBody{Code: "�"}},
		{name: "invalid UTF-8 in a key", body: "{\"co\xffde\":\"a\"}", wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: "co�de"},
		{name: "null leaves the field as it was", body: `{"code":null}`, want: edgeBody{Code: "keep"}},

		{name: "a wrong type before an unknown key", body: `{"code":7,"bogus":1}`, wantStatus: http.StatusBadRequest, wantCode: bad},
		{name: "an unknown key before a wrong type", body: `{"bogus":1,"code":7}`, wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: "bogus"},
		{name: "a wrong type in a nested object", body: `{"items":[{"plan_id":7}]}`, wantStatus: http.StatusBadRequest, wantCode: bad},

		{name: "a key that folds to a field name", body: `{"itemſ":[]}`, wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: "itemſ"},
		{name: "a key without its underscore", body: `{"items":[{"planid":"p"}]}`, wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: "planid"},
		{name: "a key with a dash for its underscore", body: `{"items":[{"plan-id":"p"}]}`, wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: "plan-id"},
		{name: "an unknown key with a slash", body: `{"a/b":1}`, wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: "a/b"},
		{name: "an unknown nested key with a tilde", body: `{"items":[{"a~b":1}]}`, wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: "a~b"},
		{name: "an unknown key with an escaped quote", body: `{"a\"b":1}`, wantStatus: http.StatusUnprocessableEntity, wantCode: unknown, wantParam: `a"b`},
		{name: "an empty key", body: `{"":1}`, wantStatus: http.StatusBadRequest, wantCode: bad},

		{name: "a whole number too large", body: `{"days":99999999999999999999}`, wantStatus: http.StatusBadRequest, wantCode: bad},
		{name: "a fraction for a whole number", body: `{"days":3.0}`, wantStatus: http.StatusBadRequest, wantCode: bad},
		{name: "an exponent for a whole number", body: `{"days":1e2}`, wantStatus: http.StatusBadRequest, wantCode: bad},
		{name: "a number too large for any", body: `{"any":1e400}`, wantStatus: http.StatusBadRequest, wantCode: bad},
		{name: "a leading zero", body: `{"days":01}`, wantStatus: http.StatusBadRequest, wantCode: bad},

		{name: "a byte order mark", body: "\xEF\xBB\xBF{\"code\":\"a\"}", wantStatus: http.StatusBadRequest, wantCode: bad},
		{name: "a control character in a string", body: "{\"code\":\"a\x01b\"}", wantStatus: http.StatusBadRequest, wantCode: bad},
		{name: "a trailing comma", body: `{"code":"a",}`, wantStatus: http.StatusBadRequest, wantCode: bad},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			v := edgeBody{Code: "keep"}
			ok := DecodeStrict(rec, req, &v)
			if tc.wantStatus == 0 {
				if !ok {
					t.Fatalf("refused with %d %s, want accepted", rec.Code, rec.Body.String())
				}
				if !reflect.DeepEqual(v, tc.want) {
					t.Errorf("decoded %+v, want %+v", v, tc.want)
				}
				return
			}
			if ok {
				t.Fatalf("accepted as %+v, want %d", v, tc.wantStatus)
			}
			body := decodeError(t, rec)
			if rec.Code != tc.wantStatus || body.Code != tc.wantCode || body.Param != tc.wantParam {
				t.Errorf("answer %d code %q param %q, want %d code %q param %q", rec.Code, body.Code, body.Param, tc.wantStatus, tc.wantCode, tc.wantParam)
			}
		})
	}
}

type exactInner struct {
	Name string `json:"name"`
}

type exactEmbedded struct {
	Region string `json:"region"`
}

type exactBody struct {
	exactEmbedded
	Plain   string                `json:"plain"`
	Untag   string                // encoding/json names it Untag
	Skipped string                `json:"-"`
	Ptr     *exactInner           `json:"ptr"`
	ByKey   map[string]exactInner `json:"by_key"`
	Raw     json.RawMessage       `json:"raw"`
	Any     any                   `json:"any"`
	Dims    map[string]any        `json:"dims"`
}

// A key matches its field exactly. encoding/json alone matches without
// regard to case, so {"Email": ...} set email although the reference lists
// only email. Keys inside a value the type decodes as it likes (raw JSON,
// any, a map's keys) are not field names and are not checked.
func TestDecodeBody_KeysMatchExactly(t *testing.T) {
	cases := []struct {
		name, body, wantField string
	}{
		{"every name exact", `{"region":"eu","plain":"p","Untag":"u","ptr":{"name":"n"},"by_key":{"K":{"name":"n"}},"raw":{"Any":1},"any":{"Any":1},"dims":{"Model":"x"}}`, ""},
		{"promoted field in another case", `{"Region":"eu"}`, "Region"},
		{"untagged field in another case", `{"untag":"u"}`, "untag"},
		{"through a pointer", `{"ptr":{"NAME":"n"}}`, "NAME"},
		{"in a map value", `{"by_key":{"k":{"Name":"n"}}}`, "Name"},
		{"skipped field", `{"Skipped":"s"}`, "Skipped"},
		{"null nested value", `{"ptr":null,"by_key":null}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v exactBody
			err := DecodeBody(httptest.NewRequest("POST", "/", strings.NewReader(tc.body)), &v)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var be *BodyError
			if !errors.As(err, &be) || be.Field != tc.wantField {
				t.Fatalf("err = %v, want a BodyError naming %q", err, tc.wantField)
			}
		})
	}
}

// A nil body (a server-side request built with no body) is empty.
func TestDecodeBody_NilBodyIsEmpty(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.Body = nil
	var v strictBody
	if err := DecodeBody(req, &v, EmptyBodyOK); err != nil {
		t.Errorf("optional: %v, want nil", err)
	}
	var be *BodyError
	if err := DecodeBody(req, &v); !errors.As(err, &be) || be.Field != "" {
		t.Errorf("required: %v, want a BodyError with no field", err)
	}
}

// DecodeBody keeps the cause, so a handler can answer a read error such as
// the router's byte cap its own way.
func TestDecodeBody_KeepsTheCause(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`[{"code":"aaaaaaaaaaaaaaaa"}]`))
	req.Body = http.MaxBytesReader(rec, req.Body, 8)
	var v []strictBody
	err := DecodeBody(req, &v)
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("err = %v, want the MaxBytesError in the chain", err)
	}

	// A whole value that ends inside the cap, followed by bytes past it,
	// keeps the cap error too.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"code":"a"}`+strings.Repeat(" ", 64)))
	req.Body = http.MaxBytesReader(rec, req.Body, 32)
	if err := DecodeBody(req, &strictBody{}); !errors.As(err, &maxErr) {
		t.Fatalf("trailing bytes past the cap: err = %v, want the MaxBytesError in the chain", err)
	}

	// Real data after the value, inside the cap, is still invalid JSON.
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"code":"a"} {"code":"b"}`))
	if err := DecodeBody(req, &strictBody{}); err == nil || err.Error() != "invalid JSON body" || errors.As(err, &maxErr) {
		t.Errorf("trailing data: err = %v, want invalid JSON body", err)
	}

	req = httptest.NewRequest("POST", "/", io.NopCloser(strings.NewReader(`{`)))
	if err := DecodeBody(req, &strictBody{}); err == nil || err.Error() != "invalid JSON body" {
		t.Errorf("err = %v, want invalid JSON body", err)
	}
}

func TestNoBody_TakesNothingButAnEmptyBody(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOK     bool
		wantStatus int
		wantParam  string
	}{
		{name: "no body", body: ``, wantOK: true},
		{name: "empty object", body: `{}`, wantOK: true},
		{name: "null", body: `null`, wantOK: true},
		{name: "a field", body: `{"at_period_end":true}`, wantStatus: http.StatusUnprocessableEntity, wantParam: "at_period_end"},
		{name: "an array", body: `[]`, wantStatus: http.StatusBadRequest},
		{name: "malformed", body: `{`, wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ok := NoBody(rec, httptest.NewRequest("POST", "/", strings.NewReader(tc.body)))
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v (body %s)", ok, tc.wantOK, rec.Body.String())
			}
			if tc.wantOK {
				if rec.Body.Len() != 0 {
					t.Fatalf("success wrote a response: %s", rec.Body.String())
				}
				return
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
			if p := decodeError(t, rec).Param; p != tc.wantParam {
				t.Errorf("param: got %q, want %q", p, tc.wantParam)
			}
		})
	}
}

// A read that stopped at the router's cap is a 413 naming the cap, also when
// the cap error is wrapped. Any other error is left to the caller, with
// nothing written.
func TestBodyTooLarge_Answers413NamingTheCap(t *testing.T) {
	capErr := &http.MaxBytesError{Limit: 1 << 20}
	for _, c := range []struct {
		name    string
		err     error
		wantMsg string
	}{
		{"the cap error", capErr, "The request body is larger than the 1 MB limit. Send a smaller body."},
		{"wrapped", fmt.Errorf("read: %w", capErr), "The request body is larger than the 1 MB limit. Send a smaller body."},
		{"in a BodyError", &BodyError{Err: capErr}, "The request body is larger than the 1 MB limit. Send a smaller body."},
		{"a cap in bytes", &http.MaxBytesError{Limit: 1000}, "The request body is larger than the 1000 bytes limit. Send a smaller body."},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !BodyTooLarge(rec, httptest.NewRequest("POST", "/", nil), c.err) {
				t.Fatal("BodyTooLarge = false, want true")
			}
			var body ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rec.Body, err)
			}
			if rec.Code != http.StatusRequestEntityTooLarge || body.Error.Type != "invalid_request_error" ||
				body.Error.Code != "payload_too_large" || body.Error.Message != c.wantMsg {
				t.Errorf("answer %d %+v, want 413 invalid_request_error payload_too_large %q", rec.Code, body.Error, c.wantMsg)
			}
		})
	}

	rec := httptest.NewRecorder()
	if BodyTooLarge(rec, httptest.NewRequest("POST", "/", nil), io.ErrUnexpectedEOF) {
		t.Error("BodyTooLarge(another read error) = true, want false")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("BodyTooLarge wrote %q for another read error, want nothing", rec.Body)
	}
}

// DecodeStrict answers a body over the cap with the 413, and a body that is
// not JSON with the 400 it always had.
func TestDecodeStrict_BodyOverTheCapIs413(t *testing.T) {
	for _, c := range []struct {
		name     string
		body     string
		wantCode int
	}{
		{"over the cap", `{"code":"aaaaaaaaaaaaaaaa"}`, http.StatusRequestEntityTooLarge},
		{"not JSON", `{"co`, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
			req.Body = http.MaxBytesReader(rec, req.Body, 8)
			if DecodeStrict(rec, req, &strictBody{}) {
				t.Fatal("DecodeStrict = true, want a refusal")
			}
			if rec.Code != c.wantCode {
				t.Errorf("status %d %s, want %d", rec.Code, rec.Body, c.wantCode)
			}
		})
	}
}
