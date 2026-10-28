package invoice

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// fakeDisputeReader reports one lost dispute for every invoice it is asked
// about, or none.
type fakeDisputeReader struct{ lost *domain.InvoiceDispute }

func (f fakeDisputeReader) ListDisputesByInvoice(context.Context, string, string) ([]domain.InvoiceDispute, error) {
	return nil, nil
}

func (f fakeDisputeReader) LostDisputesByInvoices(_ context.Context, _ string, ids []string) (map[string]domain.InvoiceDispute, error) {
	out := map[string]domain.InvoiceDispute{}
	if f.lost != nil {
		for _, id := range ids {
			d := *f.lost
			d.InvoiceID = id
			out[id] = d
		}
	}
	return out, nil
}

// spyCommitGrants reports a prepaid-commit grant and counts the reads.
type spyCommitGrants struct {
	drawable, consumed int64
	calls              int
}

func (s *spyCommitGrants) CommitGrantExposure(context.Context, string, string, time.Time) (int64, int64, error) {
	s.calls++
	return s.drawable, s.consumed, nil
}

// TestAttention_LostDisputeOnPaidCommitInvoice_ReadsTheGrant: the invoice
// service reads the prepaid-commit grant of a paid invoice when, and only
// when, a dispute on it was lost, so the dispute_lost notice can say the
// credit is still spendable. A paid invoice without a lost dispute still
// never pays for the read (the guard the exposure read was built with).
func TestAttention_LostDisputeOnPaidCommitInvoice_ReadsTheGrant(t *testing.T) {
	ctx := context.Background()
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	lost := &domain.InvoiceDispute{
		StripeDisputeID: "du_commit_1", StripePaymentIntentID: "pi_commit_1", Status: domain.DisputeLost,
		AmountCents: 9000, Currency: "USD", ClosedAt: &closedAt,
	}
	seed := func(t *testing.T, disputes fakeDisputeReader, grants *spyCommitGrants) (*Service, string) {
		t.Helper()
		store := newMemStore()
		inv, err := store.Create(ctx, "t1", domain.Invoice{
			CustomerID: "cus_1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
			BillingReason: domain.BillingReasonManual, Currency: "USD",
			TotalAmountCents: 9000, AmountPaidCents: 9000, StripePaymentIntentID: "pi_commit_1",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		svc := NewService(store, nil, newMemNumberer())
		svc.SetDisputeReader(disputes)
		svc.SetCommitGrantReader(grants)
		return svc, inv.ID
	}

	t.Run("lost dispute: the grant is read and the notice names the spendable credit", func(t *testing.T) {
		grants := &spyCommitGrants{drawable: 6000, consumed: 4000}
		svc, id := seed(t, fakeDisputeReader{lost: lost}, grants)
		inv, err := svc.Get(ctx, "t1", id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if grants.calls != 1 {
			t.Errorf("commit grant reads = %d, want 1", grants.calls)
		}
		att := inv.Attention
		if att == nil || att.Reason != domain.AttentionReasonDisputeLost {
			t.Fatalf("attention %+v, want dispute_lost", att)
		}
		if !strings.Contains(att.Message, domain.FormatMoneyMinor(6000, "USD")) || !strings.Contains(att.Message, "commit relief") {
			t.Errorf("message %q must say the customer can still spend %s and name commit relief",
				att.Message, domain.FormatMoneyMinor(6000, "USD"))
		}
	})

	t.Run("no lost dispute: a paid invoice never reads the grant", func(t *testing.T) {
		grants := &spyCommitGrants{drawable: 6000, consumed: 4000}
		svc, id := seed(t, fakeDisputeReader{}, grants)
		inv, err := svc.Get(ctx, "t1", id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if grants.calls != 0 {
			t.Errorf("commit grant reads = %d, want 0 on a paid invoice with no lost dispute", grants.calls)
		}
		if inv.Attention != nil {
			t.Errorf("attention %+v, want none", inv.Attention)
		}
	})
}
