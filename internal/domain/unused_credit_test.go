package domain

import (
	"reflect"
	"testing"
	"time"
)

// The one crediting rule for unused prepaid time (ADR-118). The real-Postgres
// walks are in internal/api/one_credit_rule_e2e_test.go; these pin each clause.
func TestAllocateUnusedCredit(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	paid := func(id string, total int64, at time.Time) Invoice {
		return Invoice{ID: id, PaymentStatus: PaymentSucceeded, SubtotalCents: total, TotalAmountCents: total, CreatedAt: at}
	}
	base := paid("base", 5000, day(1))
	base.BillingReason = BillingReasonSubscriptionCreate
	up := paid("up", 1500, day(16))
	up.BillingReason = BillingReasonSubscriptionUpdate
	up.SourceSubscriptionItemID = "item_a"
	addB := paid("add_b", 2000, day(16))
	addB.BillingReason = BillingReasonSubscriptionUpdate
	addB.SourceSubscriptionItemID = "item_b"

	cases := []struct {
		name         string
		sources      []Invoice
		credited     map[string]int64
		itemID       string
		net          int64
		want         []CreditShare
		wantUnplaced int64
	}{
		{
			name:    "newest charging invoice first, the rest spills to the base",
			sources: []Invoice{base, up}, itemID: "item_a", net: 1667,
			want: []CreditShare{{"up", 1500, true}, {"base", 167, true}},
		},
		{
			name:    "fits in the newest: the base is untouched",
			sources: []Invoice{base, up}, itemID: "item_a", net: 1000,
			want: []CreditShare{{"up", 1000, true}},
		},
		{
			name:    "another item's invoice is not a candidate",
			sources: []Invoice{base, addB}, itemID: "item_a", net: 667,
			want: []CreditShare{{"base", 667, true}},
		},
		{
			name:    "a whole-subscription credit takes every invoice, newest first",
			sources: []Invoice{base, addB}, itemID: "", net: 2667,
			want: []CreditShare{{"add_b", 2000, true}, {"base", 667, true}},
		},
		{
			name: "same instant: the subscription_update invoice goes first",
			sources: []Invoice{base, func() Invoice {
				u := up
				u.CreatedAt = day(1)
				return u
			}()}, itemID: "item_a", net: 1000,
			want: []CreditShare{{"up", 1000, true}},
		},
		{
			name: "same instant and reason: the later row goes first",
			sources: []Invoice{func() Invoice { u := up; u.ID = "up1"; return u }(),
				func() Invoice { u := up; u.ID = "up2"; return u }()},
			itemID: "item_a", net: 100,
			want: []CreditShare{{"up2", 100, true}},
		},
		{
			name:    "paid room is total minus credited",
			sources: []Invoice{base, up}, credited: map[string]int64{"up": 1200}, itemID: "item_a", net: 1667,
			want: []CreditShare{{"up", 300, true}, {"base", 1367, true}},
		},
		{
			name: "in flight is capped like paid, not by what is due",
			sources: []Invoice{func() Invoice {
				b := base
				b.PaymentStatus, b.AmountDueCents = PaymentProcessing, 0
				return b
			}()}, net: 2500,
			want: []CreditShare{{"base", 2500, false}},
		},
		{
			name: "unpaid room is what is due plus credits applied",
			sources: []Invoice{base, func() Invoice {
				u := up
				u.PaymentStatus, u.AmountDueCents, u.CreditsAppliedCents = PaymentPending, 500, 400
				return u
			}()}, itemID: "item_a", net: 1667,
			want: []CreditShare{{"up", 900, false}, {"base", 767, true}},
		},
		{
			name:    "an invoice with no room is skipped",
			sources: []Invoice{base, up}, credited: map[string]int64{"up": 1500}, itemID: "item_a", net: 1667,
			want: []CreditShare{{"base", 1667, true}},
		},
		{
			name:    "what fits nowhere is returned, not placed",
			sources: []Invoice{base}, credited: map[string]int64{"base": 4000}, net: 2500,
			want: []CreditShare{{"base", 1000, true}}, wantUnplaced: 1500,
		},
		{
			name:    "no room at all: no shares, all unplaced",
			sources: []Invoice{base}, credited: map[string]int64{"base": 5000}, net: 2500,
			want: nil, wantUnplaced: 2500,
		},
		{
			name: "gross up by the invoice's tax ratio",
			sources: []Invoice{{ID: "taxed", PaymentStatus: PaymentSucceeded, SubtotalCents: 6000,
				TaxFacts: TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600, CreatedAt: day(1)}},
			net:  2000,
			want: []CreditShare{{"taxed", 2200, true}},
		},
		{
			name: "net to gross rounding never passes the room",
			sources: []Invoice{{ID: "taxed", PaymentStatus: PaymentSucceeded, SubtotalCents: 3,
				TaxFacts: TaxFacts{TaxAmountCents: 1}, TotalAmountCents: 4, CreatedAt: day(1)}},
			credited: map[string]int64{"taxed": 2}, net: 5,
			want: []CreditShare{{"taxed", 2, true}}, wantUnplaced: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, unplaced := AllocateUnusedCredit(tc.sources, tc.credited, tc.itemID, tc.net)
			if !reflect.DeepEqual(got, tc.want) || unplaced != tc.wantUnplaced {
				t.Errorf("got %+v unplaced %d, want %+v unplaced %d", got, unplaced, tc.want, tc.wantUnplaced)
			}
			var room int64
			for _, sh := range got {
				for _, inv := range tc.sources {
					if inv.ID == sh.InvoiceID {
						room = inv.TotalAmountCents - tc.credited[inv.ID]
					}
				}
				if sh.GrossCents > room {
					t.Errorf("share %+v passes the invoice's room %d", sh, room)
				}
			}
		})
	}
}

func TestTaxPendingShare(t *testing.T) {
	pending := Invoice{ID: "d", Status: InvoiceDraft, TaxFacts: TaxFacts{TaxStatus: InvoiceTaxPending}}
	ok := Invoice{ID: "o", Status: InvoiceDraft, TaxFacts: TaxFacts{TaxStatus: InvoiceTaxOK}}
	if _, hit := TaxPendingShare([]Invoice{pending, ok}, []CreditShare{{InvoiceID: "o", GrossCents: 1}}); hit {
		t.Error("a pending draft that gets no share must not refuse the change")
	}
	if inv, hit := TaxPendingShare([]Invoice{pending, ok}, []CreditShare{{InvoiceID: "d", GrossCents: 1}}); !hit || inv.ID != "d" {
		t.Errorf("a share on a pending draft must refuse: got %v %v", inv.ID, hit)
	}
}
