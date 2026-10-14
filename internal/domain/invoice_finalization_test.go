package domain

import (
	"testing"
	"time"
)

// TestInvoiceFinalizationStatus locks in the single-source-of-truth
// rule shared across all four invoice-emitting paths
// (engine.billOnePeriod, engine.BillOnCreate,
// engine.BillFinalOnImmediateCancel, subscription.handleItemProration).
// Pre-fix BillOnCreate + BillFinalOnImmediateCancel + handleItemProration
// hardcoded InvoiceFinalized regardless of tax_status, producing
// finalized invoices with TaxAmountCents=0 when Stripe Tax returned
// customer_data_invalid — lying about authoritative amounts.
func TestInvoiceFinalizationStatus(t *testing.T) {
	pause := &PauseCollection{Behavior: PauseCollectionKeepAsDraft}
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	timed := func(resumesAt time.Time) *PauseCollection {
		return &PauseCollection{Behavior: PauseCollectionKeepAsDraft, ResumesAt: &resumesAt}
	}

	cases := []struct {
		name      string
		taxStatus InvoiceTaxStatus
		pause     *PauseCollection
		want      InvoiceStatus
	}{
		{"happy path (tax ok, no pause)", InvoiceTaxOK, nil, InvoiceFinalized},
		{"tax pending → draft", InvoiceTaxPending, nil, InvoiceDraft},
		{"tax failed → finalized (worker exhausted, operator-resolved)", InvoiceTaxFailed, nil, InvoiceFinalized},
		{"pause set + tax ok → draft", InvoiceTaxOK, pause, InvoiceDraft},
		{"pause set + tax pending → draft (both gates fire)", InvoiceTaxPending, pause, InvoiceDraft},
		// A close can run after resumes_at; it reads the pause as it stood
		// at the instant it bills to.
		{"resumes after the instant → draft", InvoiceTaxOK, timed(at.Add(30 * time.Minute)), InvoiceDraft},
		{"resumed exactly at the instant → finalized", InvoiceTaxOK, timed(at), InvoiceFinalized},
		{"resumed before the instant → finalized", InvoiceTaxOK, timed(at.Add(-time.Hour)), InvoiceFinalized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InvoiceFinalizationStatus(tc.taxStatus, tc.pause, at)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
