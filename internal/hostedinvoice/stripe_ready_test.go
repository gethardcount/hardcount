package hostedinvoice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type fakeReady struct {
	ready bool
	calls int
}

func (f *fakeReady) HasFor(_ context.Context, _ string, _ bool) (bool, error) {
	f.calls++
	return f.ready, nil
}

func readyHandler(t *testing.T, token string, ready bool, mutate func(*domain.Invoice)) (*Handler, *fakeCheckout, *fakeInvoices) {
	t.Helper()
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	inv := seedFinalized(t, fi, fc, fs, token)
	if mutate != nil {
		mutate(&inv)
		fi.byToken[token] = inv
	}
	stripe := &fakeCheckout{}
	h := New(Deps{
		Invoices: fi, Customers: fc, Settings: fs, CreditNotes: fakeCreditNotes{},
		Stripe: stripe, StripeReady: &fakeReady{ready: ready}, BaseURL: "https://app.velox.dev",
	})
	return h, stripe, fi
}

func viewPayload(t *testing.T, h *Handler, token string) hostedInvoicePayload {
	t.Helper()
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+token, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("view: got %d, body=%s", w.Code, w.Body.String())
	}
	var resp hostedInvoicePayload
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// An owed invoice from a seller without Stripe keys has no Pay button and
// says why; the customer used to press Pay and read "failed to create
// checkout session".
func TestViewInvoice_NoStripeKeys_PayDisabledWithReason(t *testing.T) {
	h, _, _ := readyHandler(t, "vlx_pinv_nokeys", false, nil)
	resp := viewPayload(t, h, "vlx_pinv_nokeys")
	if resp.PayEnabled {
		t.Error("pay_enabled = true without Stripe keys")
	}
	if resp.PayUnavailableReason != PayUnavailableOnline {
		t.Errorf("pay_unavailable_reason = %q, want %q", resp.PayUnavailableReason, PayUnavailableOnline)
	}
}

func TestViewInvoice_StripeKeys_PayEnabled(t *testing.T) {
	h, _, _ := readyHandler(t, "vlx_pinv_keys", true, nil)
	resp := viewPayload(t, h, "vlx_pinv_keys")
	if !resp.PayEnabled || resp.PayUnavailableReason != "" {
		t.Errorf("pay_enabled=%v reason=%q, want true and empty", resp.PayEnabled, resp.PayUnavailableReason)
	}
}

// Nothing owed (paid): no reason, the page shows its Paid state.
func TestViewInvoice_Paid_NoReason(t *testing.T) {
	h, _, _ := readyHandler(t, "vlx_pinv_paid_nokeys", false, func(inv *domain.Invoice) {
		inv.Status = domain.InvoicePaid
		inv.AmountDueCents = 0
	})
	resp := viewPayload(t, h, "vlx_pinv_paid_nokeys")
	if resp.PayEnabled || resp.PayUnavailableReason != "" {
		t.Errorf("pay_enabled=%v reason=%q, want false and empty", resp.PayEnabled, resp.PayUnavailableReason)
	}
}

// A direct POST without keys is a 409 the page can explain, and Stripe is
// never called.
func TestCheckout_NoStripeKeys_Conflict(t *testing.T) {
	h, stripe, _ := readyHandler(t, "vlx_pinv_post_nokeys", false, nil)
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/vlx_pinv_post_nokeys/checkout", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != PayUnavailableOnline {
		t.Errorf("code = %q, want %q", body.Error.Code, PayUnavailableOnline)
	}
	if stripe.lastTenant != "" {
		t.Error("Stripe was called without keys")
	}
}

// Stripe refused this amount as below its minimum charge: the page has no
// Pay button and says why, and a direct POST is a 409 the page can explain,
// without asking Stripe for a session it would refuse.
func TestHostedInvoice_BelowMinimum_NoPay(t *testing.T) {
	refused := func(inv *domain.Invoice) {
		at := inv.CreatedAt
		inv.AmountDueCents = 30
		inv.BelowMinimumAt = &at
	}
	h, stripe, _ := readyHandler(t, "vlx_pinv_min", true, refused)
	resp := viewPayload(t, h, "vlx_pinv_min")
	if resp.PayEnabled || resp.PayUnavailableReason != PayUnavailableBelowMinimum {
		t.Errorf("pay_enabled=%v reason=%q, want false and %q", resp.PayEnabled, resp.PayUnavailableReason, PayUnavailableBelowMinimum)
	}

	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/vlx_pinv_min/checkout", nil))
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusConflict || body.Error.Code != PayUnavailableBelowMinimum {
		t.Errorf("POST checkout: %d %q, want 409 %q; body=%s", w.Code, body.Error.Code, PayUnavailableBelowMinimum, w.Body.String())
	}
	if stripe.lastTenant != "" {
		t.Error("Stripe was asked for a session it would refuse")
	}
}

// Stripe was disconnected after the readiness check: the session create
// answers stripe_not_connected. The customer gets the same 409 as the check
// gives, not the operator's code or its "Connect it under Settings" sentence.
func TestCheckout_DisconnectedDuringCreate_Conflict(t *testing.T) {
	h, stripe, _ := readyHandler(t, "vlx_pinv_race_nokeys", true, nil)
	stripe.err = fmt.Errorf("ensure stripe customer: %w", errs.StripeNotConnected())
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/vlx_pinv_race_nokeys/checkout", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != PayUnavailableOnline {
		t.Errorf("code = %q, want %q", body.Error.Code, PayUnavailableOnline)
	}
	if body.Error.Message != "online payment is not set up for this invoice" {
		t.Errorf("message = %q, want the readiness check's sentence", body.Error.Message)
	}
	if stripe.lastTenant == "" {
		t.Error("the session create was not reached")
	}
}
