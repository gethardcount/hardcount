package clock

import (
	"context"
	"errors"
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

// TestBindEffectiveNow covers the resolver-driven binding helper used
// at every operator entry point. Verifies the precedence (invoice →
// sub → customer), the resolver-error fallback, and the no-resolver
// shortcut.
func TestBindEffectiveNow(t *testing.T) {
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)

	t.Run("nil resolver returns ctx unchanged", func(t *testing.T) {
		base := context.Background()
		ctx, ok := BindEffectiveNow(base, nil, Pin{TenantID: "t1", CustomerID: "cus_1"})
		if ok {
			t.Error("expected ok=false")
		}
		if _, bound := EffectiveNow(ctx); bound {
			t.Error("expected ctx to remain unbound")
		}
	})

	t.Run("invoice id wins over sub and customer", func(t *testing.T) {
		stub := &stubResolver{
			invoiceFunc: func() (Sim, error) { return Sim{At: frozen, TestClockID: "tc_1"}, nil },
			subFunc:     func() (Sim, error) { return Sim{}, errors.New("should not be called") },
			custFunc:    func() (Sim, error) { return Sim{}, errors.New("should not be called") },
		}
		ctx, ok := BindEffectiveNow(context.Background(), stub, Pin{
			TenantID: "t1", InvoiceID: "inv_1", SubscriptionID: "sub_1", CustomerID: "cus_1",
		})
		if !ok {
			t.Fatal("expected ok=true")
		}
		got, _ := EffectiveNow(ctx)
		if !got.Equal(frozen) {
			t.Errorf("got %v, want %v", got, frozen)
		}
		// The pin resolution carries the CLOCK, not just the instant — this is
		// what makes audit sim-axis stamping total (ADR-090 §5).
		sim, ok := SimOf(ctx)
		if !ok || sim.TestClockID != "tc_1" || !sim.At.Equal(frozen) {
			t.Errorf("SimOf = %+v (ok=%v), want {At: %v, TestClockID: tc_1}", sim, ok, frozen)
		}
	})

	t.Run("sub id wins over customer when no invoice", func(t *testing.T) {
		stub := &stubResolver{
			subFunc:  func() (Sim, error) { return Sim{At: frozen, TestClockID: "tc_1"}, nil },
			custFunc: func() (Sim, error) { return Sim{}, errors.New("should not be called") },
		}
		ctx, ok := BindEffectiveNow(context.Background(), stub, Pin{
			TenantID: "t1", SubscriptionID: "sub_1", CustomerID: "cus_1",
		})
		if !ok {
			t.Fatal("expected ok=true")
		}
		got, _ := EffectiveNow(ctx)
		if !got.Equal(frozen) {
			t.Errorf("got %v, want %v", got, frozen)
		}
		// The pin resolution carries the CLOCK, not just the instant — this is
		// what makes audit sim-axis stamping total (ADR-090 §5).
		sim, ok := SimOf(ctx)
		if !ok || sim.TestClockID != "tc_1" || !sim.At.Equal(frozen) {
			t.Errorf("SimOf = %+v (ok=%v), want {At: %v, TestClockID: tc_1}", sim, ok, frozen)
		}
	})

	t.Run("customer id is the final fallback", func(t *testing.T) {
		stub := &stubResolver{
			custFunc: func() (Sim, error) { return Sim{At: frozen, TestClockID: "tc_1"}, nil },
		}
		ctx, ok := BindEffectiveNow(context.Background(), stub, Pin{
			TenantID: "t1", CustomerID: "cus_1",
		})
		if !ok {
			t.Fatal("expected ok=true")
		}
		got, _ := EffectiveNow(ctx)
		if !got.Equal(frozen) {
			t.Errorf("got %v, want %v", got, frozen)
		}
		// The pin resolution carries the CLOCK, not just the instant — this is
		// what makes audit sim-axis stamping total (ADR-090 §5).
		sim, ok := SimOf(ctx)
		if !ok || sim.TestClockID != "tc_1" || !sim.At.Equal(frozen) {
			t.Errorf("SimOf = %+v (ok=%v), want {At: %v, TestClockID: tc_1}", sim, ok, frozen)
		}
	})

	t.Run("resolver error returns ctx unchanged", func(t *testing.T) {
		stub := &stubResolver{
			custFunc: func() (Sim, error) { return Sim{}, errors.New("dangling pin") },
		}
		ctx, ok := BindEffectiveNow(context.Background(), stub, Pin{
			TenantID: "t1", CustomerID: "cus_missing",
		})
		if ok {
			t.Error("expected ok=false on resolver error")
		}
		if _, bound := EffectiveNow(ctx); bound {
			t.Error("expected ctx to remain unbound on resolver error")
		}
	})

	t.Run("no ids returns ctx unchanged", func(t *testing.T) {
		stub := &stubResolver{}
		ctx, ok := BindEffectiveNow(context.Background(), stub, Pin{TenantID: "t1"})
		if ok {
			t.Error("expected ok=false")
		}
		if _, bound := EffectiveNow(ctx); bound {
			t.Error("expected ctx to remain unbound")
		}
	})
}

