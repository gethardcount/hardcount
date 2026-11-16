package subscription

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// subDecidedAt is an instant on clock tclk_1 before its frozen time, as a
// request decided before an advance would carry. subLaterOnSameClock answers
// a later time on that same clock (its frozen time, three days on) on
// purpose: an entry point that asked it would stamp that later time, so a row
// that sees subDecidedAt and no reads shows the resolver was never asked.
var subDecidedAt = time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)

type subLaterOnSameClock struct{ reads int }

func (r *subLaterOnSameClock) answer() (clock.Sim, error) {
	r.reads++
	return clock.Sim{At: subDecidedAt.Add(72 * time.Hour), TestClockID: "tclk_1"}, nil
}
func (r *subLaterOnSameClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *subLaterOnSameClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *subLaterOnSameClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// subSteppingClock is a server clock that moves a second each time it answers
// a read itself (no instant bound on ctx), and counts those reads.
type subSteppingClock struct {
	at    time.Time
	reads int
}

func (c *subSteppingClock) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// writeSpyStore records the instant the ctx of an item removal or a
// thresholds write carries. The in-memory store stamps neither (the database
// store's DELETE stamps nothing; its thresholds writes stamp updated_at from
// that instant), so the spy is the witness.
type writeSpyStore struct {
	*memStore
	at []time.Time
}

func (s *writeSpyStore) seen(ctx context.Context) {
	at, _ := clock.EffectiveNow(ctx)
	s.at = append(s.at, at)
}

func (s *writeSpyStore) last(t *testing.T) time.Time {
	t.Helper()
	if len(s.at) == 0 {
		t.Fatal("the spied write never ran")
	}
	return s.at[len(s.at)-1]
}

func (s *writeSpyStore) RemoveItemTx(ctx context.Context, tx *sql.Tx, tenantID, itemID string) error {
	s.seen(ctx)
	return s.memStore.RemoveItemTx(ctx, tx, tenantID, itemID)
}

func (s *writeSpyStore) SetBillingThresholds(ctx context.Context, tenantID, id string, t domain.BillingThresholds) (domain.Subscription, error) {
	s.seen(ctx)
	return s.memStore.SetBillingThresholds(ctx, tenantID, id, t)
}

func (s *writeSpyStore) ClearBillingThresholds(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s.seen(ctx)
	return s.memStore.ClearBillingThresholds(ctx, tenantID, id)
}

// subEntry is a subscription entry point run on ctx against a fresh service
// and a subscription seeded around base (on clock tclk_1 when clocked). It
// returns the stamp the action wrote. realTimeReads is how many times it
// reads the server's clock at real time when not 1 (see ScheduleCancel).
type subEntry struct {
	name          string
	status        domain.SubscriptionStatus
	realTimeReads int
	run           func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time) time.Time
}

