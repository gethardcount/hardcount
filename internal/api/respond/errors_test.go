package respond

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
)

// Each kind answers the status, type, code, message and param in FromError's
// table. A DomainError's Code replaces the default code, except on a 403.
func TestFromError_EachKindAnswersItsStatusCodeAndParam(t *testing.T) {
	cases := []struct {
		name                      string
		err                       error
		status                    int
		typ, code, message, param string
	}{
		{"bare not-found", errs.ErrNotFound, 404, "invalid_request_error", "not_found", "customer not found", ""},
		{"wrapped already-exists", fmt.Errorf("%w: external_id dup", errs.ErrAlreadyExists), 409, "invalid_request_error", "already_exists", "already exists: external_id dup", ""},
		{"AlreadyExists", errs.AlreadyExists("code", `plan code "acme-pro" already exists`), 409, "invalid_request_error", "already_exists", `plan code "acme-pro" already exists`, "code"},
		{"InvalidState", errs.InvalidState("cannot rotate a revoked key"), 409, "invalid_request_error", "invalid_state", "cannot rotate a revoked key", ""},
		{"InvalidState with a code", errs.InvalidState("clock is advancing").WithCode("clock_advancing"), 409, "invalid_request_error", "clock_advancing", "clock is advancing", ""},
		{"Forbidden", errs.Forbidden("this key cannot do that"), 403, "authentication_error", "forbidden", "this key cannot do that", ""},
		{"Forbidden with a code", errs.Forbidden("this key cannot do that").WithCode("other"), 403, "authentication_error", "forbidden", "this key cannot do that", ""},
		{"Required", errs.Required("external_id"), 422, "invalid_request_error", "validation_error", "external_id is required", "external_id"},
		{"Invalid", errs.Invalid("company_email", "must contain @ and a domain"), 422, "invalid_request_error", "validation_error", "must contain @ and a domain", "company_email"},
		{"Invalid with a code", errs.Invalid("url", "must start with https://").WithCode("url_not_https"), 422, "invalid_request_error", "url_not_https", "must start with https://", "url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			FromError(rec, httptest.NewRequest("POST", "/", nil), tc.err, "customer")

			if rec.Code != tc.status {
				t.Errorf("status: got %d, want %d", rec.Code, tc.status)
			}
			got := decodeError(t, rec)
			if got.Type != tc.typ || got.Code != tc.code || got.Message != tc.message || got.Param != tc.param {
				t.Errorf("body: got type %q code %q message %q param %q, want %q %q %q %q",
					got.Type, got.Code, got.Message, got.Param, tc.typ, tc.code, tc.message, tc.param)
			}
		})
	}
}

// An error with no kind is a server fault, even when its text reads like a
// validation message: 500 internal_error, a generic body, and the error in the
// log at ERROR.
func TestFromError_UntypedErrorIsServerFault(t *testing.T) {
	cases := []string{
		"amount must be at least 50",
		"customer_id is required",
		"cannot void a paid invoice",
		"invalid billing_interval",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			buf := captureLogs(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", nil)

			FromError(rec, req, fmt.Errorf("%s", msg), "resource")

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status: got %d, want 500", rec.Code)
			}
			if body := decodeError(t, rec); body.Code != "internal_error" || strings.Contains(body.Message, msg) {
				t.Errorf("body: got code %q message %q, want internal_error and a generic message", body.Code, body.Message)
			}
			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("want exactly one JSON log line, got %q: %v", buf.String(), err)
			}
			if entry["level"] != "ERROR" || entry["msg"] != "unhandled error" || entry["error"] != msg {
				t.Errorf("logged %v %q error=%v, want ERROR \"unhandled error\" error=%q", entry["level"], entry["msg"], entry["error"], msg)
			}
		})
	}
}

// captureLogs points the default slog logger at a buffer for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// safeCause is a SafeMessageError that wraps its cause, as a payment error
// wraps the Stripe client's error.
type safeCause struct{ cause error }

func (e *safeCause) Error() string               { return "stripe: " + e.cause.Error() }
func (e *safeCause) Unwrap() error               { return e.cause }
func (e *safeCause) OperatorSafeMessage() string { return "the payment provider did not answer" }

