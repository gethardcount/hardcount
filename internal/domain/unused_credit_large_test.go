package domain

import "testing"

// The credit for unused time converts between net and gross by the invoice's
// own total/subtotal ratio. The products are worked in 128 bits, so the
// shares are exact for any invoice amounts that fit int64 cents (these are
// not realistic; they used to wrap).
func TestAllocateUnusedCredit_LargeInvoicesExact(t *testing.T) {
	paid := func(id string, subtotal, total int64) Invoice {
		return Invoice{ID: id, PaymentStatus: PaymentSucceeded, SubtotalCents: subtotal, TotalAmountCents: total}
	}
	// No tax: net and gross are the same, 4e18 on a 9e18 invoice.
	shares, unplaced := AllocateUnusedCredit([]Invoice{paid("inv_a", 9_000_000_000_000_000_000, 9_000_000_000_000_000_000)}, nil, "", 4_000_000_000_000_000_000)
	if unplaced != 0 || len(shares) != 1 || shares[0].GrossCents != 4_000_000_000_000_000_000 {
		t.Errorf("no tax: shares %+v, unplaced %d; want one share of 4000000000000000000", shares, unplaced)
	}
	// 20% tax: net 5e18 of a 6e18 invoice (subtotal 5e18) is the whole
	// invoice, gross 6e18.
	shares, unplaced = AllocateUnusedCredit([]Invoice{paid("inv_b", 5_000_000_000_000_000_000, 6_000_000_000_000_000_000)}, nil, "", 5_000_000_000_000_000_000)
	if unplaced != 0 || len(shares) != 1 || shares[0].GrossCents != 6_000_000_000_000_000_000 {
		t.Errorf("20%% tax: shares %+v, unplaced %d; want one share of 6000000000000000000", shares, unplaced)
	}
	// A net larger than the room: the room is credited and the rest unplaced.
	shares, unplaced = AllocateUnusedCredit([]Invoice{paid("inv_c", 5_000_000_000_000_000_000, 6_000_000_000_000_000_000)}, nil, "", 7_000_000_000_000_000_000)
	if unplaced != 2_000_000_000_000_000_000 || len(shares) != 1 || shares[0].GrossCents != 6_000_000_000_000_000_000 {
		t.Errorf("past the room: shares %+v, unplaced %d; want 6000000000000000000 credited and 2000000000000000000 unplaced", shares, unplaced)
	}
}
