package domain

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

// computeInt is a test-only convenience wrapping ComputeAmountCents with
// an int64 quantity input. The production signature takes decimal.Decimal
// (fractional usage is supported); these table tests still express
// quantity as int for readability so we wrap once at the call site.
func computeInt(rule PriceShape, n int64) (int64, error) {
	return ComputeAmountCents(rule, decimal.NewFromInt(n))
}

func TestComputeAmountCents_Flat(t *testing.T) {
	rule := PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(500)}

	tests := []struct {
		name    string
		qty     int64
		wantAmt int64
		wantErr bool
	}{
		{"zero quantity", 0, 0, false},
		{"positive quantity", 100, 50000, false},
		{"negative quantity", -1, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeInt(rule, tt.qty)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.wantAmt {
				t.Errorf("got %d, want %d", got, tt.wantAmt)
			}
		})
	}
}

// TestComputeAmountCents_SubCentFlatRate locks the ADR-045 motivating case:
// a sub-cent-per-unit decimal rate ($3.00 / 1,000,000 tokens = 0.0003
// cents/token) prices linearly and exactly — integer cents could not.
func TestComputeAmountCents_SubCentFlatRate(t *testing.T) {
	rule := PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0003")}
	cases := []struct {
		qty  int64
		want int64
	}{
		{1_000_000, 300}, // 1M tokens @ $3/M = 300c
		{500_000, 150},   // exact half — no per-million block leakage
		{1, 0},           // a single token rounds to 0c (line amount), but...
		{3333, 1},        // 3333 * 0.0003 = 0.9999c -> banker's-rounds to 1c
	}
	for _, c := range cases {
		got, err := computeInt(rule, c.qty)
		if err != nil {
			t.Fatalf("qty %d: unexpected err %v", c.qty, err)
		}
		if got != c.want {
			t.Errorf("qty %d @ 0.0003c: got %dc, want %dc", c.qty, got, c.want)
		}
	}
}

// TestComputeAmountCents_RejectsMisorderedCatchAllTiers guards the ADR-045
// hardening: a catch-all (UpTo==0) tier must be last and unique, else later
// tiers are dead config and overflow quantity would silently price wrong.
func TestComputeAmountCents_RejectsMisorderedCatchAllTiers(t *testing.T) {
	for _, name := range []string{"two catch-alls", "catch-all not last"} {
		tiers := []RatingTier{
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(5)},
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(1)},
		}
		if name == "catch-all not last" {
			tiers = []RatingTier{
				{UpTo: nil, UnitAmountCents: decimal.NewFromInt(5)},
				{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(1)},
			}
		}
		rule := PriceShape{Mode: PricingGraduated, Tiers: tiers}
		if _, err := computeInt(rule, 150); err == nil {
			t.Errorf("%s: expected ErrInvalidPricingConfig, got nil", name)
		}
	}
}

func TestComputeAmountCents_Graduated(t *testing.T) {
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(10)), UnitAmountCents: decimal.NewFromInt(100)},
			{UpTo: new(int64(50)), UnitAmountCents: decimal.NewFromInt(50)},
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(25)}, // unlimited
		},
	}

	tests := []struct {
		name    string
		qty     int64
		wantAmt int64
	}{
		{"within first tier", 5, 500},
		{"exact first tier boundary", 10, 1000},
		{"into second tier", 20, 1000 + 10*50},           // 1500
		{"exact second tier boundary", 50, 1000 + 40*50}, // 3000
		{"into overflow tier", 60, 1000 + 40*50 + 10*25}, // 3250
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeInt(rule, tt.qty)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantAmt {
				t.Errorf("got %d, want %d", got, tt.wantAmt)
			}
		})
	}
}

