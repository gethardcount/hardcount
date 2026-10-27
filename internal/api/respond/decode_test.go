package respond

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestDecodeStrict(t *testing.T) {
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
		{name: "unknown top-level key", body: `{"code":"a","trial_period_days":14}`, wantStatus: http.StatusUnprocessableEntity, wantParam: "trial_period_days", wantMsg: `unknown field "trial_period_days"`},
		{name: "unknown nested key", body: `{"code":"a","items":[{"plan_id":"p","price":"x"}]}`, wantStatus: http.StatusUnprocessableEntity, wantParam: "price", wantMsg: `unknown field "price"`},
		{name: "malformed JSON", body: `{"code":`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "wrong type", body: `{"code":7}`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "empty body", body: ``, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "two values", body: `{"code":"a"} {"code":"b"}`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
		{name: "trailing brace", body: `{"code":"a"}}`, wantStatus: http.StatusBadRequest, wantMsg: "invalid JSON body"},
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

// A nil body (a server-side request built with no body) is empty.
func TestDecodeBody_NilBody(t *testing.T) {
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

	req = httptest.NewRequest("POST", "/", io.NopCloser(strings.NewReader(`{`)))
	if err := DecodeBody(req, &strictBody{}); err == nil || err.Error() != "invalid JSON body" {
		t.Errorf("err = %v, want invalid JSON body", err)
	}
}

func TestNoBody(t *testing.T) {
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
