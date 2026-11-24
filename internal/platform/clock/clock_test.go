package clock

import (
	"context"
	"errors"
	"fmt"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"testing"
	"time"
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

type stubResolver struct {
	invoiceFunc func() (Sim, error)
	subFunc     func() (Sim, error)
	custFunc    func() (Sim, error)
}

func (s *stubResolver) SimForInvoice(_ context.Context, _, _ string) (Sim, error) {
	if s.invoiceFunc == nil {
		return Sim{}, errors.New("invoiceFunc not stubbed")
	}
	return s.invoiceFunc()
}

func (s *stubResolver) SimForSubscription(_ context.Context, _, _ string) (Sim, error) {
	if s.subFunc == nil {
		return Sim{}, errors.New("subFunc not stubbed")
	}
	return s.subFunc()
}

func (s *stubResolver) SimForCustomer(_ context.Context, _, _ string) (Sim, error) {
	if s.custFunc == nil {
		return Sim{}, errors.New("custFunc not stubbed")
	}
	return s.custFunc()
}

// TestSimOf_RequiresBothHalves pins the rule that makes the sim axis
// trustworthy: a half-set binding is NOT a simulation. A clock id with a zero
// instant (or an instant with no clock) would otherwise stamp an audit row that
// sits inside the partial clock index and answers sim-time queries with a
// wall-clock timestamp — a lie in an append-only log.
func TestSimOf_RequiresBothHalves(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)

	t.Run("both halves = simulated", func(t *testing.T) {
		ctx := WithSim(context.Background(), Sim{At: frozen, TestClockID: "tc_1"})
		sim, ok := SimOf(ctx)
		if !ok || sim.TestClockID != "tc_1" || !sim.At.Equal(frozen) {
			t.Fatalf("SimOf = %+v, ok=%v", sim, ok)
		}
	})

	t.Run("clock id without an instant is not simulated", func(t *testing.T) {
		ctx := WithSim(context.Background(), Sim{TestClockID: "tc_1"})
		if sim, ok := SimOf(ctx); ok {
			t.Errorf("expected not simulated, got %+v", sim)
		}
	})

	t.Run("instant without a clock id is not simulated", func(t *testing.T) {
		ctx := WithEffectiveNow(context.Background(), frozen)
		if sim, ok := SimOf(ctx); ok {
			t.Errorf("expected not simulated, got %+v", sim)
		}
		// ...but it IS still an effective-now binding: Clock.Now must honor it.
		if got, ok := EffectiveNow(ctx); !ok || !got.Equal(frozen) {
			t.Errorf("EffectiveNow = %v (ok=%v), want %v", got, ok, frozen)
		}
	})

	t.Run("unbound ctx", func(t *testing.T) {
		if _, ok := SimOf(context.Background()); ok {
			t.Error("expected not simulated")
		}
	})
}

// TestWithEffectiveNow_ClearsStaleClock: rebinding a BARE instant must drop any
// clock already on ctx. Carrying it forward would stamp rows with a clock the
// new instant does not belong to — e.g. a catchup-bound ctx (clock C, sim time)
// re-bound to wall-clock for an unpinned entity would keep claiming clock C.
func TestWithEffectiveNow_ClearsStaleClock(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	wall := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	ctx := WithSim(context.Background(), Sim{At: frozen, TestClockID: "tc_1"})
	ctx = WithEffectiveNow(ctx, wall)

	if sim, ok := SimOf(ctx); ok {
		t.Errorf("stale clock survived a bare rebind: %+v", sim)
	}
	if got, _ := EffectiveNow(ctx); !got.Equal(wall) {
		t.Errorf("EffectiveNow = %v, want %v", got, wall)
	}
}

