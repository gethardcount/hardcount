package clock

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestRealClock_FallbackAndBinding covers the two branches of Real()
// — wall-clock when ctx has no binding, and the bound value when it
// does. Locks in the contract that Real() respects ctx-bound
// effective-now as a first-class case.
func TestRealClock_FallbackAndBinding(t *testing.T) {
	c := Real()

	t.Run("falls back to wall clock without binding", func(t *testing.T) {
		before := time.Now().UTC()
		got := c.Now(context.Background())
		after := time.Now().UTC()
		if got.Before(before) || got.After(after) {
			t.Errorf("wall-clock fallback: got %v, want between %v and %v", got, before, after)
		}
	})

	t.Run("bound effective-now wins over wall clock", func(t *testing.T) {
		want := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
		ctx := WithEffectiveNow(context.Background(), want)
		got := c.Now(ctx)
		if !got.Equal(want) {
			t.Errorf("bound: got %v, want %v", got, want)
		}
	})
}

// TestFakeClock_FallbackAndBinding mirrors the real-clock test for
// Fake. Tests get the same ctx-binding semantics so they can pin
// effective-now without losing Fake's per-test deterministic value.
func TestFakeClock_FallbackAndBinding(t *testing.T) {
	fixed := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	c := NewFake(fixed)

	t.Run("returns fixed time without binding", func(t *testing.T) {
		got := c.Now(context.Background())
		if !got.Equal(fixed) {
			t.Errorf("fake fixed: got %v, want %v", got, fixed)
		}
	})

	t.Run("bound effective-now wins over fixed time", func(t *testing.T) {
		want := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
		ctx := WithEffectiveNow(context.Background(), want)
		got := c.Now(ctx)
		if !got.Equal(want) {
			t.Errorf("fake with binding: got %v, want %v", got, want)
		}
	})

	t.Run("Set + Advance still work", func(t *testing.T) {
		other := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		c.Set(other)
		if got := c.Now(context.Background()); !got.Equal(other) {
			t.Errorf("after Set: got %v, want %v", got, other)
		}
		c.Advance(time.Hour)
		if got := c.Now(context.Background()); !got.Equal(other.Add(time.Hour)) {
			t.Errorf("after Advance(1h): got %v, want %v", got, other.Add(time.Hour))
		}
	})
}

// TestPackageNow covers the clock.Now(ctx) shortcut used by postgres
// stores and render-layer code that doesn't own a Clock field.
func TestPackageNow(t *testing.T) {
	t.Run("falls back to wall clock", func(t *testing.T) {
		before := time.Now().UTC()
		got := Now(context.Background())
		after := time.Now().UTC()
		if got.Before(before) || got.After(after) {
			t.Errorf("wall-clock: got %v, want between %v and %v", got, before, after)
		}
	})

	t.Run("returns bound effective-now", func(t *testing.T) {
		want := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
		ctx := WithEffectiveNow(context.Background(), want)
		got := Now(ctx)
		if !got.Equal(want) {
			t.Errorf("bound: got %v, want %v", got, want)
		}
	})
}

// TestEffectiveNow covers the lookup helper directly.
func TestEffectiveNow(t *testing.T) {
	t.Run("nil ctx returns zero, false", func(t *testing.T) {
		// EffectiveNow explicitly handles nil for defensive callers
		// (postgres-store callsites where ctx might be missing during
		// migration). The lint complaint about nil ctx doesn't apply.
		//nolint:staticcheck // SA1012: intentional nil-ctx test
		got, ok := EffectiveNow(nil)
		if ok {
			t.Error("expected ok=false for nil ctx")
		}
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("unbound ctx returns zero, false", func(t *testing.T) {
		got, ok := EffectiveNow(context.Background())
		if ok {
			t.Error("expected ok=false for unbound ctx")
		}
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("bound ctx returns the value", func(t *testing.T) {
		want := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
		ctx := WithEffectiveNow(context.Background(), want)
		got, ok := EffectiveNow(ctx)
		if !ok {
			t.Error("expected ok=true for bound ctx")
		}
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// countingClock answers a fixed instant and counts the reads it answers.
type countingClock struct {
	at    time.Time
	reads int
}

func (c *countingClock) Now(context.Context) time.Time {
	c.reads++
	return c.at
}

// EnsureEffectiveNow keeps the instant ctx carries and reads no clock;
// otherwise it reads the clock once and binds that instant.
func TestEnsureEffectiveNow(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	server := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name      string
		ctx       context.Context
		want      time.Time
		wantReads int
	}{
		{"an instant is kept", WithEffectiveNow(testMode, frozen), frozen, 0},
		{"real time is read once and bound", WithRealTime(testMode), server, 1},
		{"nothing bound is read once and bound", testMode, server, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			cc := &countingClock{at: server}
			got := EnsureEffectiveNow(c.ctx, cc)
			at, ok := EffectiveNow(got)
			if !ok || !at.Equal(c.want) || cc.reads != c.wantReads {
				t.Errorf("instant %v (bound %v), reads %d; want %v, %d reads", at, ok, cc.reads, c.want, c.wantReads)
			}
			if again := Now(got); !again.Equal(at) {
				t.Errorf("a second read gives %v; want the bound %v", again, at)
			}
		})
	}
}

// AtArrival dates a result at one read of c taken when it arrives: the read
// is bound on ctx as a real instant, replacing any instant ctx carries, so
// every later read of ctx answers it.
func TestAtArrival(t *testing.T) {
	action := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	arrived := action.Add(time.Minute)
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"nothing bound", testMode},
		{"real time with no instant", WithRealTime(testMode)},
		{"the action's real instant", WithEffectiveNow(testMode, action)},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := NewFake(arrived)
			got := AtArrival(c.ctx, server)
			server.Advance(time.Hour) // a later read must answer the bound instant
			at, ok := EffectiveNow(got)
			if later := server.Now(got); !ok || !at.Equal(arrived) || !later.Equal(arrived) {
				t.Errorf("instant %v (%v), later read %v; want %v", at, ok, later, arrived)
			}
		})
	}
}
