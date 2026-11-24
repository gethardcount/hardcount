package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// usageSteppingClock is a server clock that moves a second each time it
// answers a read itself (no instant bound on ctx), and counts those reads.
type usageSteppingClock struct {
	at    time.Time
	reads int
}

func (c *usageSteppingClock) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// releaseSpy records the NotAfter a held release hands the store: the
// release's decided time.
type releaseSpy struct {
	*heldMemStore
	notAfter []time.Time
}

func (r *releaseSpy) ReleaseHeld(ctx context.Context, tenantID string, in ReleaseHeldInput, emit func(*sql.Tx, ReleaseHeldResult) error) (ReleaseHeldResult, error) {
	r.notAfter = append(r.notAfter, in.NotAfter)
	return r.heldMemStore.ReleaseHeld(ctx, tenantID, in, emit)
}

// callSpy records the events a LiteLLM call hands the store.
type callSpy struct {
	*heldMemStore
	events []domain.UsageEvent
}

func (c *callSpy) IngestCallResolvingHeld(ctx context.Context, tenantID string, call StoreCall, after func(*sql.Tx, CallResult) error) (CallResult, error) {
	c.events = append(c.events, call.Events...)
	return c.heldMemStore.IngestCallResolvingHeld(ctx, tenantID, call, after)
}

// Each ingest entry binds its time itself, once per event (a LiteLLM call,
// once for the call), and everything below reads it. Every row runs from
// three contexts: nothing bound, which is what usage's routes get from the
// door; real time marked with no instant (clock.WithRealTime, what a
// scheduled job runs on); and a real instant bound above the service.
// The event is stamped at one read of the server's clock, or at the real
// instant when one is bound.
func TestUsageEntries_StampTheCustomersDecidedTime(t *testing.T) {
	serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	realAt := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	setup := func(store Store) (*Service, *usageSteppingClock) {
		svc := NewService(store)
		server := &usageSteppingClock{at: serverAt.Add(-time.Second)}
		svc.SetClock(server)
		return svc, server
	}
	for _, base := range []struct {
		name string
		ctx  func() context.Context
		// want is when an event is stamped, and serverReads how many reads
		// of the server's clock that takes.
		want        time.Time
		serverReads int
	}{
		{"nothing bound", testMode, serverAt, 1},
		{"real time marked", func() context.Context { return clock.WithRealTime(testMode()) }, serverAt, 1},
		{"a real instant bound above", func() context.Context { return clock.WithEffectiveNow(testMode(), realAt) }, realAt, 0},
	} {
		t.Run("Ingest, from "+base.name, func(t *testing.T) {
			svc, server := setup(newMemStore())
			e, err := svc.Ingest(base.ctx(), "t1", IngestInput{CustomerID: "cus_n", MeterID: "mtr_1", Quantity: dec(1)})
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if !e.Timestamp.Equal(base.want) || server.reads != base.serverReads {
				t.Errorf("stamped %v, %d server reads; want %v, %d", e.Timestamp, server.reads, base.want, base.serverReads)
			}
		})
		t.Run("IngestCall: one instant for the call's events, from "+base.name, func(t *testing.T) {
			store := &callSpy{heldMemStore: &heldMemStore{memStore: newMemStore()}}
			svc, server := setup(store)
			ev := func(key string) CallEvent { return CallEvent{Quantity: dec(1), IdempotencyKey: key} }
			if _, err := svc.IngestCall(base.ctx(), "t1", CallInput{
				CustomerID: "cus_n", MeterID: "mtr_1",
				Events: []CallEvent{ev("c:input"), ev("c:output"), ev("c:cache_write_5m")},
			}); err != nil {
				t.Fatalf("IngestCall: %v", err)
			}
			if len(store.events) != 3 {
				t.Fatalf("%d events; want 3", len(store.events))
			}
			for _, e := range store.events {
				if !e.Timestamp.Equal(base.want) {
					t.Errorf("stamped %v; want %v", e.Timestamp, base.want)
				}
			}
			if server.reads != base.serverReads {
				t.Errorf("%d server reads; want %d for the call", server.reads, base.serverReads)
			}
		})
		t.Run("a held release: its NotAfter is the bound instant, from "+base.name, func(t *testing.T) {
			store := &releaseSpy{heldMemStore: &heldMemStore{memStore: newMemStore()}}
			svc, _ := setup(store)
			if _, err := svc.Release(base.ctx(), "t1", ReleaseInput{ExternalCustomerID: "ext_n", CustomerID: "cus_n", MeterIDs: map[string]string{"tokens": "mtr_1"}}); err != nil {
				t.Fatalf("Release: %v", err)
			}
			if len(store.notAfter) != 1 || !store.notAfter[0].Equal(base.want) {
				t.Errorf("not after %v; want %v", store.notAfter, base.want)
			}
		})
	}
}

// Backfill's future check reads the bound instant, not the machine's time: at
// a past instant, a backfill at that instant is accepted and one a second
// later is refused.
func TestBackfill_TheFutureIsAfterTheBoundInstant(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	ctx := clock.WithEffectiveNow(testMode(), at)
	if _, err := svc.Backfill(ctx, "t1", IngestInput{CustomerID: "cus_n", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &at}); err != nil {
		t.Fatalf("a backfill at the bound instant: %v", err)
	}
	later := at.Add(time.Second)
	if _, err := svc.Backfill(ctx, "t1", IngestInput{CustomerID: "cus_n", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &later}); err == nil {
		t.Error("a backfill a second after the bound instant was accepted; want it refused as in the future")
	}
}

// One instant per action: a LiteLLM call reads the server's clock once for
// all its events, and a backfill once for its check and its ingest.
func TestUsage_DecidesTheCustomerOncePerAction(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	for _, c := range []struct {
		name string
		act  func(*Service) error
	}{
		{"a LiteLLM call with three events", func(s *Service) error {
			ev := func(key string) CallEvent { return CallEvent{Quantity: dec(1), IdempotencyKey: key} }
			_, err := s.IngestCall(testMode(), "t1", CallInput{
				CustomerID: "cus_1", MeterID: "mtr_1",
				Events: []CallEvent{ev("c:input"), ev("c:output"), ev("c:cache_write_5m")},
			})
			return err
		}},
		{"a backfill", func(s *Service) error {
			_, err := s.Backfill(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &past})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := NewService(&heldMemStore{memStore: newMemStore()})
			server := &usageSteppingClock{at: now}
			svc.SetClock(server)
			if err := c.act(svc); err != nil {
				t.Fatalf("act: %v", err)
			}
			if server.reads != 1 {
				t.Errorf("%d server reads; want one for the action", server.reads)
			}
		})
	}
}