func subEntries() []subEntry {
	return []subEntry{
		{"Cancel", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, _, err := svc.Cancel(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			return *out.CanceledAt
		}},
		{"CancelForDunning", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, _, err := svc.CancelForDunning(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("CancelForDunning: %v", err)
			}
			return *out.CanceledAt
		}},
		// At real time ScheduleCancel reads the server's clock twice, as on
		// main: it closes an ended period (CloseEndedPeriodFirst, on its own
		// instant) before it takes the schedule's. The door binding one
		// instant per request ends that (the entry-binding plan).
		{"ScheduleCancel", domain.SubscriptionActive, 2, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			at := base.Add(35 * 24 * time.Hour)
			out, err := svc.ScheduleCancel(ctx, "t1", sub.ID, ScheduleCancelInput{CancelAt: &at})
			if err != nil {
				t.Fatalf("ScheduleCancel: %v", err)
			}
			return out.UpdatedAt
		}},
		{"ClearScheduledCancel", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			at := base.Add(35 * 24 * time.Hour)
			if _, err := svc.ScheduleCancel(clock.WithEffectiveNow(ctx, base.Add(-time.Hour)), "t1", sub.ID, ScheduleCancelInput{CancelAt: &at}); err != nil {
				t.Fatalf("seed the schedule: %v", err)
			}
			out, _, err := svc.ClearScheduledCancel(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("ClearScheduledCancel: %v", err)
			}
			return out.UpdatedAt
		}},
		{"UpdateItem (seat change)", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			q := int64(3)
			res, err := svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{Quantity: &q})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			return res.Item.UpdatedAt
		}},
		{"CancelPendingItemChange", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			if _, err := svc.store.SetItemPendingPlanTx(context.Background(), nil, "t1", sub.Items[0].ID, "pln_2", base.Add(72*time.Hour)); err != nil {
				t.Fatalf("seed pending plan: %v", err)
			}
			item, _, err := svc.CancelPendingItemChange(ctx, "t1", sub.ID, sub.Items[0].ID)
			if err != nil {
				t.Fatalf("CancelPendingItemChange: %v", err)
			}
			return item.UpdatedAt
		}},
		{"AddItem", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			item, err := svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "pln_2", Quantity: 1})
			if err != nil {
				t.Fatalf("AddItem: %v", err)
			}
			return item.CreatedAt
		}},
		{"RemoveItem", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			extra, err := svc.AddItem(clock.WithEffectiveNow(ctx, base.Add(-time.Hour)), "t1", sub.ID, AddItemInput{PlanID: "pln_2", Quantity: 1})
			if err != nil {
				t.Fatalf("seed extra item: %v", err)
			}
			if err := svc.RemoveItem(ctx, "t1", sub.ID, extra.ID); err != nil {
				t.Fatalf("RemoveItem: %v", err)
			}
			return store.last(t)
		}},
		{"Activate", domain.SubscriptionDraft, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			// The day-1 invoice is billed in the activation's transaction and
			// finalized after the commit on the activation's ctx: its
			// finalize is what the time the ctx carries decides.
			b := &reliefTimeBiller{fakeBiller: &fakeBiller{createTxOK: true, createTxInv: domain.Invoice{ID: "inv_day1", Currency: "USD"}}}
			svc.SetBiller(b)
			if _, err := svc.Activate(ctx, "t1", sub.ID); err != nil {
				t.Fatalf("Activate: %v", err)
			}
			if len(b.finalizeAt) != 1 {
				t.Fatalf("%d day-1 finalizes; want 1", len(b.finalizeAt))
			}
			return b.finalizeAt[0]
		}},
		{"PauseCollection", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, err := svc.PauseCollection(ctx, "t1", sub.ID, PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft})
			if err != nil {
				t.Fatalf("PauseCollection: %v", err)
			}
			return out.UpdatedAt
		}},
		{"PauseCollectionForDunning", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			if _, err := svc.PauseCollectionForDunning(ctx, "t1", sub.ID, "inv_1"); err != nil {
				t.Fatalf("PauseCollectionForDunning: %v", err)
			}
			got, err := store.Get(context.Background(), "t1", sub.ID)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			return got.UpdatedAt
		}},
		{"ResumeCollection", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			if _, err := svc.PauseCollection(clock.WithEffectiveNow(ctx, base.Add(-time.Hour)), "t1", sub.ID, PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft}); err != nil {
				t.Fatalf("seed the pause: %v", err)
			}
			out, _, err := svc.ResumeCollection(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("ResumeCollection: %v", err)
			}
			return out.UpdatedAt
		}},
		{"EndTrial", domain.SubscriptionTrialing, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, err := svc.EndTrial(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("EndTrial: %v", err)
			}
			return out.UpdatedAt
		}},
		{"ExtendTrial", domain.SubscriptionTrialing, 0, func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			out, err := svc.ExtendTrial(ctx, "t1", sub.ID, base.Add(20*24*time.Hour))
			if err != nil {
				t.Fatalf("ExtendTrial: %v", err)
			}
			return out.UpdatedAt
		}},
		{"SetBillingThresholds", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			if _, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{AmountGTE: 10000}); err != nil {
				t.Fatalf("SetBillingThresholds: %v", err)
			}
			return store.last(t)
		}},
		{"ClearBillingThresholds", domain.SubscriptionActive, 0, func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			if _, err := svc.SetBillingThresholds(clock.WithEffectiveNow(ctx, base.Add(-time.Hour)), "t1", sub.ID, BillingThresholdsInput{AmountGTE: 10000}); err != nil {
				t.Fatalf("seed thresholds: %v", err)
			}
			if _, err := svc.ClearBillingThresholds(ctx, "t1", sub.ID); err != nil {
				t.Fatalf("ClearBillingThresholds: %v", err)
			}
			return store.last(t)
		}},
	}
}

