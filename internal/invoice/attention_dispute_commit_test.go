package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// fakeDisputeReader reports one dispute for every invoice it is asked about,
// or none.
type fakeDisputeReader struct{ dispute *domain.InvoiceDispute }

func (f fakeDisputeReader) ListDisputesByInvoice(context.Context, string, string) ([]domain.InvoiceDispute, error) {
	return nil, nil
}

func (f fakeDisputeReader) NoticeDisputesByInvoices(_ context.Context, _ string, ids []string) (map[string]domain.InvoiceDispute, error) {
	out := map[string]domain.InvoiceDispute{}
	if f.dispute != nil {
		for _, id := range ids {
			d := *f.dispute
			d.InvoiceID = id
			out[id] = d
		}
	}
	return out, nil
}

func (f fakeDisputeReader) RecordDisputeLoss(context.Context, string, string, string) (domain.InvoiceDispute, error) {
	return domain.InvoiceDispute{}, nil
}

// spyTopupCredit reports a top-up's notes and unused credit, and counts the
// reads.
type spyTopupCredit struct {
	unused                 int64
	noteReads, unusedReads int
}

func (s *spyTopupCredit) ListIssuedForInvoice(context.Context, string, string) ([]domain.CreditNote, error) {
	s.noteReads++
	return nil, nil
}

func (s *spyTopupCredit) UnusedCredit(context.Context, string, string, time.Time) (int64, error) {
	s.unusedReads++
	return s.unused, nil
}

// The hosted page's lookup by token computes no attention: the public page
// shows none, so a paid top-up with a lost dispute, whose operator-facing
// attention reads the top-up's notes, is returned without reading them.
func TestGetByPublicToken_ComputesNoAttention(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	inv, err := store.Create(ctx, "t1", domain.Invoice{
		CustomerID: "cus_1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		BillingReason: domain.BillingReasonTopup, Currency: "USD", TotalAmountCents: 9000, AmountPaidCents: 9000,
		PublicToken: "tok_commit", StripePaymentIntentID: "pi_1",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	credit := &spyTopupCredit{unused: 6000}
	svc := NewService(store, nil, newMemNumberer())
	svc.SetDisputeReader(fakeDisputeReader{dispute: &domain.InvoiceDispute{StripeDisputeID: "du_1", StripePaymentIntentID: "pi_1", Status: domain.DisputeLost, AmountCents: 9000, Currency: "USD"}})
	svc.SetTopupCreditReader(credit)

	got, _, err := svc.GetByPublicToken(ctx, "tok_commit")
	if err != nil {
		t.Fatalf("GetByPublicToken: %v", err)
	}
	if got.ID != inv.ID || got.Attention != nil || credit.noteReads+credit.unusedReads != 0 {
		t.Errorf("invoice %s, attention %+v, reads %d; want %s, no attention, no read", got.ID, got.Attention, credit.noteReads+credit.unusedReads, inv.ID)
	}
}

// TestAttention_TopupDispute_ReadsWhatItsNoticeShows: the invoice service
// reads a paid top-up's credit notes when a dispute on its own payment was
// lost, and its unused credit while a chargeback on it is open, so each
// notice can state its figures. A top-up with neither pays for no read.
func TestAttention_TopupDispute_ReadsWhatItsNoticeShows(t *testing.T) {
	ctx := context.Background()
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	dispute := func(st domain.DisputeStatus) *domain.InvoiceDispute {
		d := &domain.InvoiceDispute{StripeDisputeID: "du_commit_1", StripePaymentIntentID: "pi_commit_1", Status: st, AmountCents: 9000, Currency: "USD"}
		if st.Closed() {
			d.ClosedAt = &closedAt
		}
		return d
	}
	for _, tc := range []struct {
		name                   string
		dispute                *domain.InvoiceDispute
		noteReads, unusedReads int
		reason                 domain.AttentionReason
	}{
		{"a lost dispute reads the notes", dispute(domain.DisputeLost), 1, 0, domain.AttentionReasonDisputeLost},
		{"an open chargeback reads the unused credit", dispute(domain.DisputeNeedsResponse), 0, 1, domain.AttentionReasonDisputeOpen},
		{"no dispute reads nothing", nil, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			inv, err := store.Create(ctx, "t1", domain.Invoice{
				CustomerID: "cus_1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				BillingReason: domain.BillingReasonTopup, Currency: "USD",
				TotalAmountCents: 9000, AmountPaidCents: 9000, StripePaymentIntentID: "pi_commit_1",
			})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			credit := &spyTopupCredit{unused: 6000}
			svc := NewService(store, nil, newMemNumberer())
			svc.SetDisputeReader(fakeDisputeReader{dispute: tc.dispute})
			svc.SetTopupCreditReader(credit)
			got, err := svc.Get(ctx, "t1", inv.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if credit.noteReads != tc.noteReads || credit.unusedReads != tc.unusedReads {
				t.Errorf("reads: notes %d, unused credit %d; want %d, %d", credit.noteReads, credit.unusedReads, tc.noteReads, tc.unusedReads)
			}
			var reason domain.AttentionReason
			if got.Attention != nil {
				reason = got.Attention.Reason
			}
			if reason != tc.reason {
				t.Errorf("attention reason %q, want %q", reason, tc.reason)
			}
		})
	}
}
