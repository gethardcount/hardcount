package subscription

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// subDecidedAt is the instant a caller bound. The server's clock in these
// tests answers a month later, so a stamp read from it instead shows.
var subDecidedAt = time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)

// subAtDecidedTime is a test-mode ctx carrying subDecidedAt.
func subAtDecidedTime() context.Context {
	return clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), subDecidedAt)
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
// and a subscription seeded around base. It returns the stamp
// the action wrote.
type subEntry struct {
	name string
	run  func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time) time.Time
}

func subEntries() []subEntry {
	return []subEntry{
		// Create makes a new subscription (the seeded one is unused): its
		// stored created_at and started_at are one instant, and its period
		// starts on that instant's day (the tenant's zone, UTC here).
		{"Create", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, _ domain.Subscription, _ time.Time) time.Time {
			created, err := svc.Create(ctx, "t1", CreateInput{
				Code: "sub-created", DisplayName: "Created", CustomerID: "cus_time",
				Items: []CreateItemInput{{PlanID: "pln_1"}},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			day := created.CreatedAt.UTC().Truncate(24 * time.Hour)
			if created.StartedAt == nil || !created.StartedAt.Equal(created.CreatedAt) ||
				created.CurrentBillingPeriodStart == nil || !created.CurrentBillingPeriodStart.Equal(day) {
				t.Fatalf("created_at %v, started_at %v, period start %v; want one instant, and the period from its day", created.CreatedAt, created.StartedAt, created.CurrentBillingPeriodStart)
			}
			return created.CreatedAt
		}},
		{"Cancel", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, _, err := svc.Cancel(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			return *out.CanceledAt
		}},
		{"CancelForDunning", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, _, err := svc.CancelForDunning(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("CancelForDunning: %v", err)
			}
			return *out.CanceledAt
		}},
		{"ScheduleCancel", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, err := svc.ScheduleCancel(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("ScheduleCancel: %v", err)
			}
			return out.UpdatedAt
		}},
		{"ClearScheduledCancel", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			if _, err := svc.ScheduleCancel(clock.WithEffectiveNow(ctx, base.Add(-time.Hour)), "t1", sub.ID); err != nil {
				t.Fatalf("seed the schedule: %v", err)
			}
			out, _, err := svc.ClearScheduledCancel(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("ClearScheduledCancel: %v", err)
			}
			return out.UpdatedAt
		}},
		{"UpdateItem (seat change)", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			q := int64(3)
			res, err := svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{Quantity: &q})
			if err != nil {
				t.Fatalf("UpdateItem: %v", err)
			}
			return res.Item.UpdatedAt
		}},
		{"CancelPendingItemChange", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			if _, err := svc.store.SetItemPendingPlanTx(context.Background(), nil, "t1", sub.Items[0].ID, "pln_2", base.Add(72*time.Hour)); err != nil {
				t.Fatalf("seed pending plan: %v", err)
			}
			item, _, err := svc.CancelPendingItemChange(ctx, "t1", sub.ID, sub.Items[0].ID)
			if err != nil {
				t.Fatalf("CancelPendingItemChange: %v", err)
			}
			return item.UpdatedAt
		}},
		{"AddItem", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			item, err := svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "pln_2", Quantity: 1})
			if err != nil {
				t.Fatalf("AddItem: %v", err)
			}
			return item.CreatedAt
		}},
		{"RemoveItem", func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			extra, err := svc.AddItem(clock.WithEffectiveNow(ctx, base.Add(-time.Hour)), "t1", sub.ID, AddItemInput{PlanID: "pln_2", Quantity: 1})
			if err != nil {
				t.Fatalf("seed extra item: %v", err)
			}
			if err := svc.RemoveItem(ctx, "t1", sub.ID, extra.ID); err != nil {
				t.Fatalf("RemoveItem: %v", err)
			}
			return store.last(t)
		}},
		{"PauseCollection", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			out, err := svc.PauseCollection(ctx, "t1", sub.ID, PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft})
			if err != nil {
				t.Fatalf("PauseCollection: %v", err)
			}
			return out.UpdatedAt
		}},
		{"PauseCollectionForDunning", func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			if _, err := svc.PauseCollectionForDunning(ctx, "t1", sub.ID, "inv_1"); err != nil {
				t.Fatalf("PauseCollectionForDunning: %v", err)
			}
			got, err := store.Get(context.Background(), "t1", sub.ID)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			return got.UpdatedAt
		}},
		{"ResumeCollection", func(t *testing.T, ctx context.Context, svc *Service, _ *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
			if _, err := svc.PauseCollection(clock.WithEffectiveNow(ctx, base.Add(-time.Hour)), "t1", sub.ID, PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft}); err != nil {
				t.Fatalf("seed the pause: %v", err)
			}
			out, _, err := svc.ResumeCollection(ctx, "t1", sub.ID)
			if err != nil {
				t.Fatalf("ResumeCollection: %v", err)
			}
			return out.UpdatedAt
		}},
		{"SetBillingThresholds", func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, _ time.Time) time.Time {
			if _, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{AmountGTE: 10000}); err != nil {
				t.Fatalf("SetBillingThresholds: %v", err)
			}
			return store.last(t)
		}},
		{"ClearBillingThresholds", func(t *testing.T, ctx context.Context, svc *Service, store *writeSpyStore, sub domain.Subscription, base time.Time) time.Time {
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

func newSubTimeSvc(t *testing.T, base time.Time, server clock.Clock) (*Service, *writeSpyStore, domain.Subscription) {
	t.Helper()
	store := &writeSpyStore{memStore: newMemStore()}
	svc := NewService(store, server)
	ps, pe := base.Add(-3*24*time.Hour), base.Add(28*24*time.Hour)
	seed := domain.Subscription{
		Code: "sub-time", DisplayName: "Time", CustomerID: "cus_time", Status: domain.SubscriptionActive,
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
		Items: []domain.SubscriptionItem{{PlanID: "pln_1", Quantity: 1}},
	}
	sub, err := store.CreateWithBill(clock.WithEffectiveNow(context.Background(), base.Add(-2*time.Hour)), "t1", seed, nil)
	if err != nil {
		t.Fatalf("seed sub: %v", err)
	}
	return svc, store, sub
}

// Each subscription entry point runs on the time its caller bound and reads
// no clock of its own: the stamp it writes is the bound instant, and the
// server's clock is not read.
func TestSubscriptionEntryPoints_RunOnTheDecidedTime(t *testing.T) {
	for _, e := range subEntries() {
		t.Run(e.name, func(t *testing.T) {
			server := &subSteppingClock{at: subDecidedAt.Add(30 * 24 * time.Hour)}
			svc, store, sub := newSubTimeSvc(t, subDecidedAt, server)
			if at := e.run(t, subAtDecidedTime(), svc, store, sub, subDecidedAt); !at.Equal(subDecidedAt) || server.reads != 0 {
				t.Errorf("stamped %v; server clock reads %d; want %v, none", at, server.reads, subDecidedAt)
			}
		})
	}
}

// reliefTimeBiller is the fake biller that also records the instant the ctx
// of each relief's finish carries (the relief credit note is issued on that
// ctx: creditnote.Service.Issue stamps issued_at from its time).
type reliefTimeBiller struct {
	*fakeBiller
	at []time.Time
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
	run  func(t *testing.T, ctx context.Context, svc *Service, sub domain.Subscription)
}

func reliefEntries() []reliefEntry {
	return []reliefEntry{
		{"Cancel (the cancel route; dunning's final cancel through Cancel)", func(t *testing.T, ctx context.Context, svc *Service, sub domain.Subscription) {
			if _, _, err := svc.Cancel(ctx, "t1", sub.ID); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}},
		{"CancelForDunning", func(t *testing.T, ctx context.Context, svc *Service, sub domain.Subscription) {
			if _, _, err := svc.CancelForDunning(ctx, "t1", sub.ID); err != nil {
				t.Fatalf("CancelForDunning: %v", err)
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

// A cancel's relief credit note is issued on the time its caller bound: the
// cancel route's (its request's) or dunning's final action's (its run). The
// server's clock is not read.
func TestSubscriptionRelief_IsIssuedOnTheDecidedTime(t *testing.T) {
	for _, e := range reliefEntries() {
		t.Run(e.name, func(t *testing.T) {
			server := &subSteppingClock{at: subDecidedAt.Add(30 * 24 * time.Hour)}
			svc, _, sub := newSubTimeSvc(t, subDecidedAt, server)
			b := withReliefBiller(svc)
			e.run(t, subAtDecidedTime(), svc, sub)
			if len(b.at) != 1 || !b.at[0].Equal(subDecidedAt) || server.reads != 0 {
				t.Errorf("relief issued at %v; server clock reads %d; want once at %v, none", b.at, server.reads, subDecidedAt)
			}
		})
	}
}

// closeTimeBiller closes a subscription's ended periods on the clock it is
// given and records the instant the ctx of each close carries.
type closeTimeBiller struct {
	store *writeSpyStore
	clk   clock.Clock
	at    []time.Time
}

func (b *closeTimeBiller) BillDue(ctx context.Context, _, id string, grace time.Duration) (int, error) {
	at, _ := clock.EffectiveNow(ctx)
	b.at = append(b.at, at)
	sub := b.store.subs[id]
	n := 0
	for sub.NextBillingAt != nil && !sub.NextBillingAt.After(b.clk.Now(ctx).Add(-grace)) {
		start := *sub.NextBillingAt
		end := start.AddDate(0, 1, 0)
		sub.CurrentBillingPeriodStart, sub.CurrentBillingPeriodEnd, sub.NextBillingAt = &start, &end, &end
		n++
	}
	b.store.subs[id] = sub
	return n, nil
}

// closeEndedSub seeds an active subscription whose period ended ten days
// before base, and wires a due biller that closes it on server.
func closeEndedSub(t *testing.T, base time.Time, server clock.Clock) (*Service, *closeTimeBiller, string) {
	t.Helper()
	svc, store, sub := newSubTimeSvc(t, base, server)
	ended := store.subs[sub.ID]
	ps, pe := base.Add(-40*24*time.Hour), base.Add(-10*24*time.Hour)
	ended.CurrentBillingPeriodStart, ended.CurrentBillingPeriodEnd, ended.NextBillingAt = &ps, &pe, &pe
	ended.BillingTime = domain.BillingTimeCalendar
	store.subs[sub.ID] = ended
	b := &closeTimeBiller{store: store, clk: server}
	svc.SetDueBiller(b)
	return svc, b, sub.ID
}

// Closing an ended period runs on the caller's one time for the whole close:
// the period check, the close and the check after it all read the bound
// time, and the server's clock is not read.
func TestCloseEndedPeriodFirst_OneTimeForTheWholeClose(t *testing.T) {
	server := &subSteppingClock{at: subDecidedAt.Add(30 * 24 * time.Hour)}
	svc, b, id := closeEndedSub(t, subDecidedAt, server)
	if _, err := svc.CloseEndedPeriodFirst(subAtDecidedTime(), "t1", id); err != nil {
		t.Fatalf("CloseEndedPeriodFirst: %v", err)
	}
	if len(b.at) != 1 || !b.at[0].Equal(subDecidedAt) || server.reads != 0 {
		t.Errorf("closed at %v; server clock reads %d; want once at %v, none", b.at, server.reads, subDecidedAt)
	}
}
