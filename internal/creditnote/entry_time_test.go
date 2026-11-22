package creditnote

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// cnDecidedAt is an instant on clock clk_d before its frozen time, as a
// request decided before an advance would carry. laterOnSameClock answers a
// later time on that same clock (its frozen time, three days on) on purpose:
// an entry point that asked it would stamp that later time, so a row that
// sees cnDecidedAt and no reads shows the resolver was never asked.
var cnDecidedAt = time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)

func cnOnDecidedClock() context.Context {
	return clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: cnDecidedAt, TestClockID: "clk_d"})
}

type laterOnSameClock struct{ reads int }

func (r *laterOnSameClock) answer() (clock.Sim, error) {
	r.reads++
	return clock.Sim{At: cnDecidedAt.Add(72 * time.Hour), TestClockID: "clk_d"}, nil
}
func (r *laterOnSameClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *laterOnSameClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *laterOnSameClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
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
// generator, the resolver res, and clk as the server's clock when one is
// given (else the default, the machine's).
func cnService(store Store, inv domain.Invoice, clk clock.Clock, res clock.Resolver) *Service {
	svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}, &fakeRefunder{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetResolver(res)
	if clk != nil {
		svc.SetClock(clk)
	}
	return svc
}

func newCNSvc(clk clock.Clock) (*Service, *memStore, *laterOnSameClock) {
	store, res := newMemStore(), &laterOnSameClock{}
	return cnService(store, cardInvoice(), clk, res), store, res
}

// Each credit-note entry point runs on the time its caller decided and reads
// no clock of its own: the stamp it writes is the decided instant.
func TestCreditNoteEntryPoints_RunOnTheDecidedTime(t *testing.T) {
	for _, e := range cnEntries() {
		t.Run(e.name+" at the decided time", func(t *testing.T) {
			svc, store, res := newCNSvc(nil)
			stamp := e.run(t, cnOnDecidedClock(), svc, store)
			if stamp == nil || !stamp.Equal(cnDecidedAt) || res.reads != 0 {
				t.Errorf("stamp %v, clock reads %d; want %v, none", stamp, res.reads, cnDecidedAt)
			}
		})
	}
}

// Issue binds its own instant: the clawback_issue sweep binds none per note.
// At real time it reads the server's clock once, and the note is issued at
// that instant.
func TestIssue_AtRealTimeReadsTheServersClockOnce(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, e := range cnEntries() {
		if e.name != "a credit note is issued" {
			continue
		}
		server := &cnSteppingClock{at: past}
		svc, store, _ := newCNSvc(server)
		stamp := e.run(t, clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), svc, store)
		if want := past.Add(time.Second); server.reads != 1 || stamp == nil || !stamp.Equal(want) {
			t.Errorf("server clock read %d times, issued at %v; want one read, %v", server.reads, stamp, want)
		}
	}
}
