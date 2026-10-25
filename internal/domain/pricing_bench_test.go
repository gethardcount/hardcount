package domain

import (
	"testing"

	"github.com/shopspring/decimal"
)

func BenchmarkComputeAmountCents_Flat(b *testing.B) {
	rule := RatingRuleVersion{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(500)}
	q := decimal.NewFromInt(1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ComputeAmountCents(rule, q)
	}
}

func BenchmarkComputeAmountCents_Graduated_5Tiers(b *testing.B) {
	rule := RatingRuleVersion{
		Mode: PricingGraduated,
		Tiers: []RatingTier{
			{UpTo: new(int64(100)), UnitAmountCents: decimal.NewFromInt(50)},
			{UpTo: new(int64(500)), UnitAmountCents: decimal.NewFromInt(30)},
			{UpTo: new(int64(1000)), UnitAmountCents: decimal.NewFromInt(20)},
			{UpTo: new(int64(5000)), UnitAmountCents: decimal.NewFromInt(10)},
			{UpTo: nil, UnitAmountCents: decimal.NewFromInt(5)},
		},
	}
	q := decimal.NewFromInt(7500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ComputeAmountCents(rule, q)
	}
}

func BenchmarkComputeAmountCents_Package(b *testing.B) {
	rule := RatingRuleVersion{
		Mode:               PricingPackage,
		PackageSize:        1000,
		PackageAmountCents: decimal.NewFromInt(2000),
	}
	q := decimal.NewFromInt(15750)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ComputeAmountCents(rule, q)
	}
}
