package creditnote

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// rowStore is a StripeRefundStore with one refund row (or none). Only the
// row read linkUnderLockTx makes is implemented.
type rowStore struct {
	StripeRefundStore
	row   *domain.InvoiceStripeRefund
	reads int
}

func (r *rowStore) LockStripeRefundTx(context.Context, *sql.Tx, string) (domain.InvoiceStripeRefund, bool, error) {
	r.reads++
	if r.row == nil {
		return domain.InvoiceStripeRefund{}, false, nil
	}
	return *r.row, true, nil
}

// TestLinkUnderLockTx: under the credit-note lock, a refund a note on the
// invoice already holds is refused whether or not Hardcount has a row for it
// (a refund whose events never arrived has none), and a row allows the link
// only while it is flagged or dismissed, on this invoice.
func TestLinkUnderLockTx(t *testing.T) {
	link := &linkedStripeRefund{facts: domain.StripeRefundFacts{ID: "re_1"}}
	row := func(o domain.StripeRefundOutcome, invoiceID string) *domain.InvoiceStripeRefund {
		return &domain.InvoiceStripeRefund{ID: "hc_srf_1", InvoiceID: invoiceID, StripeRefundID: "re_1", Outcome: o}
	}
	held := []domain.CreditNote{{CreditNoteNumber: "CN-1", StripeRefundID: "re_1"}}
	cases := []struct {
		name     string
		row      *domain.InvoiceStripeRefund
		existing []domain.CreditNote
		wantRow  bool
		wantErr  error
	}{
		{name: "no row, no note: link without a row"},
		{name: "no row, a note holds it", existing: held, wantErr: errs.ErrInvalidState},
		{name: "flagged row", row: row(domain.StripeRefundNeedsReview, "inv_1"), wantRow: true},
		{name: "dismissed row", row: row(domain.StripeRefundDismissed, "inv_1"), wantRow: true},
		{name: "flagged row, but a note holds it", row: row(domain.StripeRefundNeedsReview, "inv_1"), existing: held, wantErr: errs.ErrInvalidState},
		{name: "awaiting row", row: row(domain.StripeRefundAwaiting, "inv_1"), wantErr: errs.ErrInvalidState},
		{name: "recorded row", row: row(domain.StripeRefundRecordedAsCreditNote, "inv_1"), wantErr: errs.ErrInvalidState},
		{name: "another payment", row: row(domain.StripeRefundOtherPayment, "inv_1"), wantErr: errs.ErrValidation},
		{name: "moved no money", row: row(domain.StripeRefundNotRefunded, "inv_1"), wantErr: errs.ErrValidation},
		{name: "another invoice", row: row(domain.StripeRefundNeedsReview, "inv_2"), wantErr: errs.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(newMemStore(), &memInvoiceReader{}, nil)
			s.SetStripeRefundRecording(&rowStore{row: tc.row}, nil, nil)
			got, err := s.linkUnderLockTx(context.Background(), nil, "inv_1", link, tc.existing)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if (got != nil) != tc.wantRow {
				t.Errorf("row to link = %+v, want one: %v", got, tc.wantRow)
			}
		})
	}
}
