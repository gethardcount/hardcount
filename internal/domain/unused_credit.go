package domain

import (
	"math"
	"sort"

	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// CreditShare is one invoice's part of the credit for unused, prepaid time:
// GrossCents is the credit note's amount, tax included. ToBalance says the
// share goes to the customer's balance: the invoice is paid, or its charge is
// in flight, which Issue holds until the charge settles and which normally
// succeeds. Otherwise the share reduces what the open invoice still asks for.
type CreditShare struct {
	InvoiceID  string
	GrossCents int64
	ToBalance  bool
}

// AllocateUnusedCredit splits netCents of unused prepaid time (tax excluded)
// across the invoices that charged it. It is the one crediting rule for every
// change that gives prepaid time back: a quantity decrease, a downgrade, an
// item removal, a cancel and a cross-interval plan swap (ADR-118).
//
// The rule is "credit the invoice that charged it, newest first", which is
// how Stripe credits a decrease ("based on the last price billed"):
//
//   - Candidates. With an itemID, only the invoices that charged that item:
//     the cycle invoices (no source item) and the item's own proration
//     invoices. A change to seat item A never credits the invoice that added
//     item B. With no itemID (cancel, swap: the whole subscription) every
//     funding invoice is a candidate.
//   - Order. Newest first by CreatedAt. On a tie the subscription_update
//     invoice goes first: it is the later price level (a test-clock catchup
//     stamps every invoice it makes with one instant). Then the later row in
//     sources' order (FindFundingInvoicesForPeriod orders base first).
//   - Room. A paid invoice, or one whose charge is in flight, can take what is
//     left of its total: total − credited. For an in-flight invoice Issue
//     picks the channel once the charge settles. An unpaid invoice can take
//     what it still asks for plus the customer credits applied to it, which
//     the note hands back (ADR-061), and never more than total − credited:
//     a note held while a charge was in flight has not reduced what is due
//     yet, and the credit-note writer counts it. An invoice with no room is
//     skipped.
//
// credited maps an invoice id to the credit notes already on it (counting
// drafts). Callers MUST read it outside the change's transaction: the change's
// own drafts would otherwise shrink their own room. The credit-note store
// re-checks the cap under the invoice lock when the draft is written, so a
// concurrent note makes the change fail and retry, never over-credit.
//
// unplacedCents (net) is what did not fit. It is not owed: every invoice is
// either credited up to its total or, unpaid, relieved of all it asks for. The
// caller records it on the change (uncredited_cents) rather than logging it.
func AllocateUnusedCredit(sources []Invoice, credited map[string]int64, itemID string, netCents int64) (shares []CreditShare, unplacedCents int64) {
	if netCents <= 0 {
		return nil, 0
	}
	type candidate struct {
		inv              Invoice
		pos              int
		grossCap, netCap int64
	}
	cands := make([]candidate, 0, len(sources))
	for pos, inv := range sources {
		if itemID != "" && inv.SourceSubscriptionItemID != "" && inv.SourceSubscriptionItemID != itemID {
			continue // charged another item
		}
		var grossCap int64
		if inv.PaymentStatus == PaymentSucceeded || inv.PaymentInFlight() {
			grossCap = inv.TotalAmountCents - credited[inv.ID]
		} else {
			grossCap = min(inv.UnpaidRelievableCents(), inv.TotalAmountCents-credited[inv.ID])
		}
		if grossCap <= 0 {
			continue // no room left
		}
		netCap := grossCap
		if inv.SubtotalCents > 0 && inv.TotalAmountCents > 0 {
			// Worked in 128 bits. A cap past int64 cents is above any
			// netCents, so MaxInt64 allocates exactly as the exact cap would.
			var ok bool
			if netCap, ok = money.MulDivRoundHalfToEven(grossCap, inv.SubtotalCents, inv.TotalAmountCents); !ok {
				netCap = math.MaxInt64
			}
		}
		cands = append(cands, candidate{inv: inv, pos: pos, grossCap: grossCap, netCap: netCap})
	}
	sort.SliceStable(cands, func(a, b int) bool {
		ia, ib := cands[a].inv, cands[b].inv
		if !ia.CreatedAt.Equal(ib.CreatedAt) {
			return ia.CreatedAt.After(ib.CreatedAt)
		}
		ua := ia.BillingReason == BillingReasonSubscriptionUpdate
		ub := ib.BillingReason == BillingReasonSubscriptionUpdate
		if ua != ub {
			return ua
		}
		return cands[a].pos > cands[b].pos
	})

	netCaps := make([]int64, len(cands))
	for i := range cands {
		netCaps[i] = cands[i].netCap
	}
	nets, unplaced := money.AllocateLIFO(netCents, netCaps)
	for i, c := range cands {
		if nets[i] <= 0 {
			continue
		}
		gross := nets[i]
		if c.inv.SubtotalCents > 0 {
			// Worked in 128 bits. A gross past int64 cents is above the
			// room, so the room is what the min below would keep.
			var ok bool
			if gross, ok = money.MulDivRoundHalfToEven(nets[i], c.inv.TotalAmountCents, c.inv.SubtotalCents); !ok {
				gross = c.grossCap
			}
		}
		gross = min(gross, c.grossCap) // net→gross rounding never passes the room
		if gross <= 0 {
			continue
		}
		shares = append(shares, CreditShare{InvoiceID: c.inv.ID, GrossCents: gross, ToBalance: c.inv.PaymentStatus == PaymentSucceeded || c.inv.PaymentInFlight()})
	}
	return shares, unplaced
}

// TaxPendingShare returns the invoice of the first share that is a draft
// whose tax is not calculated yet. It has no final total, so no credit can be
// sized against it: the change is refused (funding_invoice_tax_pending) and
// retried once the tax retry settles it, normally within minutes.
func TaxPendingShare(sources []Invoice, shares []CreditShare) (Invoice, bool) {
	for _, sh := range shares {
		for _, inv := range sources {
			if inv.ID == sh.InvoiceID && inv.Status == InvoiceDraft && inv.TaxStatus != "" && inv.TaxStatus != InvoiceTaxOK {
				return inv, true
			}
		}
	}
	return Invoice{}, false
}
