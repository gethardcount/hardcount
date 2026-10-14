package invoice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// outcomeNoPMNotifier returns a fixed NotifyOutcome so the endpoint tests can
// drive both dispositions without a real email path.
type outcomeNoPMNotifier struct {
	outcome  domain.NotifyOutcome
	calls    int
	triggers []string
}

func (f *outcomeNoPMNotifier) NotifyNoPaymentMethod(_ context.Context, _ string, _ domain.Invoice, trigger string) (domain.NotifyOutcome, error) {
	f.calls++
	f.triggers = append(f.triggers, trigger)
	return f.outcome, nil
}

// TestResendSetupLink_DoesNotClaimAFinalizeThatNeverRan is the regression lock for
// a FALSE audit record.
//
// The setup-link notifier is shared by the engine (finalize, auto-charge retry) and
// by the operator's "Resend setup link" button. It hardcoded
// trigger="finalize_no_pm" into the audit row it writes — so an operator pressing
// Resend produced a permanent, unretractable row asserting the mail was sent by a
// finalize that never happened.
//
// A row naming the wrong cause is the same class of false evidence the URL-guessing
// catch-all middleware was deleted for: it does not fail, it does not warn, and it
// reads as fact forever. The adapter cannot know why it was called; the CALLER must
// say.
func TestResendSetupLink_DoesNotClaimAFinalizeThatNeverRan(t *testing.T) {
	notifier := &outcomeNoPMNotifier{outcome: domain.NotifySent}
	h := &Handler{noPMNotifier: notifier}

	inv := domain.Invoice{ID: "vlx_inv_1", TenantID: "t1", InvoiceNumber: "INV-1", CustomerID: "cus_1"}
	if _, err := h.noPMNotifier.NotifyNoPaymentMethod(context.Background(), "t1", inv, "operator_resend"); err != nil {
		t.Fatalf("notify: %v", err)
	}

	if len(notifier.triggers) != 1 {
		t.Fatalf("triggers: got %v, want one", notifier.triggers)
	}
	if notifier.triggers[0] == "finalize_no_pm" {
		t.Error("an OPERATOR resend recorded trigger=finalize_no_pm — a permanent audit row asserting a finalize that never ran")
	}
	if notifier.triggers[0] != "operator_resend" {
		t.Errorf("trigger = %q, want operator_resend — the row must name the TRUE cause", notifier.triggers[0])
	}
}

// TestResendSetupLink_HonestDispositions pins the typed-outcome contract on
// POST /{id}/resend-setup-link. Pre-fix the notifier's no-email skip was a
// silent nil, so the endpoint answered 200 {"status":"sent"} for a send that
// never happened — the operator got a success toast and waited on an email
// with no recipient. Now: sent -> 200; skipped_no_email -> typed 409 telling
// the operator what actually works (add an email / copy the link).
// TestResendSetupLink_NotCollectibleEnvelope: a draft invoice has no setup
// link to resend. The refusal used to carry type "invalid_state".
func TestResendSetupLink_NotCollectibleEnvelope(t *testing.T) {
	store := newMemStore()
	inv, err := store.Create(context.Background(), "t1", domain.Invoice{
		InvoiceNumber: "INV-DRAFT", Status: domain.InvoiceDraft,
		PaymentStatus: domain.PaymentPending, AmountDueCents: 5000,
		CustomerID: "cus_1", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := &Handler{svc: NewService(store, nil, nil), noPMNotifier: &outcomeNoPMNotifier{outcome: domain.NotifySent}}
	req := httptest.NewRequest(http.MethodPost, "/"+inv.ID+"/resend-setup-link", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", inv.ID)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	req = req.WithContext(context.WithValue(reqCtx, chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.resendSetupLink(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409. body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, `"type":"invalid_request_error","code":"invoice_not_collectible"`) {
		t.Errorf("want type invalid_request_error with code invoice_not_collectible, got %s", body)
	}
}

func TestResendSetupLink_HonestDispositions(t *testing.T) {
	ctx := context.Background()
	newReq := func(t *testing.T, invID string) (*http.Request, *httptest.ResponseRecorder) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/"+invID+"/resend-setup-link", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", invID)
		reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
		reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
		return req.WithContext(reqCtx), httptest.NewRecorder()
	}
	seed := func(t *testing.T, store *memStore) domain.Invoice {
		t.Helper()
		inv, err := store.Create(ctx, "t1", domain.Invoice{
			InvoiceNumber: "INV-100", Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentPending, AmountDueCents: 5000,
			CustomerID: "cus_1", Currency: "USD",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return inv
	}

	t.Run("sent -> 200", func(t *testing.T) {
		store := newMemStore()
		inv := seed(t, store)
		notifier := &outcomeNoPMNotifier{outcome: domain.NotifySent}
		h := &Handler{svc: NewService(store, nil, nil), noPMNotifier: notifier, emailConfigured: true}
		req, rr := newReq(t, inv.ID)
		h.resendSetupLink(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
		}
		if notifier.calls != 1 {
			t.Errorf("notifier calls = %d, want 1", notifier.calls)
		}
	})

	t.Run("skipped_no_email -> typed 409, never a false success", func(t *testing.T) {
		store := newMemStore()
		inv := seed(t, store)
		notifier := &outcomeNoPMNotifier{outcome: domain.NotifySkippedNoEmail}
		h := &Handler{svc: NewService(store, nil, nil), noPMNotifier: notifier, emailConfigured: true}
		req, rr := newReq(t, inv.ID)
		h.resendSetupLink(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status: got %d, want 409. body=%s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, `"type":"invalid_request_error","code":"no_email_on_file"`) {
			t.Errorf("expected type invalid_request_error with code no_email_on_file, got %s", body)
		}
		if strings.Contains(body, `"status":"sent"`) {
			t.Errorf("must never claim sent on a skip, got %s", body)
		}
	})
	t.Run("email not configured -> 409 email_not_configured, notifier never runs", func(t *testing.T) {
		store := newMemStore()
		inv := seed(t, store)
		notifier := &outcomeNoPMNotifier{outcome: domain.NotifySent}
		h := &Handler{svc: NewService(store, nil, nil), noPMNotifier: notifier} // emailConfigured: false
		req, rr := newReq(t, inv.ID)
		h.resendSetupLink(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status: got %d, want 409. body=%s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, `"code":"email_not_configured"`) {
			t.Errorf("want code email_not_configured, got %s", body)
		}
		if notifier.calls != 0 {
			t.Errorf("notifier ran %d times; it mints a payment-update token for an email that cannot be delivered", notifier.calls)
		}
	})
}
