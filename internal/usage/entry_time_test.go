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

// Each ingest entry decides its customer's time itself, once per event (a
// batch, one per event; a LiteLLM call, one for the call), and everything
// below reads that decided time: an event on a customer on a test clock is
// stamped at that clock's frozen time, and on a customer on no clock at one
// read of the server's clock.
func TestUsageEntries_StampTheCustomersDecidedTime(t *testing.T) {
	frozenA := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	frozenB := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	setup := func(store Store) (*Service, *fakeResolver, *usageSteppingClock) {
		svc := NewService(store)
		res := &fakeResolver{frozen: map[string]time.Time{"cus_a": frozenA, "cus_b": frozenB}}
		svc.SetResolver(res)
		server := &usageSteppingClock{at: serverAt.Add(-time.Second)}
		svc.SetClock(server)
		return svc, res, server
	}

	t.Run("Ingest, a customer on a test clock", func(t *testing.T) {
		svc, res, server := setup(newMemStore())
		e, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_a", MeterID: "mtr_1", Quantity: dec(1)})
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if !e.Timestamp.Equal(frozenA) || res.calls != 1 || server.reads != 0 {
			t.Errorf("stamped %v, %d resolver reads, %d server reads; want %v, one, none", e.Timestamp, res.calls, server.reads, frozenA)
		}
	})
	t.Run("Ingest, a customer on no clock", func(t *testing.T) {
		svc, res, server := setup(newMemStore())
		e, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_n", MeterID: "mtr_1", Quantity: dec(1)})
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if !e.Timestamp.Equal(serverAt) || res.calls != 1 || server.reads != 1 {
			t.Errorf("stamped %v, %d resolver reads, %d server reads; want %v, one, one", e.Timestamp, res.calls, server.reads, serverAt)
		}
	})
	t.Run("a batch: two customers on two clocks and one on none", func(t *testing.T) {
		store := newMemStore()
		svc, res, server := setup(store)
		result, batchErrs := svc.BatchIngest(testMode(), "t1", []IngestInput{
			{CustomerID: "cus_a", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "b-a"},
			{CustomerID: "cus_b", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "b-b"},
			{CustomerID: "cus_n", MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: "b-n"},
		})
		if len(batchErrs) != 0 || result.Inserted != 3 {
			t.Fatalf("batch: %+v, errors %v", result, batchErrs)
		}
		want := map[string]time.Time{"cus_a": frozenA, "cus_b": frozenB, "cus_n": serverAt}
		for _, e := range store.events {
			if !e.Timestamp.Equal(want[e.CustomerID]) {
				t.Errorf("%s stamped %v; want %v", e.CustomerID, e.Timestamp, want[e.CustomerID])
			}
		}
		if res.calls != 3 || server.reads != 1 {
			t.Errorf("%d resolver reads, %d server reads; want one per event (3), one (the customer on no clock)", res.calls, server.reads)
		}
	})
	t.Run("Backfill: its future check is the customer's decided time", func(t *testing.T) {
		svc, res, _ := setup(newMemStore())
		at := frozenA
		if _, err := svc.Backfill(testMode(), "t1", IngestInput{CustomerID: "cus_a", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &at}); err != nil {
			t.Fatalf("a backfill at the clock's frozen time: %v", err)
		}
		if res.calls != 1 {
			t.Errorf("%d resolver reads; want one", res.calls)
		}
		later := frozenA.Add(time.Second)
		if _, err := svc.Backfill(testMode(), "t1", IngestInput{CustomerID: "cus_a", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &later}); err == nil {
			t.Error("a backfill a second after the clock's frozen time was accepted; want it refused as in the future")
		}
	})
	t.Run("IngestCall: one decision for the call's events", func(t *testing.T) {
		store := &callSpy{heldMemStore: &heldMemStore{memStore: newMemStore()}}
		svc, res, _ := setup(store)
		ev := func(key string) CallEvent { return CallEvent{Quantity: dec(1), IdempotencyKey: key} }
		if _, err := svc.IngestCall(testMode(), "t1", CallInput{
			CustomerID: "cus_a", MeterID: "mtr_1",
			Events: []CallEvent{ev("c:input"), ev("c:output"), ev("c:cache_write_5m")},
		}); err != nil {
			t.Fatalf("IngestCall: %v", err)
		}
		if len(store.events) != 3 {
			t.Fatalf("%d events; want 3", len(store.events))
		}
		for _, e := range store.events {
			if !e.Timestamp.Equal(frozenA) {
				t.Errorf("stamped %v; want %v", e.Timestamp, frozenA)
			}
		}
		if res.calls != 1 {
			t.Errorf("%d resolver reads; want one for the call", res.calls)
		}
	})
	t.Run("a held release: its NotAfter is the customer's decided time", func(t *testing.T) {
		store := &releaseSpy{heldMemStore: &heldMemStore{memStore: newMemStore()}}
		svc, res, _ := setup(store)
		if _, err := svc.Release(testMode(), "t1", ReleaseInput{ExternalCustomerID: "ext_a", CustomerID: "cus_a", MeterIDs: map[string]string{"tokens": "mtr_1"}}); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if len(store.notAfter) != 1 || !store.notAfter[0].Equal(frozenA) || res.calls != 1 {
			t.Errorf("not after %v, %d resolver reads; want %v, one", store.notAfter, res.calls, frozenA)
		}
	})
}

// One decision per action at real time (a customer on no clock): a LiteLLM
// call decides its customer once for all its events, and a backfill once for
// its check and its ingest.
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
			res := &answerResolver{sim: clock.Sim{At: now}}
			svc.SetResolver(res)
			if err := c.act(svc); err != nil {
				t.Fatalf("act: %v", err)
			}
			if res.reads != 1 {
				t.Errorf("%d resolver reads; want one decision for the action", res.reads)
			}
		})
	}
}