func newSubTimeSvc(t *testing.T, status domain.SubscriptionStatus, clocked bool, base time.Time, server clock.Clock) (*Service, *writeSpyStore, domain.Subscription, *subLaterOnSameClock) {
	t.Helper()
	store := &writeSpyStore{memStore: newMemStore()}
	svc := NewService(store, server)
	res := &subLaterOnSameClock{}
	svc.SetResolver(res)
	ps, pe := base.Add(-3*24*time.Hour), base.Add(28*24*time.Hour)
	seed := domain.Subscription{
		Code: "sub-time", DisplayName: "Time", CustomerID: "cus_time", Status: status,
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
		Items: []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
	}
	if clocked {
		seed.TestClockID = "tclk_1"
	}
	if status == domain.SubscriptionTrialing {
		trialEnd := base.Add(7 * 24 * time.Hour)
		seed.TrialEndAt = &trialEnd
	}
	sub, err := store.CreateWithBill(clock.WithEffectiveNow(context.Background(), base.Add(-2*time.Hour)), "t1", seed, nil)
	if err != nil {
		t.Fatalf("seed sub: %v", err)
	}
	return svc, store, sub, res
}

// Each subscription entry point runs on the time its caller decided and reads
// no clock of its own: the stamp it writes is the decided instant, and
// neither the resolver nor the server's clock is read.
func TestSubscriptionEntryPoints_RunOnTheDecidedTime(t *testing.T) {
	for _, e := range subEntries() {
		t.Run(e.name, func(t *testing.T) {
			server := &subSteppingClock{at: subDecidedAt.Add(30 * 24 * time.Hour)}
			svc, store, sub, res := newSubTimeSvc(t, e.status, true, subDecidedAt, server)
			ctx := clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: subDecidedAt, TestClockID: "tclk_1"})
			if at := e.run(t, ctx, svc, store, sub, subDecidedAt); !at.Equal(subDecidedAt) || res.reads != 0 || server.reads != 0 {
				t.Errorf("stamped %v; resolver reads %d, server clock reads %d; want %v, none, none", at, res.reads, server.reads, subDecidedAt)
			}
		})
	}
}

// At real time (a subscription on no test clock: the decision binds no
// instant) each entry point reads the server's clock once, and its stamp is
// that instant.
func TestSubscriptionEntryPoints_AtRealTimeReadTheServersClockOnce(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, e := range subEntries() {
		t.Run(e.name, func(t *testing.T) {
			server := &subSteppingClock{at: past}
			svc, store, sub, _ := newSubTimeSvc(t, e.status, false, past, server)
			ctx := clock.WithRealTime(postgres.WithLivemode(context.Background(), false))
			reads := e.realTimeReads
			if reads == 0 {
				reads = 1
			}
			if want, at := past.Add(time.Duration(reads)*time.Second), e.run(t, ctx, svc, store, sub, past); server.reads != reads || !at.Equal(want) {
				t.Errorf("server clock read %d times, stamped %v; want %d, %v", server.reads, at, reads, want)
			}
		})
	}
}

// reliefTimeBiller is the fake biller that also records the instant the ctx
// of each relief's finish carries (the relief credit note is issued on that
// ctx: creditnote.Service.Issue stamps issued_at from its time), and of each
// day-1 invoice's finalize.
type reliefTimeBiller struct {
	*fakeBiller
	at         []time.Time
	finalizeAt []time.Time
}

