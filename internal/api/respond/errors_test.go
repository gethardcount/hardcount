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

func TestFromError_NotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)

	FromError(rec, req, errs.ErrNotFound, "customer")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Code != "not_found" {
		t.Errorf("code: got %q", body.Code)
	}
}

func TestFromError_AlreadyExists(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	FromError(rec, req, fmt.Errorf("%w: external_id dup", errs.ErrAlreadyExists), "customer")

	if rec.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", rec.Code)
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
func TestFromError_TypedValidationDoesNotLog(t *testing.T) {
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

func TestFromError_UnknownError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)

	FromError(rec, req, fmt.Errorf("database connection timed out"), "resource")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	// ADR-026: raw error message MUST NOT leak to the response body.
	// The full error is logged server-side; the wire response is
	// generic.
	if got := rec.Body.String(); strings.Contains(got, "database connection") {
		t.Errorf("raw error leaked to wire body: %s", got)
	}
}

// An errs.Unavailable refusal answers 503 with Retry-After: 1, its own code
// and message, and never its cause. A not-found in the cause does not turn it
// into a 404. The cause goes to the log at ERROR.
func TestFromError_Unavailable(t *testing.T) {
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

// fakeSafeMessageError is a test double that opts into operator-safe
// rendering via the SafeMessageError marker interface.
type fakeSafeMessageError struct{ raw, safe string }

func (e *fakeSafeMessageError) Error() string               { return e.raw }
func (e *fakeSafeMessageError) OperatorSafeMessage() string { return e.safe }

func TestFromError_SafeMessageError_SurfacedNotRaw(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	// Simulate the leak path: a Stripe SDK error wrapped in our
	// PaymentError-shaped marker. Raw message is leaky; safe message
	// is curated.
	err := &fakeSafeMessageError{
		raw:  "Keys for idempotent requests can only be used with the same parameters they were first used with. Try using a key other than 'hardcount_inv_xxx'",
		safe: "Card was declined: insufficient funds.",
	}
	FromError(rec, req, err, "payment")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	// The curated message is what hits the wire.
	if !strings.Contains(body, "Card was declined") {
		t.Errorf("expected curated message in response, got %s", body)
	}
	// The raw Stripe SDK string MUST NOT leak.
	if strings.Contains(body, "Keys for idempotent requests") {
		t.Errorf("raw Stripe SDK error leaked to wire: %s", body)
	}
	if strings.Contains(body, "hardcount_inv_") {
		t.Errorf("internal idempotency-key leaked to wire: %s", body)
	}
}

// TestFromError_SafeMessageWrapped_DetectedViaErrorsAs asserts that
// errors wrapped via fmt.Errorf with %w still trigger SafeMessageError
// detection. This is what the real ChargeInvoice path does:
// `fmt.Errorf("payment failed: %w", paymentErr)` — the wrapper
// must not hide the marker interface from errors.As.
func TestFromError_SafeMessageWrapped_DetectedViaErrorsAs(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	inner := &fakeSafeMessageError{
		raw:  "internal sdk error verbatim",
		safe: "Payment provider rejected the request. Please retry.",
	}
	wrapped := fmt.Errorf("payment failed: %w", inner)
	FromError(rec, req, wrapped, "payment")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Payment provider rejected") {
		t.Errorf("safe message lost through wrapper: %s", body)
	}
	if strings.Contains(body, "internal sdk error verbatim") {
		t.Errorf("raw sdk error leaked: %s", body)
	}
}

// codedSafeMessageError opts into a specific wire code as well.
type codedSafeMessageError struct {
	fakeSafeMessageError
	code string
}

func (e *codedSafeMessageError) OperatorErrorCode() string { return e.code }

// An unknown payment outcome must not share provider_error with a refusal:
// the dashboard says "Couldn't charge" for provider_error, which is false
// when the card may have been charged.
func TestFromError_SafeMessageError_OwnCodeWinsOverProviderError(t *testing.T) {
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

func TestFromError_Nil(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)

	FromError(rec, req, nil, "resource")

	if rec.Code != http.StatusOK {
		t.Errorf("nil error should not write response, got %d", rec.Code)
	}
}

func TestFromError_RequiredCarriesField(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	FromError(rec, req, errs.Required("external_id"), "customer")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d, want 422", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Param != "external_id" {
		t.Errorf("param: got %q, want external_id", body.Param)
	}
	if body.Message != "external_id is required" {
		t.Errorf("message: got %q", body.Message)
	}
}

func TestFromError_InvalidCarriesField(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	FromError(rec, req, errs.Invalid("company_email", "must contain @ and a domain"), "settings")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d, want 422", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Param != "company_email" {
		t.Errorf("param: got %q, want company_email", body.Param)
	}
	if body.Message != "must contain @ and a domain" {
		t.Errorf("message: got %q", body.Message)
	}
}

func TestFromError_AlreadyExistsCarriesField(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	FromError(rec, req, errs.AlreadyExists("code", "plan code \"acme-pro\" already exists"), "plan")

	if rec.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Param != "code" {
		t.Errorf("param: got %q, want code", body.Param)
	}
}

func TestFromError_WrappedFieldErrorPreservesField(t *testing.T) {
	// Handlers sometimes wrap service errors with context (fmt.Errorf("%w: ...")).
	// Field extraction walks the chain via errors.As, so the field survives.
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

type errorResponse struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var body struct {
		Error errorResponse `json:"error"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	return body.Error
}
