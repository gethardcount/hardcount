package dunning

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// steppingClock is a server clock that moves a second each time it answers
// a read itself (no instant bound on ctx), and counts those reads.
type steppingClock struct {
	at    time.Time
	reads int
}

func (c *steppingClock) Now(ctx context.Context) time.Time {
	if t, ok := clock.EffectiveNow(ctx); ok {
		return t
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// At real time (an invoice on no clock: the decision binds no instant) each
// dunning entry point reads the server's clock once, and every stamp of the
// action is that one instant. Two reads would differ here, as the clock
// steps on each.
func TestDunning_AtRealTimeReadsTheServersClockOnce(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)

	t.Run("a run starts at the server's time, read once", func(t *testing.T) {
		store := &instantStore{memStore: newMemStore()}
		server := &steppingClock{at: past}
		svc := NewService(store, &noopRetrier{}, server)
		run, err := svc.StartDunning(testutil.AtRealTime(), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("StartDunning: %v", err)
		}
		want := past.Add(time.Second)
		if server.reads != 1 || !run.CreatedAt.Equal(want) || run.NextActionAt == nil || !run.NextActionAt.After(want) {
			t.Errorf("server clock read %d times, created_at %v, next_action_at %v; want one read, created at %v and the first retry after it",
				server.reads, run.CreatedAt, run.NextActionAt, want)
		}
		store.wroteAt(t, "StartRun", want)
	})

	t.Run("a resolve stamps the server's time, read once", func(t *testing.T) {
		store := &instantStore{memStore: newMemStore()}
		server := &steppingClock{at: past}
		svc := NewService(store, &noopRetrier{}, server)
		started, err := svc.StartDunning(clock.EnsureEffectiveNow(testutil.AtRealTime(), clock.NewFake(past.Add(-time.Hour))), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("StartDunning: %v", err)
		}
		if err := svc.ResolveByInvoice(testutil.AtRealTime(), "t1", "inv_1", domain.ResolutionPaymentRecovered); err != nil {
			t.Fatalf("ResolveByInvoice: %v", err)
		}
		run := store.runs[started.ID]
		want := past.Add(time.Second)
		if server.reads != 1 || run.ResolvedAt == nil || !run.ResolvedAt.Equal(want) {
			t.Errorf("server clock read %d times, resolved_at %v; want one read, resolved at %v", server.reads, run.ResolvedAt, want)
		}
		store.wroteAt(t, "ResolveRun", want)
		store.wroteAt(t, "CreateEvent", want)
	})

	// The cron's tick reads the server's clock once to list what is due; each
	// run it works then reads it once more, its own instant.
	t.Run("a due run's retry reads the server's clock once", func(t *testing.T) {
		store := &instantStore{memStore: newMemStore()}
		server := &steppingClock{at: past}
		svc := NewService(store, &failingRetrier{}, server)
		run, err := svc.StartDunning(clock.EnsureEffectiveNow(testutil.AtRealTime(), clock.NewFake(past.Add(-48*time.Hour))), "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("StartDunning: %v", err)
		}
		due := past.Add(-time.Hour)
		run.NextActionAt = &due
		store.runs[run.ID] = run
		if _, errs := svc.ProcessDueRuns(testutil.AtRealTime(), "t1", 10); len(errs) != 0 {
			t.Fatalf("ProcessDueRuns: %v", errs)
		}
		got := store.runs[run.ID]
		want := past.Add(2 * time.Second)
		if server.reads != 2 || got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(want) {
			t.Errorf("server clock read %d times, last_attempt_at %v; want two reads (the list, the run), the attempt at %v", server.reads, got.LastAttemptAt, want)
		}
		store.wroteAt(t, "UpdateRunIfActive", want)
	})
}

// instantStore records the instant each write's ctx carries, by method: the
// database store stamps its own columns and queued events from it, so it
// must be the action's one instant.
type instantStore struct {
	*memStore
	at map[string]time.Time
}

func (s *instantStore) saw(ctx context.Context, method string) {
	if s.at == nil {
		s.at = map[string]time.Time{}
	}
	s.at[method], _ = clock.EffectiveNow(ctx)
}

func (s *instantStore) wroteAt(t *testing.T, method string, want time.Time) {
	t.Helper()
	if got, ok := s.at[method]; !ok || !got.Equal(want) {
		t.Errorf("%s ran at %v (called %v); want %v, the action's one instant", method, got, ok, want)
	}
}

func (s *instantStore) StartRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	s.saw(ctx, "StartRun")
	return s.memStore.StartRun(ctx, tenantID, run)
}

func (s *instantStore) ResolveRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, fromActiveOnly bool) (bool, error) {
	s.saw(ctx, "ResolveRun")
	return s.memStore.ResolveRun(ctx, tenantID, run, fromActiveOnly)
}

func (s *instantStore) UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, then func(tx *sql.Tx) error) (bool, error) {
	s.saw(ctx, "UpdateRunIfActive")
	return s.memStore.UpdateRunIfActive(ctx, tenantID, run, expectedAttempts, then)
}

func (s *instantStore) CreateEvent(ctx context.Context, tenantID string, event domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error) {
	s.saw(ctx, "CreateEvent")
	return s.memStore.CreateEvent(ctx, tenantID, event)
}

// The scheduled job lists due runs on one read of the server's clock and
// then works each run on its own read: with two due runs, each run's attempt
// is at its own instant, not the list's and not the other run's.
func TestDunning_TheCronWorksEachRunOnItsOwnInstant(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	store := &instantStore{memStore: newMemStore()}
	server := &steppingClock{at: past}
	svc := NewService(store, &failingRetrier{}, server)
	started := clock.EnsureEffectiveNow(testutil.AtRealTime(), clock.NewFake(past.Add(-48*time.Hour)))
	due := past.Add(-time.Hour)
	var ids []string
	for _, inv := range []string{"inv_1", "inv_2"} {
		run, err := svc.StartDunning(started, "t1", inv, "cus_1", time.Time{}, domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("StartDunning %s: %v", inv, err)
		}
		run.NextActionAt = &due
		store.runs[run.ID] = run
		ids = append(ids, run.ID)
	}
	if _, errs := svc.ProcessDueRuns(testutil.AtRealTime(), "t1", 10); len(errs) != 0 {
		t.Fatalf("ProcessDueRuns: %v", errs)
	}
	if server.reads != 3 {
		t.Fatalf("server clock read %d times; want three (the list, then one per run)", server.reads)
	}
	seen := map[time.Time]bool{}
	for _, id := range ids {
		got := store.runs[id]
		if got.LastAttemptAt == nil {
			t.Fatalf("run %s: no attempt recorded", id)
		}
		at := *got.LastAttemptAt
		if at.Equal(past.Add(time.Second)) || seen[at] {
			t.Errorf("run %s attempted at %v: want its own read, not the list's (%v) or the other run's", id, at, past.Add(time.Second))
		}
		seen[at] = true
	}
}