// TestFromError_ClientGoneLogsAtWarn: a handler error from a request whose
// client went away, such as a closed browser tab, is logged at WARN with the
// same fields. The same error on a live request, and the 30s route timeout,
// stay at ERROR, the level operators alert on.
// Mutation-verify: log at slog.LevelError in FromError → the "client went
// away" rows log at ERROR.
func TestFromError_ClientGoneLogsAtWarn(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(stop)
	canceled := fmt.Errorf("list invoices: %w", context.Canceled)

	cases := []struct {
		name    string
		ctx     context.Context
		err     error
		wantMsg string
		want    string
	}{
		{"client went away", gone, canceled, "unhandled error", "WARN"},
		{"same error, request still live", context.Background(), canceled, "unhandled error", "ERROR"},
		{"route timeout", timedOut, fmt.Errorf("list invoices: %w", context.DeadlineExceeded), "unhandled error", "ERROR"},
		{"provider call, client went away", gone, &safeCause{context.Canceled}, "safe-message error returned", "WARN"},
		{"provider call, request still live", context.Background(), &safeCause{context.Canceled}, "safe-message error returned", "ERROR"},
		{"unavailable refusal, client went away", gone, errs.Unavailable("proration_unavailable", "retry", canceled), "request refused as unavailable; the client may send it again", "WARN"},
		{"unavailable refusal, request still live", context.Background(), errs.Unavailable("proration_unavailable", "retry", canceled), "request refused as unavailable; the client may send it again", "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			req := httptest.NewRequest("GET", "/v1/invoices", nil).WithContext(tc.ctx)
			FromError(httptest.NewRecorder(), req, tc.err, "invoice")

			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("want exactly one JSON log line, got %q: %v", buf.String(), err)
			}
			if entry["level"] != tc.want || entry["msg"] != tc.wantMsg {
				t.Errorf("logged %v %q, want %s %q", entry["level"], entry["msg"], tc.want, tc.wantMsg)
			}
			if entry["error"] != tc.err.Error() || entry["resource"] != "invoice" {
				t.Errorf("fields error=%v resource=%v, want %q and invoice", entry["error"], entry["resource"], tc.err.Error())
			}
		})
	}
}

// A typed validation error is the client's mistake, so it logs nothing.
func TestFromError_ValidationErrorLogsNothing(t *testing.T) {
	buf := captureLogs(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/things", nil)

	FromError(rec, req, errs.Required("quantity"), "thing")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("typed validation error logged: %s", buf.String())
	}
}

// A DomainError with a code and no kind has no status of its own, so it is a
// server fault: its code and message stay in the log.
func TestFromError_CodeWithoutKindIsServerFault(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	err := &errs.DomainError{Code: "billing_setup_incomplete", Message: "stripe connection required"}
	FromError(rec, req, err, "billing")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "stripe connection required") {
		t.Errorf("the body carries the error's message: %s", rec.Body.String())
	}
	if body := decodeError(t, rec); body.Code != "internal_error" {
		t.Errorf("code: got %q, want internal_error", body.Code)
	}
}

// A wrapped NotFound answers with its own message. The prefixes its callers
// added name internal steps and ids, and stay in the log.
func TestFromError_WrappedNotFoundAnswersItsOwnMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)

	err := fmt.Errorf("get current plan sub_123: %w", errs.NotFound("plan"))
	FromError(rec, req, err, "subscription")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	if body := decodeError(t, rec); body.Code != "not_found" || body.Message != "plan not found" {
		t.Errorf("body: got code %q message %q, want not_found and %q", body.Code, body.Message, "plan not found")
	}
}