func TestComputeAmountCents_Package(t *testing.T) {
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        100,
		PackageAmountCents: decimal.NewFromInt(2000),
	}

	tests := []struct {
		name    string
		qty     int64
		wantAmt int64
	}{
		{"zero", 0, 0},
		{"one unit is a whole package", 1, 2000},
		{"one full package", 100, 2000},
		{"a part package bills as a whole one", 150, 4000},
		{"two packages", 200, 4000},
		{"one unit into the third package", 201, 6000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeInt(rule, tt.qty)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantAmt {
				t.Errorf("got %d, want %d", got, tt.wantAmt)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Edge case: flat pricing — overflow, negative unit price, large quantity
// ---------------------------------------------------------------------------

func TestComputeAmountCents_Flat_NegativeUnitPrice(t *testing.T) {
	rule := PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(-1)}
	_, err := computeInt(rule, 10)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for negative flat price, got %v", err)
	}
}

func TestComputeAmountCents_Flat_LargeQuantityOverflowCheck(t *testing.T) {
	// Verify large quantity * unit price doesn't silently overflow.
	// With UnitAmountCents=1000 ($10) and quantity near int64 max / 1000,
	// the multiplication should still produce a valid result.
	rule := PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1000)}
	qty := int64(math.MaxInt64 / 1000) // largest safe quantity
	got, err := computeInt(rule, qty)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := qty * 1000
	if got != expected {
		t.Errorf("got %d, want %d", got, expected)
	}
}

func TestComputeAmountCents_Flat_OverflowDetected(t *testing.T) {
	// Pathological input: qty * unit would wrap int64. Must be rejected,
	// not silently produce a negative invoice line.
	rule := PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(2)}
	_, err := computeInt(rule, math.MaxInt64)
	if err != ErrAmountOverflow {
		t.Fatalf("expected ErrAmountOverflow, got %v", err)
	}
}

func TestComputeAmountCents_Graduated_OverflowDetected(t *testing.T) {
	// Catch-all tier with huge unit price and huge quantity — multiply wraps.
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(math.MaxInt64 / 2)},
		},
	}
	_, err := computeInt(rule, 10)
	if err != ErrAmountOverflow {
		t.Fatalf("expected ErrAmountOverflow in catch-all tier, got %v", err)
	}
}

func TestComputeAmountCents_Graduated_OverflowOnSum(t *testing.T) {
	// First tier fills up; second tier addition would overflow the running sum.
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(1)), UnitAmountCents: decimal.NewFromInt(math.MaxInt64 - 5)},
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(10)},
		},
	}
	_, err := computeInt(rule, 2)
	if err != ErrAmountOverflow {
		t.Fatalf("expected ErrAmountOverflow on tier sum, got %v", err)
	}
}

func TestComputeAmountCents_Package_OverflowDetected(t *testing.T) {
	// Package multiply wraps: packages * PackageAmountCents.
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        1,
		PackageAmountCents: decimal.NewFromInt(math.MaxInt64 / 2),
	}
	_, err := computeInt(rule, 10)
	if err != ErrAmountOverflow {
		t.Fatalf("expected ErrAmountOverflow on package multiply, got %v", err)
	}
}

func TestComputeAmountCents_Package_OverflowOnPartPackage(t *testing.T) {
	// One package fits int64; the part package rounds up to a second one,
	// which does not.
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        2,
		PackageAmountCents: decimal.NewFromInt(math.MaxInt64/2 + 1),
	}
	if _, err := computeInt(rule, 2); err != nil {
		t.Fatalf("one package: %v", err)
	}
	_, err := computeInt(rule, 3)
	if err != ErrAmountOverflow {
		t.Fatalf("expected ErrAmountOverflow on the part package, got %v", err)
	}
}

func TestComputeAmountCents_Flat_ZeroUnitPrice(t *testing.T) {
	// Free tier: 0 cents per unit
	rule := PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(0)}
	got, err := computeInt(rule, 999999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("expected 0 for free tier, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Edge case: graduated pricing — boundary, insufficient tiers, bad config
// ---------------------------------------------------------------------------

func TestComputeAmountCents_Graduated_ExactTierBoundary(t *testing.T) {
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(50)},
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(25)}, // catch-all
		},
	}
	// Exactly at the first tier boundary
	got, err := computeInt(rule, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 100*50 {
		t.Errorf("got %d, want %d", got, 100*50)
	}
}

