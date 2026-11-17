package domain

import (
	"testing"
	"time"
)

// TestDisputeStatus_ValidAndClosed: the eight statuses Stripe documents for a
// Dispute are the eight Hardcount stores (the column's CHECK lists the same), and
// exactly won, lost, warning_closed and prevented are closed.
func TestDisputeStatus_ValidAndClosed(t *testing.T) {
	closed := map[DisputeStatus]bool{
		DisputeWarningNeedsResponse: false, DisputeWarningUnderReview: false,
		DisputeNeedsResponse: false, DisputeUnderReview: false,
		DisputeWarningClosed: true, DisputeWon: true, DisputeLost: true, DisputePrevented: true,
	}
	for s, want := range closed {
		if !s.Valid() {
			t.Errorf("%s: not valid", s)
		}
		if s.Closed() != want {
			t.Errorf("%s: closed = %v, want %v", s, s.Closed(), want)
		}
	}
	for _, s := range []DisputeStatus{"", "charge_refunded", "under_arbitration"} {
		if s.Valid() || s.Closed() {
			t.Errorf("%q: valid=%v closed=%v, want neither", s, s.Valid(), s.Closed())
		}
	}
}

// TestInvoiceDispute_SupersededBy is the ordering rule that makes the record
// insensitive to delivery order: a later event replaces the row, an earlier
// one never does, the same event never does (a redelivery), and within one
// second the later stage of the dispute's life wins.
func TestInvoiceDispute_SupersededBy(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	row := InvoiceDispute{Status: DisputeUnderReview, LastEventAt: at, LastStripeEventID: "evt_row"}
	snap := func(id string, s DisputeStatus, when time.Time) DisputeSnapshot {
		return DisputeSnapshot{StripeEventID: id, Status: s, EventAt: when}
	}
	cases := []struct {
		name string
		s    DisputeSnapshot
		want bool
	}{
		{"later event", snap("evt_b", DisputeNeedsResponse, at.Add(time.Second)), true},
		{"earlier event", snap("evt_b", DisputeLost, at.Add(-time.Second)), false},
		{"same event redelivered, even if its time were later", snap("evt_row", DisputeLost, at.Add(time.Hour)), false},
		{"same second, later stage", snap("evt_b", DisputeLost, at), true},
		{"same second, earlier stage", snap("evt_b", DisputeNeedsResponse, at), false},
		{"same second, same stage", snap("evt_b", DisputeUnderReview, at), false},
	}
	for _, tc := range cases {
		if got := row.SupersededBy(tc.s); got != tc.want {
			t.Errorf("%s: SupersededBy = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A late win: lost, then won days later.
	lost := InvoiceDispute{Status: DisputeLost, LastEventAt: at, LastStripeEventID: "evt_lost"}
	if !lost.SupersededBy(snap("evt_won", DisputeWon, at.Add(72*time.Hour))) {
		t.Error("a later won must replace lost (Stripe's late win)")
	}
	// Two closes in one second do not order each other: the first one stays.
	if lost.SupersededBy(snap("evt_won", DisputeWon, at)) {
		t.Error("won in the same second as lost must not replace it")
	}
}
