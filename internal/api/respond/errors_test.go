package respond

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/errs"
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

func TestFromError_DuplicateKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	FromError(rec, req, fmt.Errorf("%w: idempotency_key", errs.ErrDuplicateKey), "usage_event")

	if rec.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", rec.Code)
	}
}

func TestFromError_ValidationMessage(t *testing.T) {
	cases := []string{
		"customer_id is required",
		"quantity must be positive",
		"can only activate draft subscriptions",
		"cannot void a paid invoice",
		"invalid billing_interval",
	}

	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", nil)

			FromError(rec, req, fmt.Errorf("%s", msg), "resource")

			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("'%s' → status: got %d, want 422", msg, rec.Code)
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

// The message-text fallback is the one branch that answers from an untyped
// error's text, so each use must leave a log line naming the route.
func TestFromError_TextFallbackLogsWarning(t *testing.T) {
	buf := captureLogs(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/things/tok_123", nil)

	router := chi.NewRouter()
	router.Post("/v1/things/{id}", func(w http.ResponseWriter, r *http.Request) {
		FromError(w, r, fmt.Errorf("quantity must be positive"), "thing")
	})
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("want exactly one JSON log line, got %q: %v", buf.String(), err)
	}
	if entry["level"] != "WARN" {
		t.Errorf("level: got %v, want WARN", entry["level"])
	}
	for k, want := range map[string]string{
		"error":    "quantity must be positive",
		"resource": "thing",
		"method":   "POST",
		"route":    "/v1/things/{id}",
	} {
		if entry[k] != want {
			t.Errorf("log attr %s: got %v, want %q", k, entry[k], want)
		}
	}
	// The path can carry a token on public routes; only the pattern is logged.
	if strings.Contains(buf.String(), "tok_123") {
		t.Errorf("log line carries the raw path: %s", buf.String())
	}
}

// A typed validation error does not go through the fallback and logs nothing.
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

func TestFromError_DomainError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", nil)

	err := errs.New("billing_setup_incomplete", "stripe connection required")
	FromError(rec, req, err, "billing")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d, want 422", rec.Code)
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
		raw:  "Keys for idempotent requests can only be used with the same parameters they were first used with. Try using a key other than 'velox_inv_xxx'",
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
	if strings.Contains(body, "velox_inv_") {
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