func TestComputeAmountCents_Graduated_OnlyUnlimitedTier(t *testing.T) {
	// Single tier with UpTo=0 (catch-all) handles any quantity
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(10)},
		},
	}
	got, err := computeInt(rule, 5000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5000*10 {
		t.Errorf("got %d, want %d", got, 5000*10)
	}
}

func TestComputeAmountCents_Graduated_InsufficientTiers(t *testing.T) {
	// Tiers only cover up to 100, but quantity is 200 and no catch-all
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(50)},
		},
	}
	_, err := computeInt(rule, 200)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for insufficient tiers, got %v", err)
	}
}

func TestComputeAmountCents_Graduated_EmptyTiers(t *testing.T) {
	rule := PriceShape{
		Mode:  PricingGraduated,
		Tiers: nil,
	}
	_, err := computeInt(rule, 10)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for empty tiers, got %v", err)
	}
}

func TestComputeAmountCents_Graduated_NegativeUnitPrice(t *testing.T) {
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(-5)},
		},
	}
	_, err := computeInt(rule, 10)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for negative unit price in tier, got %v", err)
	}
}

func TestComputeAmountCents_Graduated_NegativeUpTo(t *testing.T) {
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(-10)), UnitAmountCents: decimal.NewFromInt(50)},
		},
	}
	_, err := computeInt(rule, 5)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for negative UpTo, got %v", err)
	}
}

func TestComputeAmountCents_Graduated_ZeroQuantity(t *testing.T) {
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(50)},
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(25)},
		},
	}
	got, err := computeInt(rule, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("expected 0 for zero quantity, got %d", got)
	}
}

func TestComputeAmountCents_Graduated_ThreeTiersExactBoundaries(t *testing.T) {
	// Tier 1: 0-10 @ 100, Tier 2: 11-50 @ 50, Tier 3: 51+ @ 25
	rule := PriceShape{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(10)), UnitAmountCents: decimal.NewFromInt(100)},
			{UpTo: new(int64(50)), UnitAmountCents: decimal.NewFromInt(50)},
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(25)},
		},
	}
	tests := []struct {
		name    string
		qty     int64
		wantAmt int64
	}{
		{"1 unit", 1, 100},
		{"10 units (end of tier 1)", 10, 1000},
		{"11 units (first in tier 2)", 11, 1000 + 50},
		{"50 units (end of tier 2)", 50, 1000 + 40*50},
		{"51 units (first in tier 3)", 51, 1000 + 40*50 + 25},
		{"1000 units (deep in tier 3)", 1000, 1000 + 40*50 + 950*25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeInt(rule, tt.qty)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantAmt {
				t.Errorf("got %d, want %d", got, tt.wantAmt)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Edge case: package pricing
// ---------------------------------------------------------------------------

func TestComputeAmountCents_Package_ExactMultiple(t *testing.T) {
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        50,
		PackageAmountCents: decimal.NewFromInt(1000),
	}
	// 150 = 3 * 50, no overage
	got, err := computeInt(rule, 150)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 3*1000 {
		t.Errorf("got %d, want %d", got, 3*1000)
	}
}

func TestComputeAmountCents_Package_SingleUnitLargePackage(t *testing.T) {
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        1000,
		PackageAmountCents: decimal.NewFromInt(5000),
	}
	// qty=1: a part package bills as a whole one
	got, err := computeInt(rule, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5000 {
		t.Errorf("got %d, want %d (one whole package)", got, 5000)
	}
}

func TestComputeAmountCents_Package_ZeroPackageSize(t *testing.T) {
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        0,
		PackageAmountCents: decimal.NewFromInt(1000),
	}
	_, err := computeInt(rule, 10)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for zero package size, got %v", err)
	}
}

func TestComputeAmountCents_Package_NegativePackageSize(t *testing.T) {
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        -5,
		PackageAmountCents: decimal.NewFromInt(1000),
	}
	_, err := computeInt(rule, 10)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for negative package size, got %v", err)
	}
}