func (b *reliefTimeBiller) FinalizeOnCreateInvoice(ctx context.Context, sub domain.Subscription, inv domain.Invoice) {
	at, _ := clock.EffectiveNow(ctx)
	b.finalizeAt = append(b.finalizeAt, at)
	b.fakeBiller.FinalizeOnCreateInvoice(ctx, sub, inv)
}

func (b *reliefTimeBiller) FinishRelief(ctx context.Context, sub domain.Subscription, r domain.Relief) error {
	at, _ := clock.EffectiveNow(ctx)
	b.at = append(b.at, at)
	return b.fakeBiller.FinishRelief(ctx, sub, r)
}

// reliefEntry is a subscription entry point that writes a relief and issues
// its credit note through the biller's FinishRelief.
type reliefEntry struct {
	name string
	run  func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time)
}

func reliefEntries() []reliefEntry {
	return []reliefEntry{
		{"Cancel (the cancel route; dunning's final cancel through Cancel)", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) {
			if _, _, err := svc.Cancel(ctx, "t1", sub.ID); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}},
		{"CancelForDunning", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) {
			if _, _, err := svc.CancelForDunning(ctx, "t1", sub.ID); err != nil {
				t.Fatalf("CancelForDunning: %v", err)
			}
		}},
		{"FireDueScheduledCancel (the billing run and the catch-up)", func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time) {
			cancelAt := base.Add(-time.Hour)
			due := store.subs[sub.ID]
			due.CancelAt = &cancelAt
			due.BillingTime = domain.BillingTimeCalendar
			due.NextBillingAt = due.CurrentBillingPeriodEnd
			store.subs[sub.ID] = due
			if err := svc.FireDueScheduledCancel(ctx, "t1", sub.ID, cancelAt); err != nil {
				t.Fatalf("FireDueScheduledCancel: %v", err)
			}
		}},
	}
}

func withReliefBiller(svc *Service) *reliefTimeBiller {
	b := &reliefTimeBiller{fakeBiller: &fakeBiller{
		draftIDs: []string{"cn_relief"}, draftCredit: 2500,
		finalCancelInv: domain.Invoice{ID: "inv_final", Currency: "USD"},
	}}
	svc.SetBiller(b)
	return b
}

// A cancel's relief credit note is issued on the time its caller decided:
// the cancel route's (the door), dunning's final action's (its run), the
// billing run's or the catch-up's. On a decided time neither the resolver nor
// the server's clock is read.
func TestSubscriptionRelief_IsIssuedOnTheDecidedTime(t *testing.T) {
	for _, e := range reliefEntries() {
		t.Run(e.name, func(t *testing.T) {
			server := &subSteppingClock{at: subDecidedAt.Add(30 * 24 * time.Hour)}
			svc, store, sub, res := newSubTimeSvc(t, domain.SubscriptionActive, true, subDecidedAt, server)
			b := withReliefBiller(svc)
			e.run(t, clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: subDecidedAt, TestClockID: "tclk_1"}), svc, store, sub, subDecidedAt)
			if len(b.at) != 1 || !b.at[0].Equal(subDecidedAt) || res.reads != 0 || server.reads != 0 {
				t.Errorf("relief issued at %v; resolver reads %d, server clock reads %d; want once at %v, none, none", b.at, res.reads, server.reads, subDecidedAt)
			}
		})
	}
}

// At real time the cancel reads the server's clock once, and its relief
// credit note is issued at that instant.
func TestSubscriptionRelief_AtRealTimeIsIssuedOnOneReadOfTheServersClock(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, e := range reliefEntries() {
		t.Run(e.name, func(t *testing.T) {
			server := &subSteppingClock{at: past}
			svc, store, sub, _ := newSubTimeSvc(t, domain.SubscriptionActive, false, past, server)
			b := withReliefBiller(svc)
			e.run(t, clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), svc, store, sub, past)
			if want := past.Add(time.Second); server.reads != 1 || len(b.at) != 1 || !b.at[0].Equal(want) {
				t.Errorf("server clock read %d times, relief issued at %v; want one read, once at %v", server.reads, b.at, want)
			}
		})
	}
}