// An errs.Unavailable refusal answers 503 with Retry-After: 1, its own code
// and message, and never its cause. A not-found in the cause does not turn it
// into a 404. The cause goes to the log at ERROR.
func TestFromError_UnavailableAnswers503WithRetryAfterAndItsOwnMessage(t *testing.T) {
	const message = "could not read this period's invoice — please retry"
	causes := map[string]error{
		"database error":     fmt.Errorf("connection reset by peer"),
		"not-found in cause": fmt.Errorf("connection reset by peer: %w", errs.ErrNotFound),
	}
	for name, cause := range causes {
		t.Run(name, func(t *testing.T) {
			buf := captureLogs(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", nil)

			err := fmt.Errorf("proration in atomic addItem tx: %w",
				errs.Unavailable("proration_unavailable", message, cause))
			FromError(rec, req, err, "subscription")

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status: got %d, want 503", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != "1" {
				t.Errorf("Retry-After: got %q, want 1", got)
			}
			body := decodeError(t, rec)
			if body.Type != "api_error" || body.Code != "proration_unavailable" || body.Message != message {
				t.Errorf("body: got %+v, want api_error proration_unavailable %q", body, message)
			}
			if !strings.Contains(buf.String(), "connection reset by peer") || !strings.Contains(buf.String(), `"level":"ERROR"`) {
				t.Errorf("want the cause logged at ERROR, got %s", buf.String())
			}
		})
	}
}

// fakeSafeMessageError is a SafeMessageError whose Error() is raw text that
// must not reach the client.
type fakeSafeMessageError struct{ raw, safe string }

func (e *fakeSafeMessageError) Error() string               { return e.raw }
func (e *fakeSafeMessageError) OperatorSafeMessage() string { return e.safe }

// A SafeMessageError answers 502 with its safe message and none of its raw
// text, bare or wrapped with %w as the charge path wraps its payment error.
func TestFromError_SafeMessageErrorAnswers502WithItsSafeMessage(t *testing.T) {
	raw := "Keys for idempotent requests can only be used with the same parameters they were first used with. Try using a key other than 'hardcount_inv_xxx'"
	safe := &fakeSafeMessageError{raw: raw, safe: "Card was declined: insufficient funds."}
	for name, err := range map[string]error{
		"bare":    safe,
		"wrapped": fmt.Errorf("payment failed: %w", safe),
	} {
		rec := httptest.NewRecorder()
		FromError(rec, httptest.NewRequest("POST", "/", nil), err, "payment")

		body := rec.Body.String()
		if rec.Code != http.StatusBadGateway || !strings.Contains(body, "Card was declined") {
			t.Errorf("%s: got %d %s, want 502 with the safe message", name, rec.Code, body)
		}
		if strings.Contains(body, "Keys for idempotent requests") || strings.Contains(body, "hardcount_inv_") {
			t.Errorf("%s: the raw provider text reached the client: %s", name, body)
		}
	}
}

// codedSafeMessageError also names its own code.
type codedSafeMessageError struct {
	fakeSafeMessageError
	code string
}

func (e *codedSafeMessageError) OperatorErrorCode() string { return e.code }

// An unknown payment outcome must not share provider_error with a refusal:
// the dashboard says "Couldn't charge" for provider_error, which is false
// when the card may have been charged.
func TestFromError_SafeMessageErrorCodeReplacesProviderError(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{"payment_outcome_unknown", `"code":"payment_outcome_unknown"`},
		{"", `"code":"provider_error"`},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		err := fmt.Errorf("payment failed: %w", &codedSafeMessageError{
			fakeSafeMessageError: fakeSafeMessageError{raw: "raw", safe: "safe"},
			code:                 tc.code,
		})
		FromError(rec, req, err, "payment")
		if rec.Code != http.StatusBadGateway {
			t.Errorf("code %q: status got %d, want 502", tc.code, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("code %q: body %s, want %s", tc.code, rec.Body.String(), tc.want)
		}
	}
}

func TestFromError_NilWritesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)

	FromError(rec, req, nil, "resource")

	if rec.Code != http.StatusOK {
		t.Errorf("nil error should not write response, got %d", rec.Code)
	}
}

// A DomainError wrapped with %w keeps its field as the param.
func TestFromError_WrappedDomainErrorKeepsItsField(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	inner := errs.Required("currency")
	wrapped := fmt.Errorf("create customer: %w", inner)
	FromError(rec, req, wrapped, "customer")

	body := decodeError(t, rec)
	if body.Param != "currency" {
		t.Errorf("param: got %q, want currency (field should survive wrapping)", body.Param)
	}
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) ErrorDetail {
	t.Helper()
	var body ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body.Error
}