func TestComputeAmountCents_Package_NegativePackagePrice(t *testing.T) {
	rule := PriceShape{
		Mode:               PricingPackage,
		PackageSize:        100,
		PackageAmountCents: decimal.NewFromInt(-500),
	}
	_, err := computeInt(rule, 10)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for negative package price, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Edge case: unknown pricing mode
// ---------------------------------------------------------------------------

func TestComputeAmountCents_UnknownMode(t *testing.T) {
	rule := PriceShape{Mode: "volume"}
	_, err := computeInt(rule, 10)
	if err != ErrInvalidPricingConfig {
		t.Fatalf("expected ErrInvalidPricingConfig for unknown mode, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Edge case: negative quantity for all modes
// ---------------------------------------------------------------------------

func TestComputeAmountCents_NegativeQuantity_AllModes(t *testing.T) {
	modes := []struct {
		name string
		rule PriceShape
	}{
		{"per_unit", PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(100)}},
		{"graduated", PriceShape{
			Mode:  PricingGraduated,
			Tiers: []RatingTier{{UpTo: nil, UnitAmountCents: decimal.NewFromInt(10)}},
		}},
		{"package", PriceShape{
			Mode: PricingPackage, PackageSize: 10, PackageAmountCents: decimal.NewFromInt(100),
		}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			_, err := computeInt(m.rule, -1)
			if err != ErrInvalidPricingConfig {
				t.Fatalf("expected ErrInvalidPricingConfig for negative quantity in %s mode, got %v", m.name, err)
			}
		})
	}
}

// TestComputeAmountCents_Graduated_EmptyTiers_Validation verifies that
// an unmarshal failure on tiers (empty list) is caught at billing time.
func TestComputeAmountCents_Graduated_EmptyTiers_Validation(t *testing.T) {
	// This simulates what happens if json.Unmarshal in scanRatingRule fails
	// silently and leaves Tiers as an empty slice.
	emptyRule := PriceShape{
		Mode:  PricingGraduated,
		Tiers: []RatingTier{}, // Empty because unmarshal failed
	}

	qty := decimal.NewFromInt(100)
	_, err := ComputeAmountCents(emptyRule, qty)

	if err != ErrInvalidPricingConfig {
		t.Errorf("expected ErrInvalidPricingConfig for empty tiers, got %v", err)
	}
}

// PriceShape.Equal compares amounts as numbers and tiers bound by bound.
func TestPriceShape_Equal(t *testing.T) {
	ten, eleven := int64(10), int64(11)
	grad := func(upTo *int64, above string) PriceShape {
		return PriceShape{Mode: PricingGraduated, Tiers: RatingTiers{{UpTo: upTo, UnitAmountCents: decimal.Zero}, {UnitAmountCents: decimal.RequireFromString(above)}}}
	}
	unit := func(c string) PriceShape {
		return PriceShape{Mode: PricingPerUnit, UnitAmountCents: decimal.RequireFromString(c)}
	}
	for _, c := range []struct {
		name string
		a, b PriceShape
		want bool
	}{
		{"same digits", unit("1"), unit("1"), true},
		{"other digits, same number", unit("1"), unit("1.000"), true},
		{"other rate", unit("1"), unit("2"), false},
		{"other mode", unit("1"), PriceShape{Mode: PricingPackage, PackageSize: 1, PackageAmountCents: decimal.NewFromInt(1)}, false},
		{"same tiers", grad(&ten, "2"), grad(&ten, "2.0"), true},
		{"other bound", grad(&ten, "2"), grad(&eleven, "2"), false},
		{"open and bounded", grad(&ten, "2"), grad(nil, "2"), false},
		{"other tier rate", grad(&ten, "2"), grad(&ten, "3"), false},
	} {
		if got := c.a.Equal(c.b); got != c.want {
			t.Errorf("%s: Equal = %v, want %v", c.name, got, c.want)
		}
	}
}
