package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/tax"
)

// fakeCalcStore is a minimal TaxCalculationWriter for a calculation recorded
// at createdAt whose response names no expiry, so the CommitTax expiry guard
// can be exercised without postgres. usableUntil, when set, is the
// calculation's own expiry instead.
type fakeCalcStore struct {
	createdAt   time.Time
	usableUntil time.Time
	lookedUp    bool
}

func (f *fakeCalcStore) Record(_ context.Context, _, _ string, _ tax.Request, _ *tax.Result) (string, error) {
	return "taxcalc_fake", nil
}

func (f *fakeCalcStore) LinkInvoice(_ context.Context, _, _, _ string) error { return nil }

func (f *fakeCalcStore) LookupCalculationUsableUntil(_ context.Context, _, _, _ string) (time.Time, error) {
	f.lookedUp = true
	if !f.usableUntil.IsZero() {
		return f.usableUntil, nil
	}
	return f.createdAt.Add(tax.CalculationFallbackLifetime), nil
}

// TestCommitTax_ExpiryGuardIgnoresSimulatedClock locks the fix for the
// clock-pinned proration orphan: a calc created seconds ago in REAL time must
// commit even when the request ctx carries a far-future test-clock
// effective-now. Pre-fix the guard used clock.Now(ctx) (the simulated clock),
// so a customer whose test clock was advanced >23h past wall-clock produced a
// false "tax calculation expired" — the commit was skipped and the proration
// invoice kept tax_calculation_id but never got tax_transaction_id.
func TestCommitTax_ExpiryGuardIgnoresSimulatedClock(t *testing.T) {
	store := &fakeCalcStore{createdAt: time.Now().UTC()} // created just now, real time
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(&stubProvider{}), // Commit -> ("", nil): no SetTaxTransaction needed
		taxCalcStore: store,
	}
	// Simulate a customer pinned to a test clock advanced 30 days past real
	// wall-clock; clock.Now(ctx) would return this far-future time.
	ctx := clock.WithEffectiveNow(context.Background(), time.Now().UTC().Add(30*24*time.Hour))

	if err := e.CommitTax(ctx, "t1", "inv_1", "taxcalc_1"); err != nil {
		t.Fatalf("commit of a fresh calc must not be blocked by an advanced test clock: %v", err)
	}
	if !store.lookedUp {
		t.Fatal("expiry guard never ran (lookup not called) — assertion would be vacuous")
	}
}

// TestCommitTax_ExpiryGuardTripsOnRealAge is the over-correction guard: the
// fix must not disable expiry. A calc genuinely 30h old in real time must be
// rejected regardless of where the simulated clock sits.
func TestCommitTax_ExpiryGuardTripsOnRealAge(t *testing.T) {
	store := &fakeCalcStore{createdAt: time.Now().UTC().Add(-30 * time.Hour)} // real-stale
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(&stubProvider{}),
		taxCalcStore: store,
	}
	// Simulated clock pinned to "now" — the calc is still genuinely expired.
	ctx := clock.WithEffectiveNow(context.Background(), time.Now().UTC())

	err := e.CommitTax(ctx, "t1", "inv_1", "taxcalc_1")
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("a genuinely expired calc must be rejected with ErrInvalidState, got %v", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("want an expiry message, got %v", err)
	}
}

// TestCheckTaxCalculationUsable_DirectContract pins the standalone check —
// the method invoice.Service.Finalize consults BEFORE the state transition.
// Expired → InvalidState carrying the stable machine code; fresh → nil;
// uncertainty (unwired store / nil provider) → nil, because only a PROVEN
// expiry may block a finalize.
func TestCheckTaxCalculationUsable_DirectContract(t *testing.T) {
	t.Run("expired calc blocks with the stable code", func(t *testing.T) {
		e := &Engine{
			settings:     &taxSettings{provider: "stripe_tax"},
			taxProviders: stubResolver(&stubProvider{}),
			taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC().Add(-30 * time.Hour)},
		}
		err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1")
		if !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("want ErrInvalidState, got %v", err)
		}
		var de *errs.DomainError
		if !errors.As(err, &de) || de.Code != "tax_calculation_expired" {
			t.Fatalf("want stable code tax_calculation_expired, got %+v", err)
		}
		if !strings.Contains(err.Error(), "retry tax to refresh, then finalize") {
			t.Fatalf("the message must carry the operator remedy, got %v", err)
		}
	})
	t.Run("fresh calc passes", func(t *testing.T) {
		e := &Engine{
			settings:     &taxSettings{provider: "stripe_tax"},
			taxProviders: stubResolver(&stubProvider{}),
			taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC()},
		}
		if err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1"); err != nil {
			t.Fatalf("fresh calc must pass: %v", err)
		}
	})
	t.Run("unwired store is uncertainty, not a block", func(t *testing.T) {
		e := &Engine{settings: &taxSettings{provider: "stripe_tax"}, taxProviders: stubResolver(&stubProvider{})}
		if err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1"); err != nil {
			t.Fatalf("no calc store wired must pass: %v", err)
		}
	})
	t.Run("nil provider skips — an aged calc must not block a tenant whose commit would skip too", func(t *testing.T) {
		e := &Engine{
			settings:     &taxSettings{provider: "none"},
			taxProviders: stubResolver(nil),
			taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC().Add(-30 * time.Hour)},
		}
		if err := e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_1"); err != nil {
			t.Fatalf("nil provider must pass even with an aged calc: %v", err)
		}
	})
}

// A calculation is usable until its own expiry, which for Stripe Tax is 90
// days after it was made. The guard used a fixed 23 hours from a mistaken
// reading of Stripe's limit, so a commit that had failed for more than a day
// was refused by Hardcount although Stripe would still have accepted it, and
// the invoice's tax was never reported.
func TestCommitTax_ExpiryGuardUsesTheCalculationsOwnExpiry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		usableUntil time.Duration // from now
		wantExpired bool
	}{
		{"made three days ago, expires in 87 days: usable", 87 * 24 * time.Hour, false},
		{"expires in two hours: usable", 2 * time.Hour, false},
		{"expires in half an hour: inside the margin, refused", 30 * time.Minute, true},
		{"expired yesterday: refused", -24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{
				settings:     &taxSettings{provider: "stripe_tax"},
				taxProviders: stubResolver(&stubProvider{}),
				// created 3 days ago either way: the age no longer decides.
				taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC().Add(-72 * time.Hour), usableUntil: time.Now().UTC().Add(tc.usableUntil)},
			}
			err := e.CommitTax(context.Background(), "t1", "inv_1", "taxcalc_1")
			if tc.wantExpired != errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("CommitTax = %v, want expired=%v", err, tc.wantExpired)
			}
		})
	}
}
