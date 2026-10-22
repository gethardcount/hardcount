package respond

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type strictItem struct {
	PlanID string `json:"plan_id"`
}

type strictBody struct {
	Code  string       `json:"code"`
	Items []strictItem `json:"items"`
}

func TestDecodeStrict(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOK     bool
		wantStatus int
		wantParam  string
	}{
		{"known keys", `{"code":"a","items":[{"plan_id":"p"}]}`, true, 0, ""},
		{"unknown top-level key", `{"code":"a","trial_period_days":14}`, false, http.StatusUnprocessableEntity, "trial_period_days"},
		{"unknown nested key", `{"code":"a","items":[{"plan_id":"p","price":"x"}]}`, false, http.StatusUnprocessableEntity, "price"},
		{"malformed JSON", `{"code":`, false, http.StatusBadRequest, ""},
		{"wrong type", `{"code":7}`, false, http.StatusBadRequest, ""},
		{"empty body", ``, false, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			var v strictBody
			ok := DecodeStrict(rec, req, &v)
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
			body := decodeError(t, rec)
			if body.Param != tc.wantParam {
				t.Errorf("param: got %q, want %q", body.Param, tc.wantParam)
			}
			if tc.wantParam != "" && body.Message != `unknown field "`+tc.wantParam+`"` {
				t.Errorf("message: got %q", body.Message)
			}
			if tc.wantStatus == http.StatusBadRequest && body.Message != "invalid JSON body" {
				t.Errorf("message: got %q, want invalid JSON body", body.Message)
			}
		})
	}
}
