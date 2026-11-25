package creditnote

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// cnDecidedAt is the instant a caller bound, far from the machine's time, so
// a stamp read from the machine's clock instead shows.
var cnDecidedAt = time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)

func cnAtDecidedTime() context.Context {
	return clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), cnDecidedAt)
}

// cnSteppingClock is a server clock that moves a second each time it answers
// a read itself (no instant bound on ctx), and counts those reads.
type cnSteppingClock struct {
	at    time.Time
	reads int
}

func (c *cnSteppingClock) Now(ctx context.Context) time.Time {
	if t, ok := clock.EffectiveNow(ctx); ok {
		return t
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// cnEntry runs one credit-note entry point on ctx against a service whose
// store has the card invoice, and returns the stamp the action wrote.
type cnEntry struct {
	name string
	run  func(t *testing.T, ctx context.Context, svc *Service, store *memStore) *time.Time
}

func cnEntries() []cnEntry {
	draft := func(t *testing.T, svc *Service) domain.CreditNote {
		t.Helper()
		cn, err := svc.Create(clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), cnDecidedAt.Add(-48*time.Hour)), "t1", CreateInput{
			InvoiceID: "inv_card", Reason: "Billing error",
			Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 1000}},
		})
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		return cn
	}
	return []cnEntry{
		{"a credit note is created", func(t *testing.T, ctx context.Context, svc *Service, _ *memStore) *time.Time {
			cn, err := svc.Create(ctx, "t1", CreateInput{
				InvoiceID: "inv_card", Reason: "Billing error",
				Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 1000}},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			return &cn.CreatedAt
		}},
		{"a credit note is issued", func(t *testing.T, ctx context.Context, svc *Service, _ *memStore) *time.Time {
			cn := draft(t, svc)
			issued, err := svc.Issue(ctx, "t1", cn.ID)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			return issued.IssuedAt
		}},
		{"a credit note is voided", func(t *testing.T, ctx context.Context, svc *Service, _ *memStore) *time.Time {
			cn := draft(t, svc)
			voided, err := svc.Void(ctx, "t1", cn.ID)
			if err != nil {
				t.Fatalf("Void: %v", err)
			}
			return voided.VoidedAt
		}},
		{"a refund note is created", func(t *testing.T, ctx context.Context, svc *Service, _ *memStore) *time.Time {
			cn, err := svc.CreateRefund(ctx, "t1", RefundInput{InvoiceID: "inv_card", AmountCents: 1000, Reason: "requested_by_customer"})
			if err != nil {
				t.Fatalf("CreateRefund: %v", err)
			}
			return &cn.CreatedAt
		}},
	}
}

// cnService builds the service every time row uses: inv behind it, a number
// generator, and clk as the server's clock when one is given (else the
// default, the machine's).
func cnService(store Store, inv domain.Invoice, clk clock.Clock) *Service {
	svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}, &fakeRefunder{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetStripeRefundRecording(noStripeRefunds{}, nil, nil)
	if clk != nil {
		svc.SetClock(clk)
	}
	return svc
}

func newCNSvc(clk clock.Clock) (*Service, *memStore) {
	store := newMemStore()
	return cnService(store, cardInvoice(), clk), store
}

// Each credit-note entry point runs on the time its caller bound and reads
// no clock of its own: the stamp it writes is the bound instant.
func TestCreditNoteEntryPoints_RunOnTheDecidedTime(t *testing.T) {
	for _, e := range cnEntries() {
		t.Run(e.name+" at the decided time", func(t *testing.T) {
			svc, store := newCNSvc(nil)
			stamp := e.run(t, cnAtDecidedTime(), svc, store)
			if stamp == nil || !stamp.Equal(cnDecidedAt) {
				t.Errorf("stamp %v; want %v", stamp, cnDecidedAt)
			}
		})
	}
}
