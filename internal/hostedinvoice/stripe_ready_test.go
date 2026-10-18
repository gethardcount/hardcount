package hostedinvoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type fakeReady struct {
	ready bool
	calls int
}

func (f *fakeReady) HasFor(_ context.Context, _ string, _ bool) bool {
	f.calls++
	return f.ready
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
