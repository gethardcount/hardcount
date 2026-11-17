package domain

import (
	"testing"
	"time"
)

func TestStripeRefundStatus(t *testing.T) {
	cases := []struct {
		s          StripeRefundStatus
		valid      bool
		noMoney    bool
		creditNote RefundStatus
	}{
		{StripeRefundPending, true, false, RefundPending},
		{StripeRefundRequiresAction, true, false, RefundPending},
		{StripeRefundSucceeded, true, false, RefundSucceeded},
		{StripeRefundFailed, true, true, RefundFailed},
		{StripeRefundCanceled, true, true, RefundFailed},
		{"reversed", false, false, RefundPending},
	}
	for _, tc := range cases {
		if tc.s.Valid() != tc.valid || tc.s.MovedNoMoney() != tc.noMoney || tc.s.CreditNoteRefundStatus() != tc.creditNote {
			t.Errorf("%q: valid=%v noMoney=%v note=%q, want %v %v %q", tc.s,
				tc.s.Valid(), tc.s.MovedNoMoney(), tc.s.CreditNoteRefundStatus(), tc.valid, tc.noMoney, tc.creditNote)
		}
	}
}

// TestInvoiceStripeRefund_SupersededBy: the row keeps the snapshot of the
// event Stripe created last, whatever order events arrive in; within one
// second the later stage of the refund wins; a redelivery is never newer.
func TestInvoiceStripeRefund_SupersededBy(t *testing.T) {
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	row := InvoiceStripeRefund{Status: StripeRefundPending, LastEventAt: at, LastStripeEventID: "evt_created"}
	snap := func(id string, d time.Duration, s StripeRefundStatus) StripeRefundSnapshot {
		return StripeRefundSnapshot{StripeEventID: id, EventAt: at.Add(d), Status: s}
	}
	cases := []struct {
		name string
		s    StripeRefundSnapshot
		want bool
	}{
		{"redelivery", snap("evt_created", time.Hour, StripeRefundFailed), false},
		{"later event", snap("evt_failed", time.Hour, StripeRefundFailed), true},
		{"older event", snap("evt_old", -time.Hour, StripeRefundSucceeded), false},
		{"same second, later stage", snap("evt_ok", 0, StripeRefundSucceeded), true},
		{"same second, same stage", snap("evt_ra", 0, StripeRefundRequiresAction), false},
	}
	for _, tc := range cases {
		if got := row.SupersededBy(tc.s); got != tc.want {
			t.Errorf("%s: SupersededBy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStripeRefundSeenLate(t *testing.T) {
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		after time.Duration
		want  bool
	}{{time.Minute, false}, {stripeRefundLateAfter, false}, {stripeRefundLateAfter + time.Second, true}, {72 * time.Hour, true}} {
		if got := StripeRefundSeenLate(created, created.Add(tc.after)); got != tc.want {
			t.Errorf("seen %s after creation: StripeRefundSeenLate = %v, want %v", tc.after, got, tc.want)
		}
	}
	if StripeRefundSeenLate(time.Time{}, created) {
		t.Error("a refund of unknown age is not late")
	}
}
