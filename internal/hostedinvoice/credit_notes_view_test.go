package hostedinvoice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

type listedCreditNotes struct {
	notes []domain.CreditNote
	err   error
}

func (l listedCreditNotes) ListIssued(_ context.Context, _, _ string) ([]domain.CreditNote, error) {
	return l.notes, l.err
}

func viewWithNotes(t *testing.T, lister CreditNoteLister) *httptest.ResponseRecorder {
	t.Helper()
	fi, fc, fs := newFakeInvoices(), newFakeCustomers(), newFakeSettings()
	token := "hc_pinv_cn_view"
	seedFinalized(t, fi, fc, fs, token)
	h := New(Deps{Invoices: fi, Customers: fc, Settings: fs, CreditNotes: lister, Stripe: &fakeCheckout{}, BaseURL: "https://app.hardcount.dev"})
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+token, nil))
	return w
}

// The hosted page lists the invoice's credit notes as the PDF prints them
// (M-04): one before payment lowered what is owed; one after payment shows
// what came back, once a part of it has settled.
func TestViewInvoice_CreditNotesAsThePDFPrintsThem(t *testing.T) {
	w := viewWithNotes(t, listedCreditNotes{notes: []domain.CreditNote{
		{CreditNoteNumber: "CN-1", TotalCents: 1500},
		{CreditNoteNumber: "CN-2", TotalCents: 2000, CreditAmountCents: 2000},
		{CreditNoteNumber: "CN-3", TotalCents: 700, RefundAmountCents: 700, RefundStatus: domain.RefundPending},
		{CreditNoteNumber: "CN-4", TotalCents: 900, RefundAmountCents: 600, RefundStatus: domain.RefundSucceeded, OutOfBandAmountCents: 300},
	}})
	if w.Code != http.StatusOK {
		t.Fatalf("view: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		CreditNotes []map[string]any `json:"credit_notes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]map[string]any{}
	for _, cn := range resp.CreditNotes {
		got[cn["number"].(string)] = cn
	}
	if len(got) != 3 {
		t.Fatalf("want CN-1, CN-2 and CN-4 (CN-3's refund has not settled), got %v", resp.CreditNotes)
	}
	if cn := got["CN-1"]; cn["before_payment"] != true || cn["amount_cents"] != float64(1500) {
		t.Errorf("CN-1 = %v, want before_payment with 1500", cn)
	}
	if cn := got["CN-2"]; cn["before_payment"] != false || cn["credited_cents"] != float64(2000) {
		t.Errorf("CN-2 = %v, want 2000 credited after payment", cn)
	}
	if cn := got["CN-4"]; cn["refunded_cents"] != float64(600) || cn["settled_outside_cents"] != float64(300) {
		t.Errorf("CN-4 = %v, want 600 refunded and 300 settled outside", cn)
	}
	if _, ok := got["CN-2"]["refunded_cents"]; ok {
		t.Errorf("CN-2 has no refund: refunded_cents must be absent, got %v", got["CN-2"])
	}
}

// No credit notes: an empty list, never a missing key.
func TestViewInvoice_NoCreditNotesIsAnEmptyList(t *testing.T) {
	w := viewWithNotes(t, listedCreditNotes{})
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(resp["credit_notes"]) != "[]" {
		t.Fatalf("credit_notes = %s, want []", resp["credit_notes"])
	}
}

// A failed read is a 500: a page without its credit notes would show totals
// that don't add up.
func TestViewInvoice_CreditNoteReadFailureIs500(t *testing.T) {
	w := viewWithNotes(t, listedCreditNotes{err: errors.New("connection reset")})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("view with a failed credit note read: %d, want 500", w.Code)
	}
}