// A ctx records one of three answers about its time: nothing decided, real
// time, or a test clock and its instant. Now reads machine time for the first
// two; only Decision tells them apart.
func TestDecision_RecordsOneOfThreeAnswers(t *testing.T) {
	bare := context.Background()
	if _, ok := Decision(bare); ok {
		t.Error("a bare ctx reports a decision")
	}

	realTime := WithRealTime(bare)
	if s, ok := Decision(realTime); !ok || s != (Sim{}) {
		t.Errorf("WithRealTime: Decision = %+v, %v; want the zero Sim, true", s, ok)
	}
	if _, ok := SimOf(realTime); ok {
		t.Error("WithRealTime: SimOf reports a clock")
	}
	before := time.Now().UTC()
	if got := Now(realTime); got.Before(before) || got.After(time.Now().UTC()) {
		t.Errorf("WithRealTime: Now = %v, want machine time", got)
	}

	frozen := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	onClock := WithSim(bare, Sim{At: frozen, TestClockID: "hc_tclk_1"})
	if s, ok := Decision(onClock); !ok || s.TestClockID != "hc_tclk_1" || !s.At.Equal(frozen) {
		t.Errorf("WithSim: Decision = %+v, %v", s, ok)
	}
}

// Decide records the resolver's answer and returns its errors: a clock binds
// the clock and its instant; an answer with no clock binds real time,
// whatever At it carries; a failed lookup, no resolver and an empty
// pin are errors that leave ctx as it was. It replaces an earlier decision,
// except a real instant for a record on no clock (TestDecide_EveryCase).
func TestDecide(t *testing.T) {
	frozen := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	pin := Pin{TenantID: "t1", CustomerID: "cus_1"}
	resolver := func(s Sim, err error) *stubResolver {
		return &stubResolver{custFunc: func() (Sim, error) { return s, err }}
	}

	got, err := Decide(context.Background(), resolver(Sim{At: frozen, TestClockID: "hc_tclk_1"}, nil), pin)
	if s, ok := SimOf(got); err != nil || !ok || s.TestClockID != "hc_tclk_1" || !s.At.Equal(frozen) {
		t.Errorf("a clock: SimOf = %+v, %v (err %v)", s, ok, err)
	}

	got, err = Decide(WithSim(context.Background(), Sim{At: frozen, TestClockID: "hc_tclk_old"}), resolver(Sim{At: frozen}, nil), pin)
	if s, ok := Decision(got); err != nil || !ok || s != (Sim{}) {
		t.Errorf("no clock: Decision = %+v, %v (err %v); want real time, replacing the earlier clock", s, ok, err)
	}

	lookup := errors.New("no such customer")
	for name, c := range map[string]struct {
		r Resolver
		p Pin
	}{
		"a failed lookup": {resolver(Sim{}, lookup), pin},
		"no resolver":     {nil, pin},
		"an empty pin":    {resolver(Sim{}, nil), Pin{TenantID: "t1"}},
	} {
		ctx := context.Background()
		got, err := Decide(ctx, c.r, c.p)
		if err == nil || got != ctx {
			t.Errorf("%s: err %v, ctx changed %v; want an error and ctx unchanged", name, err, got != ctx)
		}
	}
	if _, err := Decide(context.Background(), resolver(Sim{}, lookup), pin); !errors.Is(err, lookup) || !errors.Is(err, ErrUndecided) {
		t.Errorf("a failed lookup: err %v, want ErrUndecided wrapping the resolver's", err)
	}
}

