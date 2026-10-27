package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/testutil/pdftest"
)

// The invoice PDF is a customer document, so it prints every issued credit
// note on the invoice. It used to read the newest 100 notes of any status (the
// Activity timeline's capped list) and keep the issued ones, so past 100
// notes, or with drafts among the newest, rows went missing and the printed
// rows stopped adding up to Amount Due (found in #1404's review).
//
// Over the real server and Postgres: 101 issued notes, the most a store page
// returns plus one, then 3 drafts as the newest notes. Both the dashboard's
// PDF and the customer's hosted-page PDF must print all 101 issued notes and
// no draft.
func TestE2E_InvoicePDF_PrintsEveryIssuedCreditNote(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", true) // first invoice: 4000c, unpaid
	invID := r.firstInvoiceID()

	note := func(i int, issue bool) string {
		at := lagP0.Add(time.Hour + time.Duration(i)*time.Second)
		cn := r.must(http.MethodPost, "/v1/credit-notes", at, map[string]any{
			"invoice_id": invID, "reason": "Goodwill", "auto_issue": issue,
			"lines": []map[string]any{{"description": "Goodwill", "quantity": 1, "unit_amount_cents": 10}},
		}, http.StatusCreated)
		num := flowStr(cn, "credit_note_number")
		if num == "" {
			t.Fatalf("credit note %d: no number in %v", i, cn)
		}
		return num
	}
	const issuedCount = 101
	issued := make([]string, 0, issuedCount)
	for i := range issuedCount {
		issued = append(issued, note(i, true))
	}
	var drafts []string
	for i := range 3 {
		drafts = append(drafts, note(issuedCount+i, false))
	}

	inv := r.invoice(invID)
	if got := flowInt(inv, "amount_due_cents"); got != 4000-issuedCount*10 {
		t.Fatalf("amount due = %dc, want %dc (4000c less the issued notes)", got, 4000-issuedCount*10)
	}

	code, dashboard := r.do(http.MethodGet, "/v1/invoices/"+invID+"/pdf", lagP0.Add(2*time.Hour), nil)
	if code != http.StatusOK {
		t.Fatalf("dashboard PDF: status %d: %s", code, dashboard)
	}
	wantCreditNoteRows(t, "dashboard PDF", dashboard, issued, drafts)

	token := flowStr(inv, "public_token")
	if token == "" {
		t.Fatalf("invoice has no public_token: %v", inv)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/public/invoices/"+token+"/pdf", nil)
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hosted PDF: status %d: %s", rec.Code, rec.Body.String())
	}
	wantCreditNoteRows(t, "hosted PDF", rec.Body.Bytes(), issued, drafts)
}

// wantCreditNoteRows checks that the PDF prints a row for each issued note
// and none for a draft. A row's label is "<number> - <reason>".
func wantCreditNoteRows(t *testing.T, what string, pdfBytes []byte, issued, drafts []string) {
	t.Helper()
	pages := pdftest.Read(t, pdfBytes)
	printed := func(num string) bool {
		for _, p := range pages {
			if p.Has(num + " - Goodwill") {
				return true
			}
		}
		return false
	}
	var missing []string
	for _, num := range issued {
		if !printed(num) {
			missing = append(missing, num)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s: %d of %d issued credit notes are not printed, e.g. %v", what, len(missing), len(issued), missing[:min(3, len(missing))])
	}
	for _, num := range drafts {
		if printed(num) {
			t.Errorf("%s: draft credit note %s is printed", what, num)
		}
	}
	if t.Failed() {
		t.Logf("%s: %d pages", what, len(pages))
	}
}
