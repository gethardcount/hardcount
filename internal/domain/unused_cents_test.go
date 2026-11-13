package domain

import (
	"testing"
	"time"
)

// The unused amount is split in the terms it was worked out in. A paid
// invoice of subtotal 10000, tax 2000, total 12000 credited for 6000 of
// unused time: with tax in the amount the note is 6000; before tax it is
// grossed up to 7200; 13000 with tax in it fills the 12000 room and leaves
// 1000, in the amount's terms.
func TestAllocateUnusedCredit_InTheAmountsTerms(t *testing.T) {
	inv := Invoice{ID: "inv", PaymentStatus: PaymentSucceeded, SubtotalCents: 10000,
		TaxFacts: TaxFacts{TaxAmountCents: 2000}, TotalAmountCents: 12000, CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	cases := []struct {
		name         string
		unused       UnusedCents
		wantGross    int64
		wantUnplaced int64
	}{
		{"with tax in it: credited as is", UnusedCents{Cents: 6000, IncludesTax: true}, 6000, 0},
		{"before tax: grossed up by the invoice's 12000/10000", UnusedCents{Cents: 6000}, 7200, 0},
		{"with tax in it, more than the room", UnusedCents{Cents: 13000, IncludesTax: true}, 12000, 1000},
		{"before tax, more than the room", UnusedCents{Cents: 11000}, 12000, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shares, unplaced := AllocateUnusedCredit([]Invoice{inv}, nil, "", tc.unused)
			if len(shares) != 1 || shares[0].GrossCents != tc.wantGross || unplaced != tc.wantUnplaced {
				t.Errorf("shares %+v, unplaced %d; want gross %d, unplaced %d", shares, unplaced, tc.wantGross, tc.wantUnplaced)
			}
		})
	}
}

// A wholly voided invoice takes its total from an amount with tax in it, and
// its subtotal from one before tax, never below zero.
func TestUnusedCents_LessVoided(t *testing.T) {
	voided := Invoice{SubtotalCents: 10000, TaxFacts: TaxFacts{TaxAmountCents: 2000}, TotalAmountCents: 12000}
	cases := []struct {
		name string
		in   UnusedCents
		want int64
	}{
		{"with tax: 24000 less the total", UnusedCents{Cents: 24000, IncludesTax: true}, 12000},
		{"before tax: 20000 less the subtotal", UnusedCents{Cents: 20000}, 10000},
		{"never below zero", UnusedCents{Cents: 5000, IncludesTax: true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.LessVoided(voided)
			if got.Cents != tc.want || got.IncludesTax != tc.in.IncludesTax {
				t.Errorf("LessVoided = %+v, want %d in the same terms", got, tc.want)
			}
		})
	}
}