// Every case of Decide and DecideOrRealTime, in test mode. Where TestDecide
// and TestDecideOrRealTime start from an empty context, this crosses each
// record (on a clock, on no clock, not found) with each state a context can
// carry: a clock and its instant, a real instant, a clock id with a zero
// instant (a partial binding), and nothing decided. A record's clock always
// wins; for a record on no clock (and DecideOrRealTime's not-found) a real
// instant is kept and anything else becomes real time with no instant;
// Decide refuses a not-found as ErrUndecided, leaving ctx as it is.
func TestDecide_EveryCase(t *testing.T) {
	frozen := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	inherited := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	test := postgres.WithLivemode(context.Background(), false)
	pin := Pin{TenantID: "t1", CustomerID: "cus_1"}
	notFound := fmt.Errorf("read customer cus_1: %w", errs.ErrNotFound)
	records := map[string]func() (Sim, error){
		"on a clock":  func() (Sim, error) { return Sim{At: frozen, TestClockID: "clk_rec"}, nil },
		"on no clock": func() (Sim, error) { return Sim{At: frozen}, nil },
		"not found":   func() (Sim, error) { return Sim{}, notFound },
		"unreadable":  func() (Sim, error) { return Sim{}, errors.New("connection reset by peer") },
	}
	contexts := map[string]context.Context{
		"a clock's instant":                    WithSim(test, Sim{At: inherited, TestClockID: "clk_old"}),
		"a real instant":                       WithEffectiveNow(test, inherited),
		"a partial binding":                    WithSim(test, Sim{TestClockID: "clk_old"}),
		"real time, no instant":                WithRealTime(test),
		"the same clock at an earlier instant": WithSim(test, Sim{At: frozen.Add(-72 * time.Hour), TestClockID: "clk_rec"}),
		"nothing":                              test,
	}
	recClock := Sim{At: frozen, TestClockID: "clk_rec"}
	kept, real := Sim{At: inherited}, Sim{}
	for _, c := range []struct {
		fn, record, ctx string
		want            Sim
		wantErr         bool
	}{
		{"Decide", "on a clock", "a clock's instant", recClock, false},
		{"Decide", "on a clock", "a real instant", recClock, false},
		{"Decide", "on a clock", "nothing", recClock, false},
		{"Decide", "on no clock", "a clock's instant", real, false},
		{"Decide", "on no clock", "a real instant", kept, false},
		{"Decide", "on no clock", "nothing", real, false},
		{"Decide", "on a clock", "a partial binding", recClock, false},
		{"Decide", "on no clock", "a partial binding", real, false},
		{"Decide", "not found", "a partial binding", Sim{}, true},
		{"Decide", "not found", "a clock's instant", Sim{}, true},
		{"Decide", "not found", "a real instant", Sim{}, true},
		{"Decide", "not found", "nothing", Sim{}, true},
		{"DecideOrRealTime", "on a clock", "a clock's instant", recClock, false},
		{"DecideOrRealTime", "on a clock", "a real instant", recClock, false},
		{"DecideOrRealTime", "on a clock", "nothing", recClock, false},
		{"DecideOrRealTime", "on no clock", "a clock's instant", real, false},
		{"DecideOrRealTime", "on no clock", "a real instant", kept, false},
		{"DecideOrRealTime", "on no clock", "nothing", real, false},
		{"DecideOrRealTime", "not found", "a clock's instant", real, false},
		{"DecideOrRealTime", "not found", "a real instant", kept, false},
		{"DecideOrRealTime", "not found", "nothing", real, false},
		{"DecideOrRealTime", "on a clock", "a partial binding", recClock, false},
		{"DecideOrRealTime", "on no clock", "a partial binding", real, false},
		{"DecideOrRealTime", "not found", "a partial binding", real, false},
		// Real time with no instant (WithRealTime): nothing to keep.
		{"Decide", "on a clock", "real time, no instant", recClock, false},
		{"Decide", "on no clock", "real time, no instant", real, false},
		{"DecideOrRealTime", "on a clock", "real time, no instant", recClock, false},
		{"DecideOrRealTime", "on no clock", "real time, no instant", real, false},
		{"Decide", "not found", "real time, no instant", Sim{}, true},
		{"DecideOrRealTime", "not found", "real time, no instant", real, false},
		// A clock at an earlier instant: the record's own clock is replaced
		// by its frozen time (as on main), and dropped for any other record.
		{"Decide", "on a clock", "the same clock at an earlier instant", recClock, false},
		{"Decide", "on no clock", "the same clock at an earlier instant", real, false},
		{"Decide", "not found", "the same clock at an earlier instant", Sim{}, true},
		{"DecideOrRealTime", "on a clock", "the same clock at an earlier instant", recClock, false},
		{"DecideOrRealTime", "on no clock", "the same clock at an earlier instant", real, false},
		{"DecideOrRealTime", "not found", "the same clock at an earlier instant", real, false},
		// A resolver error under a real instant: refused, ctx as it was.
		{"Decide", "unreadable", "a real instant", Sim{}, true},
		{"DecideOrRealTime", "unreadable", "a real instant", Sim{}, true},
	} {
		t.Run(c.fn+", the record "+c.record+", ctx carrying "+c.ctx, func(t *testing.T) {
			r := &stubResolver{custFunc: records[c.record]}
			ctx := contexts[c.ctx]
			var (
				got context.Context
				err error
			)
			if c.fn == "Decide" {
				got, err = Decide(ctx, r, pin)
			} else {
				got, err = DecideOrRealTime(ctx, r, pin)
			}
			if c.wantErr {
				if !errors.Is(err, ErrUndecided) || got != ctx {
					t.Errorf("err %v, ctx changed %v; want ErrUndecided and ctx as it was", err, got != ctx)
				}
				return
			}
			if err != nil {
				t.Fatalf("err %v", err)
			}
			if d, ok := Decision(got); !ok || !d.At.Equal(c.want.At) || d.TestClockID != c.want.TestClockID {
				t.Errorf("decision %+v (%v); want %+v", d, ok, c.want)
			}
		})
	}
}

