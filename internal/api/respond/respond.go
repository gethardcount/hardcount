package respond

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/sagarsuperuser/velox/internal/version"
)

// apiVersion feeds the Velox-Version response header — the build-time
// stamp (git tag in released images, "dev" locally), not a hardcoded date
// that went stale within weeks.
var apiVersion = version.Version

// JSON writes a success response with standard headers.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	setHeaders(w, r)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Default Go HTML-escapes <, >, & to < etc. for safety when JSON
	// is embedded in HTML. API responses are served as application/json,
	// so the escaping is unnecessary and just makes errors harder to read
	// (e.g. "must be <= 604800"). Stripe / Vercel / GitHub all return
	// raw <, >, &.
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

// Error writes a Stripe-style error response.
// Format: {"error": {"type": "...", "message": "...", "code": "...", "param": "..."}}
func Error(w http.ResponseWriter, r *http.Request, status int, errType, code, message string) {
	errorField(w, r, status, errType, code, "", message)
}

// errorField is the underlying writer that includes the optional field name in
// the envelope's Param slot. All shortcut helpers funnel through here.
func errorField(w http.ResponseWriter, r *http.Request, status int, errType, code, field, message string) {
	setHeaders(w, r)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(ErrorBody{
		Error: ErrorDetail{
			Type:      errType,
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
func ErrorWith(w http.ResponseWriter, r *http.Request, status int, errType, code, message string, extra map[string]any) {
	body := make(map[string]any, len(extra)+1)
	for k, v := range extra {
		body[k] = v
	}
	body["error"] = ErrorDetail{Type: errType, Code: code, Message: message, RequestID: requestID(r)}
	setHeaders(w, r)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// Common error shortcuts

func BadRequest(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusBadRequest, "invalid_request_error", "invalid_request", message)
}

func NotFound(w http.ResponseWriter, r *http.Request, resource string) {
	Error(w, r, http.StatusNotFound, "invalid_request_error", "not_found", resource+" not found")
}

func Validation(w http.ResponseWriter, r *http.Request, message string) {
	errorField(w, r, http.StatusUnprocessableEntity, "invalid_request_error", "validation_error", "", message)
}

// ValidationField writes a 422 response with the offending field name in the
// envelope's Param slot. Use for inline handler validation where the field is
// known at the call site (service-layer validation is routed via FromError,
// which pulls the field off DomainError automatically).
func ValidationField(w http.ResponseWriter, r *http.Request, field, message string) {
	errorField(w, r, http.StatusUnprocessableEntity, "invalid_request_error", "validation_error", field, message)
}

// ValidationCoded writes a 422 response with a domain-specific error code
// in the envelope's Code slot, the offending field (optional) in Param, and
// the message. Use when the caller has a stable code (e.g. "coupon_expired")
// that API integrators will switch on.
//
// An empty code falls back to "validation_error" so the envelope is never
// missing a code.
func ValidationCoded(w http.ResponseWriter, r *http.Request, field, code, message string) {
	if code == "" {
		code = "validation_error"
	}
	errorField(w, r, http.StatusUnprocessableEntity, "invalid_request_error", code, field, message)
}

// ConflictCoded is the 409 analogue of ValidationCoded — same code+field
// plumbing, different status. Default code is "already_exists".
func ConflictCoded(w http.ResponseWriter, r *http.Request, field, code, message string) {
	if code == "" {
		code = "already_exists"
	}
	errorField(w, r, http.StatusConflict, "invalid_request_error", code, field, message)
}

// NotFoundCoded is the 404 analogue — message is built by the caller since
// domain-specific 404s often have more context than "<resource> not found".
func NotFoundCoded(w http.ResponseWriter, r *http.Request, code, message string) {
	if code == "" {
		code = "not_found"
	}
	Error(w, r, http.StatusNotFound, "invalid_request_error", code, message)
}

// PreconditionFailed writes a 412 response. The canonical use is optimistic
// concurrency: the caller sent If-Match with the ETag they last saw and a
// concurrent writer has since bumped the version. The client should GET the
// resource, re-apply its edits against the fresh copy, and retry.
func PreconditionFailed(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusPreconditionFailed, "invalid_request_error", "precondition_failed", message)
}

// RouteNotFound is the router's answer to a path it does not serve.
func RouteNotFound(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusNotFound, "invalid_request_error", "route_not_found",
		"unrecognized request URL ("+r.Method+" "+r.URL.Path+")")
}

// MethodNotAllowed is the router's answer to a method the path does not
// take. allow lists the methods it does take, for the Allow header.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request, allow []string) {
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
	}
	Error(w, r, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed",
		r.Method+" is not allowed on "+r.URL.Path)
}

// Timeout is the answer when a request runs past its time limit before
// anything was written.
func Timeout(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusGatewayTimeout, "api_error", "timeout", "the request ran past its time limit")
}

func InternalError(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusInternalServerError, "api_error", "internal_error", "an internal error occurred")
}

func Unauthorized(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusUnauthorized, "authentication_error", "unauthorized", message)
}

func Forbidden(w http.ResponseWriter, r *http.Request, message string) {
	Error(w, r, http.StatusForbidden, "authentication_error", "forbidden", message)
}

func RateLimited(w http.ResponseWriter, r *http.Request) {
	Error(w, r, http.StatusTooManyRequests, "rate_limit_error", "rate_limited", "too many requests — please retry after the rate limit resets")
}

// Types

type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Param     string `json:"param,omitempty"`
}

func setHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Velox-Version", apiVersion)
	if id := requestID(r); id != "" {
		w.Header().Set("Velox-Request-Id", id)
	}
}

func requestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	return chimw.GetReqID(r.Context())
}
