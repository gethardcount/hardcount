// Package respond writes the API's JSON responses: success bodies, list
// pages and the error envelope. FromError turns a service error into its
// HTTP response. DecodeStrict reads a request body under one strict rule,
// and ParsePage reads the paging parameters.
package respond

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/version"
)

// apiVersion feeds the Hardcount-Version response header. It is the
// build-time stamp (the git tag in released images, "dev" locally), not a
// hardcoded date, which would go stale within weeks.
var apiVersion = version.Version

// JSON writes a success response with standard headers.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	setHeaders(w)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// By default Go escapes <, > and & (as \u003c and so on), for safety
	// when JSON is embedded in HTML. API responses are served as
	// application/json, so the escaping is not needed and only makes errors
	// harder to read (for example "must be <= 604800"). Stripe, Vercel and
	// GitHub all return <, > and & unescaped.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(data)
}

// List writes one page of a list: {"data": [...], "total": n}. total counts
// every row that matches the request's filters, not only this page. A nil
// slice is written as [], never null. Read the page with ParsePage.
func List(w http.ResponseWriter, r *http.Request, data any, total int) {
	if v := reflect.ValueOf(data); data == nil || (v.Kind() == reflect.Slice && v.IsNil()) {
		data = []any{}
	}
	JSON(w, r, http.StatusOK, map[string]any{
		"data":  data,
		"total": total,
	})
}

// Error writes a Stripe-style error response:
// {"error": {"type": "...", "code": "...", "message": "...", "request_id": "..."}}.
// The type follows from the status (errorType).
func Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	errorField(w, r, status, code, "", message)
}

// errorField is the underlying writer, which also puts the optional field
// name in the envelope's Param slot. Every shortcut helper goes through it.
func errorField(w http.ResponseWriter, r *http.Request, status int, code, field, message string) {
	setHeaders(w)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(ErrorBody{
		Error: ErrorDetail{
			Type:      errorType(status),
			Code:      code,
			Message:   message,
			Param:     field,
			RequestID: requestID(r),
		},
	})
}

// ErrorWith writes the error envelope plus top-level fields beside "error".
// Use it only when the error carries data a caller needs, such as the usage
// batch's per-event errors. extra must not hold the key "error".
func ErrorWith(w http.ResponseWriter, r *http.Request, status int, code, message string, extra map[string]any) {
	body := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		body[k] = v
	}
	body["error"] = ErrorDetail{Type: errorType(status), Code: code, Message: message, RequestID: requestID(r)}
	setHeaders(w)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// Common error shortcuts

// BadRequest writes a 400 with code invalid_request and the message.
func BadRequest(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusBadRequest, "invalid_request", message)
}

// NotFound writes a 404 with code not_found that names the resource.
func NotFound(w http.ResponseWriter, r *http.Request, resource string) {
	Error(w, r, http.StatusNotFound, "not_found", resource+" not found")
}

// Validation writes a 422 with code validation_error and no field.
func Validation(w http.ResponseWriter, r *http.Request, message string) {
	errorField(w, r, http.StatusUnprocessableEntity, "validation_error", "", message)
}

// ValidationField writes a 422 with the offending field name in the
// envelope's Param slot. Use it for inline handler validation, where the
// field is known at the call site. Service-layer validation goes through
// FromError, which takes the field from the DomainError.
func ValidationField(w http.ResponseWriter, r *http.Request, field, message string) {
	errorField(w, r, http.StatusUnprocessableEntity, "validation_error", field, message)
}

// ValidationCoded writes a 422 with a domain-specific error code in the
// envelope's Code slot, the offending field (optional) in Param, and the
// message. Use it when the caller has a stable code, such as
// "invoice_number_taken", that API integrators will switch on.
//
// An empty code falls back to "validation_error", so the envelope always
// has a code.
func ValidationCoded(w http.ResponseWriter, r *http.Request, field, code, message string) {
	if code == "" {
		code = "validation_error"
	}
	errorField(w, r, http.StatusUnprocessableEntity, code, field, message)
}

// ConflictCoded is the 409 counterpart of ValidationCoded: the same code and
// field handling, with a different status. The default code is
// "already_exists".
func ConflictCoded(w http.ResponseWriter, r *http.Request, field, code, message string) {
	if code == "" {
		code = "already_exists"
	}
	errorField(w, r, http.StatusConflict, code, field, message)
}

// NotFoundCoded is the 404 counterpart. The caller builds the message,
// because a domain-specific 404 often has more context than "<resource> not
// found".
func NotFoundCoded(w http.ResponseWriter, r *http.Request, code, message string) {
	if code == "" {
		code = "not_found"
	}
	Error(w, r, http.StatusNotFound, code, message)
}

// RouteNotFound is the router's answer to a path it does not serve.
func RouteNotFound(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusNotFound, "route_not_found",
		"unrecognized request URL ("+r.Method+" "+r.URL.Path+")")
}

// MethodNotAllowed is the router's answer to a method the path does not
// take. allow lists the methods it does take, for the Allow header.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request, allow []string) {
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
	}
	Error(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
		r.Method+" is not allowed on "+r.URL.Path)
}

// Timeout is the answer when a request runs past its time limit before
// anything was written.
func Timeout(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusGatewayTimeout, "timeout", "the request ran past its time limit")
}

// InternalError writes a 500 with code internal_error and a generic
// message.
func InternalError(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusInternalServerError, "internal_error", "an internal error occurred")
}

// Unauthorized writes a 401 authentication_error with code unauthorized.
func Unauthorized(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusUnauthorized, "unauthorized", message)
}

// Forbidden writes a 403 authentication_error with code forbidden.
func Forbidden(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusForbidden, "forbidden", message)
}

// RateLimited writes a 429 rate_limit_error with code rate_limited.
func RateLimited(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests — please retry after the rate limit resets")
}

// Types

// ErrorBody is the error envelope: {"error": {...}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the error object inside ErrorBody. Param names the field
// at fault and is left out when empty.
type ErrorDetail struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Param     string `json:"param,omitempty"`
}

// errorType is the envelope's type for a status. A client switches on type
// and code, so one status always has one type.
func errorType(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= http.StatusInternalServerError:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// setHeaders sets the headers every answer from this package carries. The
// Hardcount-Request-Id header is the RequestID middleware's, set before any
// handler runs.
func setHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Hardcount-Version", apiVersion)
}

func requestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	return chimw.GetReqID(r.Context())
}