// In live mode DecideOrRealTime decides nothing and reads nothing: a real
// instant the context carries stays as it is.
func TestDecideOrRealTime_LiveKeepsARealInstant(t *testing.T) {
	inherited := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	ctx := WithEffectiveNow(postgres.WithLivemode(context.Background(), true), inherited)
	r := &stubResolver{custFunc: func() (Sim, error) { return Sim{}, errors.New("must not be read") }}
	got, err := DecideOrRealTime(ctx, r, Pin{TenantID: "t1", CustomerID: "cus_1"})
	if err != nil || got != ctx {
		t.Errorf("err %v, ctx changed %v; want ctx as it was", err, got != ctx)
	}
}

// Work that did not come through the door decides in one order: no resolver
// decides nothing; no mode is refused; live decides nothing and reads
// nothing; test mode decides the record's clock, or fails as ErrUndecided
// over the resolver's error (a not-found among them).
func TestDecideWithoutDoor(t *testing.T) {
	frozen := time.Date(2027, 4, 2, 10, 0, 0, 0, time.UTC)
	notFound := fmt.Errorf("read test clock clk_w: %w", errs.ErrNotFound)
	test := postgres.WithLivemode(context.Background(), false)
	live := postgres.WithLivemode(context.Background(), true)
	pin := Pin{TenantID: "t1", CustomerID: "cus_1"}
	for _, c := range []struct {
		name      string
		ctx       context.Context
		answer    func() (Sim, error)
		noRes     bool
		wantReads int
		wantSim   *Sim // nil: nothing decided
		wantErr   []error
	}{
		{name: "no resolver: nothing", ctx: test, noRes: true},
		{name: "no mode: refused", ctx: context.Background(), wantErr: []error{ErrUndecided}},
		{name: "live: nothing, no read", ctx: live},
		{name: "test, a clock: its time", ctx: test, answer: func() (Sim, error) { return Sim{At: frozen, TestClockID: "clk_w"}, nil },
			wantReads: 1, wantSim: &Sim{At: frozen, TestClockID: "clk_w"}},
		{name: "test, no clock: real time", ctx: test, answer: func() (Sim, error) { return Sim{At: frozen}, nil },
			wantReads: 1, wantSim: &Sim{}},
		{name: "test, unreadable: ErrUndecided", ctx: test, answer: func() (Sim, error) { return Sim{}, errors.New("db down") },
			wantReads: 1, wantErr: []error{ErrUndecided}},
		{name: "test, not found: ErrUndecided over it", ctx: test, answer: func() (Sim, error) { return Sim{}, notFound },
			wantReads: 1, wantErr: []error{ErrUndecided, errs.ErrNotFound}},
	} {
		t.Run(c.name, func(t *testing.T) {
			reads := 0
			var r Resolver = &stubResolver{custFunc: func() (Sim, error) {
				reads++
				if c.answer == nil {
					return Sim{}, errors.New("not stubbed")
				}
				return c.answer()
			}}
			if c.noRes {
				r = nil
			}
			got, err := DecideWithoutDoor(c.ctx, r, pin)
			for _, want := range c.wantErr {
				if !errors.Is(err, want) {
					t.Errorf("err %v; want it to be %v", err, want)
				}
			}
			if len(c.wantErr) == 0 && err != nil {
				t.Errorf("err %v; want none", err)
			}
			if reads != c.wantReads {
				t.Errorf("%d resolver reads, want %d", reads, c.wantReads)
			}
			sim, decided := Decision(got)
			if c.wantSim == nil && decided || c.wantSim != nil && (!decided || sim != *c.wantSim) {
				t.Errorf("decided %v, %+v; want %v", decided, sim, c.wantSim)
			}
		})
	}
}

