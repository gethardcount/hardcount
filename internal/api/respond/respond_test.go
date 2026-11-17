package respond

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
