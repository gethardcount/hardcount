package hostedinvoice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// noBaseHandler is a handler that could take payment (Stripe wired, keys for
// the mode) on a server with no HOSTED_INVOICE_BASE_URL.
func noBaseHandler(t *testing.T, token string) (*Handler, *fakeCheckout, *fakeReady) {
	t.Helper()
	fi := newFakeInvoices()
	fc := newFakeCustomers()
	fs := newFakeSettings()
	seedFinalized(t, fi, fc, fs, token)
	stripe := &fakeCheckout{}
	ready := &fakeReady{ready: true}
	h := New(Deps{
		Invoices: fi, Customers: fc, Settings: fs, CreditNotes: fakeCreditNotes{},
		Stripe: stripe, StripeReady: ready, BaseURL: "",
	})
	return h, stripe, ready
}

// With no hosted invoice address, Stripe would have no page to send the
// customer back to. The page offers no Pay button and says why, with the
// same reason as a seller without Stripe keys. It used to offer Pay and send
// the paying customer to http://localhost:5173.
func TestViewInvoice_NoBaseURL_PayDisabledWithReason(t *testing.T) {
	h, _, ready := noBaseHandler(t, "hc_pinv_nobase")
	resp := viewPayload(t, h, "hc_pinv_nobase")
	if resp.PayEnabled {
		t.Error("pay_enabled = true with no HOSTED_INVOICE_BASE_URL")
	}
	if resp.PayUnavailableReason != PayUnavailableOnline {
		t.Errorf("pay_unavailable_reason = %q, want %q", resp.PayUnavailableReason, PayUnavailableOnline)
	}
	if ready.calls != 0 {
		t.Errorf("Stripe readiness was read %d times; the missing address decides first", ready.calls)
	}
}

// A direct POST gets the same 409, and Stripe is never asked for a session.
func TestCheckout_NoBaseURL_Conflict(t *testing.T) {
	h, stripe, _ := noBaseHandler(t, "hc_pinv_post_nobase")
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hc_pinv_post_nobase/checkout", nil))
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
		t.Errorf("Stripe was asked for a session that returns to %q", stripe.lastSuccess)
	}
}

// With the address set, Stripe returns the customer to the hosted page on
// that address, and nowhere else.
func TestCheckout_BaseURL_ReturnsToHostedPage(t *testing.T) {
	h, stripe, _ := readyHandler(t, "hc_pinv_base", true, nil)
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hc_pinv_base/checkout", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200, body=%s", w.Code, w.Body.String())
	}
	const base = "https://app.hardcount.dev/invoice/hc_pinv_base"
	if stripe.lastSuccess != base+"?paid=1" || stripe.lastCancel != base {
		t.Errorf("success=%q cancel=%q, want %q and %q", stripe.lastSuccess, stripe.lastCancel, base+"?paid=1", base)
	}
}
