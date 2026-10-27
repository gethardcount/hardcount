package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// An unknown route and a method the route does not take answer the JSON
// error envelope every other error uses, not chi's plain-text defaults.
func TestRouter_UnknownRouteAndWrongMethodAnswerTheEnvelope(t *testing.T) {
	srv := newTestServer()
	cases := []struct {
		method, path string
		status       int
		code, allow  string
	}{
		{"GET", "/no-such-route", 404, "route_not_found", ""},
		{"GET", "/no/such/route", 404, "route_not_found", ""},
		{"POST", "/health", 405, "method_not_allowed", "GET"},
		{"GET", "/v1/auth/login", 405, "method_not_allowed", "POST"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.status {
			t.Errorf("%s %s = %d, want %d; body %q", c.method, c.path, rec.Code, c.status, rec.Body.String())
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s Content-Type = %q, want application/json", c.method, c.path, ct)
		}
		var body struct {
			Error struct {
				Type, Code, Message string
				RequestID           string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s body is not JSON: %q", c.method, c.path, rec.Body.String())
			continue
		}
		e := body.Error
		if e.Type != "invalid_request_error" || e.Code != c.code || e.Message == "" || e.RequestID == "" {
			t.Errorf("%s %s error = %+v, want type invalid_request_error, code %s, a message and a request_id", c.method, c.path, e, c.code)
		}
		if got := rec.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s Allow = %q, want %q", c.method, c.path, got, c.allow)
		}
	}
}