// TestBindEffectiveNow_NeverDowngradesAnInheritedBinding pins the guard that
// keeps sim-axis stamping TOTAL under a catchup.
//
// Every resolver falls back to Sim{At: clock.Now(ctx)} with an EMPTY clock id
// and a NIL error when it cannot resolve its pin. Under a catchup ctx that
// value is poison: Now(ctx) reads the binding, so the fallback is {simulated
// instant, no clock} — and binding it would keep the simulated instant while
// erasing the clock. Every audit row emitted below that call would silently
// drop off the sim axis, which is the failure the operator can never see: rows
// missing from ?test_clock_id= look exactly like rows that were never written.
//
// Services re-bind under catchup all the time (dunning, payment reconciler,
// invoice, subscription), so this is the common path, not an edge case.
func TestBindEffectiveNow_NeverDowngradesAnInheritedBinding(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	catchup := WithSim(context.Background(), Sim{At: frozen, TestClockID: "hc_tclk_1"})

	// The resolver's unresolvable-pin fallback, reproduced exactly: the instant
	// it reads back from ctx IS the simulated one, and the clock id is empty.
	lost := &stubResolver{subFunc: func() (Sim, error) {
		return Sim{At: Now(catchup)}, nil // nil error — this is the trap
	}}

	got, ok := BindEffectiveNow(catchup, lost, Pin{TenantID: "t1", SubscriptionID: "sub_dangling"})
	if !ok {
		t.Fatal("bind reported no effective-now, but the ctx carries the catchup's")
	}
	sim, bound := SimOf(got)
	if !bound {
		t.Fatal("the catchup's clock binding was ERASED by a failed nested resolution — every audit row below this call just fell off the sim axis")
	}
	if sim.TestClockID != "hc_tclk_1" {
		t.Errorf("clock id = %q, want the inherited hc_tclk_1", sim.TestClockID)
	}
	if !sim.At.Equal(frozen) {
		t.Errorf("instant = %s, want the inherited %s", sim.At, frozen)
	}

	// The guard must not become a ratchet: a resolution that DOES carry a clock
	// still wins, so a nested bind can still refine the domain.
	other := &stubResolver{subFunc: func() (Sim, error) {
		return Sim{At: frozen.AddDate(0, 1, 0), TestClockID: "hc_tclk_2"}, nil
	}}
	got2, _ := BindEffectiveNow(catchup, other, Pin{TenantID: "t1", SubscriptionID: "sub_other"})
	if sim2, _ := SimOf(got2); sim2.TestClockID != "hc_tclk_2" {
		t.Errorf("a resolution carrying a real clock must win: got %q, want hc_tclk_2", sim2.TestClockID)
	}

	// And on a plain wall-clock request there is nothing to inherit, so an
	// unpinned resolution still binds its instant as it always did.
	wall := &stubResolver{custFunc: func() (Sim, error) {
		return Sim{At: frozen}, nil
	}}
	got3, ok3 := BindEffectiveNow(context.Background(), wall, Pin{TenantID: "t1", CustomerID: "cus_1"})
	if !ok3 {
		t.Fatal("an unpinned resolution on an unbound ctx must still bind effective-now")
	}
	if !Now(got3).Equal(frozen) {
		t.Errorf("effective-now = %s, want %s", Now(got3), frozen)
	}
	if _, isSim := SimOf(got3); isSim {
		t.Error("an unpinned resolution must not report a simulation")
	}
}

// TestBindEffectiveNow_KeepsAContractedInstantOnTheSameClock pins "bind at
// the boundary, inherit below it" (ADR-029) for a deliberate rebind. A caller
// that bound an earlier instant on a clock (a cycle close at its boundary, a
// dunning retry at its slot, a trial flip at trial_end_at) must keep it
// through a nested resolution of the same clock: that resolution can only
// answer frozen_time, so taking it dated every effect below the rebind at the
// advance's target (P-5 in the activity line standard).
func TestBindEffectiveNow_KeepsAContractedInstantOnTheSameClock(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	slot := frozen.Add(-9 * 24 * time.Hour)
	resolvesTo := func(s Sim) *stubResolver {
		return &stubResolver{invoiceFunc: func() (Sim, error) { return s, nil }}
	}
	pin := Pin{TenantID: "t1", InvoiceID: "inv_1"}

	cases := []struct {
		name     string
		inherit  Sim
		resolved Sim
		want     Sim
	}{
		{"same clock, contracted instant before frozen: kept",
			Sim{At: slot, TestClockID: "hc_tclk_1"}, Sim{At: frozen, TestClockID: "hc_tclk_1"}, Sim{At: slot, TestClockID: "hc_tclk_1"}},
		{"same clock, same instant: kept",
			Sim{At: frozen, TestClockID: "hc_tclk_1"}, Sim{At: frozen, TestClockID: "hc_tclk_1"}, Sim{At: frozen, TestClockID: "hc_tclk_1"}},
		{"same clock, inherited instant after frozen: replaced",
			Sim{At: frozen.Add(time.Hour), TestClockID: "hc_tclk_1"}, Sim{At: frozen, TestClockID: "hc_tclk_1"}, Sim{At: frozen, TestClockID: "hc_tclk_1"}},
		{"different clock: the resolution wins",
			Sim{At: slot, TestClockID: "hc_tclk_1"}, Sim{At: frozen, TestClockID: "hc_tclk_2"}, Sim{At: frozen, TestClockID: "hc_tclk_2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := BindEffectiveNow(WithSim(context.Background(), c.inherit), resolvesTo(c.resolved), pin)
			if !ok {
				t.Fatal("bind reported no effective-now")
			}
			sim, bound := SimOf(got)
			if !bound || sim.TestClockID != c.want.TestClockID || !sim.At.Equal(c.want.At) {
				t.Errorf("bound %+v (simulated=%v), want %+v", sim, bound, c.want)
			}
		})
	}

	// A bare instant (no clock) is not a contracted one: the resolution wins.
	got, _ := BindEffectiveNow(WithEffectiveNow(context.Background(), slot), resolvesTo(Sim{At: frozen, TestClockID: "hc_tclk_1"}), pin)
	if sim, ok := SimOf(got); !ok || !sim.At.Equal(frozen) {
		t.Errorf("under a bare WithEffectiveNow: bound %+v (simulated=%v), want the resolved clock at %v", sim, ok, frozen)
	}
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
