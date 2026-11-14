package domain

import (
	"reflect"
	"testing"
	"time"
)

// Two paid invoices of one month, each subtotal 10000, tax 2000, total
// 12000: the older priced before tax, the newer (an item added later) with
// tax included, after the tenant switched the setting. The unused amount is
// a share of the price; each invoice takes it in its own terms. The newer one
// takes its part as it is, up to its total. The older one grosses its part up
// by 12000/10000, up to its room before tax (10000).
func TestAllocateUnusedCredit_EachInvoiceInItsOwnTerms(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	before := Invoice{ID: "before", PaymentStatus: PaymentSucceeded, BillingReason: BillingReasonSubscriptionCreate,
		SubtotalCents: 10000, TotalAmountCents: 12000, CreatedAt: day(1)}
	incl := Invoice{ID: "incl", PaymentStatus: PaymentSucceeded, BillingReason: BillingReasonSubscriptionUpdate,
		SubtotalCents: 10000, TotalAmountCents: 12000, CreatedAt: day(16), TaxInclusive: true}

	for _, tc := range []struct {
		name         string
		unused       int64
		want         []CreditShare
		wantUnplaced int64
	}{
		{
			name: "fits in the newer: 6000 of the price is 6000 of credit", unused: 6000,
			want: []CreditShare{{"incl", 6000, true, 6000, 0, 0}},
		},
		{
			name: "the newer takes 12000 as it is; the older's 3000 is grossed up to 3600", unused: 15000,
			want: []CreditShare{{"incl", 12000, true, 12000, 0, 0}, {"before", 3600, true, 3600, 0, 0}},
		},
		{
			name: "both full: 12000 and 10000 of the price, 8000 left over", unused: 30000,
			want:         []CreditShare{{"incl", 12000, true, 12000, 0, 0}, {"before", 12000, true, 12000, 0, 0}},
			wantUnplaced: 8000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, unplaced := AllocateUnusedCredit([]Invoice{before, incl}, nil, "", tc.unused)
			if !reflect.DeepEqual(got, tc.want) || unplaced != tc.wantUnplaced {
				t.Errorf("shares = %v, unplaced %d; want %v, %d", got, unplaced, tc.want, tc.wantUnplaced)
			}
		})
	}
}
