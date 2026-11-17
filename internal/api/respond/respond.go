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

// JSON writes data as the body with status. Every answer of this package goes
// through it, so each carries the same headers and encoding.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Hardcount-Version", version.Version)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// The body is never embedded in HTML, and \u003c in place of < makes a
	// message such as "must be <= 604800" hard to read.
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

// Error writes the error envelope, {"error": {...}}, with no param. The
// envelope's type follows from status.
func Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeError(w, r, status, code, "", message)
}

// writeError writes the error envelope; field, when set, is its param.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, field, message string) {
	JSON(w, r, status, ErrorBody{Error: errorDetail(r, status, code, field, message)})
}

// ErrorWith writes the error envelope with extra top-level fields beside
// "error", for an error that carries data, such as the usage batch's
// per-event errors. extra must not hold the key "error".
func ErrorWith(w http.ResponseWriter, r *http.Request, status int, code, message string, extra map[string]any) {
	body := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		body[k] = v
	}
	body["error"] = errorDetail(r, status, code, "", message)
	JSON(w, r, status, body)
}

// BadRequest writes a 400 with code invalid_request and the message.
func BadRequest(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusBadRequest, "invalid_request", message)
}

// NotFound writes a 404 with code not_found that names the resource.
func NotFound(w http.ResponseWriter, r *http.Request, resource string) {
	Error(w, r, http.StatusNotFound, "not_found", resource+" not found")
}

// Validation writes a 422 validation_error with no param.
func Validation(w http.ResponseWriter, r *http.Request, message string) {
	ValidationField(w, r, "", message)
}

// ValidationField writes a 422 validation_error whose param names field. A
// handler uses it for a check it makes itself; a service returns
// errs.Invalid, which FromError answers the same way.
func ValidationField(w http.ResponseWriter, r *http.Request, field, message string) {
	writeError(w, r, http.StatusUnprocessableEntity, "validation_error", field, message)
}

// ValidationCoded writes a 422 with code, or validation_error when code is
// empty, and field as its param.
func ValidationCoded(w http.ResponseWriter, r *http.Request, field, code, message string) {
	if code == "" {
		code = "validation_error"
	}
	writeError(w, r, http.StatusUnprocessableEntity, code, field, message)
}

// ConflictCoded writes a 409 with code, or already_exists when code is
// empty, and field as its param.
func ConflictCoded(w http.ResponseWriter, r *http.Request, field, code, message string) {
	if code == "" {
		code = "already_exists"
	}
	writeError(w, r, http.StatusConflict, code, field, message)
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

func errorDetail(r *http.Request, status int, code, field, message string) ErrorDetail {
	return ErrorDetail{Type: errorType(status), Code: code, Message: message, Param: field, RequestID: requestID(r)}
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

// requestID is the ID the RequestID middleware gave the request, which it
// also sends as the Hardcount-Request-Id header.
func requestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	return chimw.GetReqID(r.Context())
}
