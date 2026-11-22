package respond

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/version"
)

// An error's type follows from its status, for every status the API answers
// with. docs/api/errors.md lists the same pairs.
func TestError_TypeFollowsTheStatus(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusBadRequest:            "invalid_request_error",
		http.StatusUnauthorized:          "authentication_error",
		http.StatusForbidden:             "authentication_error",
		http.StatusNotFound:              "invalid_request_error",
		http.StatusMethodNotAllowed:      "invalid_request_error",
		http.StatusConflict:              "invalid_request_error",
		http.StatusRequestEntityTooLarge: "invalid_request_error",
		http.StatusUnprocessableEntity:   "invalid_request_error",
		http.StatusTooManyRequests:       "rate_limit_error",
		http.StatusInternalServerError:   "api_error",
		http.StatusBadGateway:            "api_error",
		http.StatusServiceUnavailable:    "api_error",
		http.StatusGatewayTimeout:        "api_error",
	} {
		rec := httptest.NewRecorder()
		Error(rec, httptest.NewRequest(http.MethodGet, "/", nil), status, "some_code", "a message")
		var body ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%d: decode %s: %v", status, rec.Body, err)
		}
		if rec.Code != status || body.Error.Type != want {
			t.Errorf("Error(%d): answered %d type %q, want %d type %q", status, rec.Code, body.Error.Type, status, want)
		}
	}
}

// An answer is application/json with the build's Hardcount-Version, and
// keeps <, > and & as they are rather than as \u003c and the like.
func TestJSON_HeadersAndUnescapedHTMLCharacters(t *testing.T) {
	rec := httptest.NewRecorder()
	ValidationField(rec, httptest.NewRequest(http.MethodPost, "/", nil), "ttl", "must be <= 604800 & > 0")

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Hardcount-Version"); got != version.Version {
		t.Errorf("Hardcount-Version = %q, want %q", got, version.Version)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"must be <= 604800 & > 0"`) {
		t.Errorf("body = %s, want the message with <, > and & unescaped", body)
	}
}

// A nil slice is written as [], so data is always an array.
func TestList_NilIsEmptyArray(t *testing.T) {
	rec := httptest.NewRecorder()
	var none []string
	List(rec, httptest.NewRequest(http.MethodGet, "/", nil), none, 0)
	if got := rec.Body.String(); got != "{\"data\":[],\"total\":0}\n" {
		t.Errorf("body = %q, want data [] and total 0", got)
	}
}