// DecideOrRealTime: the record's clock; a not-found is real time; any other
// failure is ErrUndecided; live mode and no resolver decide nothing.
func TestDecideOrRealTime(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	testMode := postgres.WithLivemode(context.Background(), false)
	answer := func(s Sim, err error) *stubResolver {
		return &stubResolver{custFunc: func() (Sim, error) { return s, err }}
	}
	pin := Pin{TenantID: "t1", CustomerID: "cus_1"}
	for _, c := range []struct {
		name      string
		ctx       context.Context
		r         Resolver
		wantClock string
		wantReal  bool
		wantErr   bool
	}{
		{"the record's clock", testMode, answer(Sim{At: frozen, TestClockID: "clk_1"}, nil), "clk_1", false, false},
		{"a record on no clock is real time", testMode, answer(Sim{At: frozen}, nil), "", true, false},
		{"a not-found is real time", testMode, answer(Sim{}, fmt.Errorf("read test clock clk_gone: %w", errs.ErrNotFound)), "", true, false},
		{"any other failure is undecided", testMode, answer(Sim{}, errors.New("connection reset by peer")), "", false, true},
		{"live mode decides nothing", postgres.WithLivemode(context.Background(), true), answer(Sim{}, errors.New("must not be read")), "", false, false},
		{"no resolver decides nothing", testMode, nil, "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, err := DecideOrRealTime(c.ctx, c.r, pin)
			if c.wantErr != (err != nil) || (c.wantErr && !errors.Is(err, ErrUndecided)) {
				t.Fatalf("err %v; want error %v (ErrUndecided)", err, c.wantErr)
			}
			if sim, _ := SimOf(ctx); sim.TestClockID != c.wantClock {
				t.Errorf("clock %q; want %q", sim.TestClockID, c.wantClock)
			}
			if d, ok := Decision(ctx); c.wantReal && (!ok || !d.At.IsZero() || d.TestClockID != "") {
				t.Errorf("decision %+v (%v); want real time", d, ok)
			}
		})
	}
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

// EnsureEffectiveNow keeps the instant ctx carries, a clock's decision or a
// bare instant, and reads no clock; otherwise, real time decided or nothing
// decided, it reads the clock once and binds that instant.
func TestEnsureEffectiveNow(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	server := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name      string
		ctx       context.Context
		want      time.Time
		wantClock string
		wantReads int
	}{
		{"a clock's decision is kept", WithSim(testMode, Sim{At: frozen, TestClockID: "clk_1"}), frozen, "clk_1", 0},
		{"a bare instant is kept", WithEffectiveNow(testMode, frozen), frozen, "", 0},
		{"real time is read once and bound", WithRealTime(testMode), server, "", 1},
		{"nothing decided is read once and bound", testMode, server, "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			cc := &countingClock{at: server}
			got := EnsureEffectiveNow(c.ctx, cc)
			at, ok := EffectiveNow(got)
			sim, _ := SimOf(got)
			if !ok || !at.Equal(c.want) || sim.TestClockID != c.wantClock || cc.reads != c.wantReads {
				t.Errorf("instant %v (bound %v), clock %q, reads %d; want %v, clock %q, %d reads", at, ok, sim.TestClockID, cc.reads, c.want, c.wantClock, c.wantReads)
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
		{"nothing decided", testMode},
		{"real time with no instant", WithRealTime(testMode)},
		{"the action's real instant", WithEffectiveNow(testMode, action)},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := NewFake(arrived)
			got := AtArrival(c.ctx, server)
			server.Advance(time.Hour) // a later read must answer the bound instant
			d, ok := Decision(got)
			if later := server.Now(got); !ok || !d.At.Equal(arrived) || d.TestClockID != "" || !later.Equal(arrived) {
				t.Errorf("decision %+v (%v), later read %v; want a real instant at %v", d, ok, later, arrived)
			}
		})
	}
}
