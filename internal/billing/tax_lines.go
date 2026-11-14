package billing

import (
	"fmt"
	"math"
	"slices"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/money"
	"github.com/gethardcount/hardcount/internal/tax"
)

// Tax providers must never see a negative line amount: Stripe Tax's
// calculation API rejects them outright (400 on line_items[].amount), and the
// manual provider clamps a negative base to zero — silently rewriting the
// per-line split. The proration CREATE path upholds this by taxing a single
// net line and splitting into the credit/charge pair only afterwards
// (ADR-048 Phase C), but the retry family (operator retry-tax, the tax_retry
// reconciler, the clock variant, the billing-profile flush) rebuilds the
// provider request from the STORED lines — which for a split proration
// invoice contain the negative credit line (issue #556: a create-time
// deferral on a split invoice could never be retried out of pending).
//
// collapseTaxRequestLines restores the invariant at the one chokepoint every
// caller shares: each negative line is absorbed into the largest positive
// line carrying the same tax code, so the provider is asked to tax the NET —
// exactly what the create path asked it. groups[k] lists the original
// line-item indexes behind request line k (absorber first); singleton groups
// are the untouched fast path.
//
// It is also where a prepaid-commit purchase line is left out of the
// request: buying credit is an advance payment, and the usage the credit
// pays for is taxed on its own invoice (ADR-078 amendment 2026-10-19,
// #1271). A commit line is in no group, so it gets no provider result; the
// caller stamps it untaxed. It never absorbs a negative line either.
func collapseTaxRequestLines(lineItems []domain.InvoiceLineItem, defaultTaxCode string) (reqLines []tax.RequestLine, groups [][]int, err error) {
	negatives := []int{}
	hasCommit := false
	for i := range lineItems {
		if lineItems[i].IsCommitLine() {
			hasCommit = true
			continue
		}
		if lineItems[i].AmountCents < 0 {
			negatives = append(negatives, i)
		}
	}

	// Fast path — no negative and no commit lines (every path except a
	// stored ADR-048 split pair and a commit purchase): one request line per
	// item, positionally aligned.
	if len(negatives) == 0 && !hasCommit {
		reqLines = make([]tax.RequestLine, len(lineItems))
		groups = make([][]int, len(lineItems))
		for i, li := range lineItems {
			reqLines[i] = tax.RequestLine{
				Ref:         fmt.Sprintf("line_%d", i),
				AmountCents: li.AmountCents,
				Quantity:    li.Quantity,
				TaxCode:     li.TaxCode,
			}
			groups[i] = []int{i}
		}
		return reqLines, groups, nil
	}

	lineCode := func(i int) string {
		if lineItems[i].TaxCode != "" {
			return lineItems[i].TaxCode
		}
		return defaultTaxCode
	}

	// Absorber per negative: the largest positive line with the same
	// effective tax code (rate rules follow the code, so crossing codes
	// would tax the net at the wrong rate). No absorber, or a pair that
	// nets negative, is a state no current writer produces — fail loud
	// rather than send the provider something it will misprice.
	absorberOf := make(map[int][]int) // absorber index -> absorbed negative indexes
	for _, n := range negatives {
		best := -1
		for i := range lineItems {
			if lineItems[i].AmountCents <= 0 || lineItems[i].IsCommitLine() || lineCode(i) != lineCode(n) {
				continue
			}
			if best == -1 || lineItems[i].AmountCents > lineItems[best].AmountCents {
				best = i
			}
		}
		if best == -1 {
			return nil, nil, fmt.Errorf("tax: negative line %d (%q, %d¢) has no positive line with tax code %q to absorb it for the provider calculation", n, lineItems[n].Description, lineItems[n].AmountCents, lineCode(n))
		}
		absorberOf[best] = append(absorberOf[best], n)
	}

	absorbed := make(map[int]bool, len(negatives))
	for _, n := range negatives {
		absorbed[n] = true
	}

	for i, li := range lineItems {
		if absorbed[i] || li.IsCommitLine() {
			continue
		}
		group := append([]int{i}, absorberOf[i]...)
		amount := int64(0)
		for _, gi := range group {
			if amount, err = addCents(amount, lineItems[gi].AmountCents); err != nil {
				return nil, nil, fmt.Errorf("tax: line %d group amount: %w", i, err)
			}
		}
		if amount < 0 {
			return nil, nil, fmt.Errorf("tax: line %d (%q) nets negative (%d¢) after absorbing its credit lines — cannot build a provider-safe calculation", i, li.Description, amount)
		}
		reqLines = append(reqLines, tax.RequestLine{
			Ref:         fmt.Sprintf("line_%d", len(reqLines)),
			AmountCents: amount,
			Quantity:    li.Quantity,
			TaxCode:     li.TaxCode,
		})
		groups = append(groups, group)
	}
	return reqLines, groups, nil
}

// stampUntaxedCommitLine marks a prepaid-commit purchase line, which
// collapseTaxRequestLines left out of the request: no tax, and the price is
// the line total. reason is commitLineTaxReason's.
func stampUntaxedCommitLine(li *domain.InvoiceLineItem, reason string) {
	li.TaxRate = 0
	li.TaxAmountCents = 0
	li.TaxJurisdiction = ""
	li.TaxabilityReason = reason
	li.TotalAmountCents = li.AmountCents
}

// commitLineTaxReason is the tax_reason an untaxed commit line carries. An
// account on the none provider collects no tax, so its invoices say nothing
// about tax on the line: no reason, so no dashboard label and no PDF legend.
func commitLineTaxReason(providerName string) string {
	if providerName == "none" {
		return ""
	}
	return domain.TaxReasonPrepaidCreditPurchase
}

// checkLineAmountsFit returns ErrAmountOverflow when the sizes of the line
// amounts sum past int64. Each line fits (the rater refuses one that does
// not), but two can sum past it; inside the bound, every sum of these lines,
// whatever their signs, fits.
func checkLineAmountsFit(lineItems []domain.InvoiceLineItem) error {
	var sum int64
	for i, li := range lineItems {
		a := li.AmountCents
		if a == math.MinInt64 {
			return fmt.Errorf("line %d amount: %w", i, domain.ErrAmountOverflow)
		}
		var err error
		if sum, err = domain.AddCents(sum, max(a, -a)); err != nil {
			return fmt.Errorf("invoice lines sum past int64 cents: %w", err)
		}
	}
	return nil
}

// addCentsSigned adds two cent amounts of any sign; ok is false when the sum
// does not fit int64.
func addCentsSigned(a, b int64) (int64, bool) {
	return money.Add(a, b)
}

// commitInvoiceSubtotal is the subtotal to tax an invoice from. For an
// invoice with a commit line it is the sum of the lines, not the stored
// header. An operator's lines always hold what was entered (a tax write
// keeps their amounts; see keepEnteredAmounts), but a tax-inclusive draft
// computed before the commit line was left out stored a header with that
// line carved to net. Taxed from that header, such a draft finalized after
// the change would be invoiced the net while its full credit is granted.
// Commit lines exist only on manual invoices, whose lines hold what the
// operator entered, so their sum is what to tax from. An invoice without a
// commit line keeps the subtotal it was given.
func commitInvoiceSubtotal(lineItems []domain.InvoiceLineItem, subtotal int64) (int64, error) {
	var sum int64
	hasCommit := false
	for _, li := range lineItems {
		var err error
		if sum, err = addCents(sum, li.AmountCents); err != nil {
			return 0, fmt.Errorf("invoice lines sum: %w", err)
		}
		hasCommit = hasCommit || li.IsCommitLine()
	}
	if !hasCommit {
		return subtotal, nil
	}
	return sum, nil
}

// storedTaxBasis is what a recomputation of a stored invoice's tax asks the
// provider to tax: each line's price, and their sum as the subtotal. It
// returns copies; the stored lines are not changed. The invoice's own
// TaxInclusive decides how its lines are read, never the tenant's setting
// now: the invoice was priced in those terms.
//
//   - An invoice priced before tax: the price is the stored amount, which
//     tax never changes, and the subtotal is the stored header's.
//   - An invoice priced with tax included, an operator's
//     (inv.OperatorComposed): the price is the stored amount, which is what
//     was entered. No tax write changes it (keepEnteredAmounts). The line
//     total is not used: an earlier version rounded a line's net apart from
//     its tax, so the total it stored can be a cent off the price.
//   - An invoice priced with tax included, billing's: the price is the line
//     total. A taxed line stores its net in the amount (the carve rewrote
//     it) and its price in the total. An untaxed line stores its price in
//     both.
//
// On an invoice priced with tax included the header subtotal is the net
// after any successful calculation, so the subtotal is the sum of the line
// prices instead. Taxing a stored net carved the tax out a second time: the
// invoice total fell by the tax.
func storedTaxBasis(inv domain.Invoice, items []domain.InvoiceLineItem) ([]domain.InvoiceLineItem, int64, error) {
	basis := slices.Clone(items)
	if !inv.TaxInclusive {
		return basis, inv.SubtotalCents, nil
	}
	var subtotal int64
	for i := range basis {
		if !inv.OperatorComposed() {
			basis[i].AmountCents = basis[i].TotalAmountCents
		}
		var err error
		if subtotal, err = addCents(subtotal, basis[i].AmountCents); err != nil {
			return nil, 0, fmt.Errorf("invoice line prices sum: %w", err)
		}
	}
	return basis, subtotal, nil
}

// keepEnteredAmounts sets the amount each recomputed line is saved with. Who
// made the invoice decides it, not the line's numbers:
//
//   - Billing (a subscription's draft): the net the calculation returned,
//     which computed already holds. A build saves its lines that way, so
//     every line's amount, tax and total add up, also after a rate change
//     or on a line that had no tax before.
//   - An operator (inv.OperatorComposed): the amount entered, which is the
//     price. The subtotal of a line added later sums these amounts
//     (AddLineItemAtomicAudited), and commitInvoiceSubtotal and
//     storedTaxBasis read them.
//
// In tax-exclusive mode the calculation returns every amount as stored, so
// both give the stored amount there.
func keepEnteredAmounts(inv domain.Invoice, computed, stored []domain.InvoiceLineItem) {
	if !inv.OperatorComposed() {
		return
	}
	for i := range computed {
		computed[i].AmountCents = stored[i].AmountCents
	}
}

// expandTaxResultLines maps the provider's per-request-line results back onto
// the original line items, one ResultLine per original index. Singleton
// groups pass through. For an absorbed group the returned tax (and, in
// inclusive mode, the returned net) is partitioned across the members
// proportionally by their amounts, remainder on the absorber — the same
// partition splitUpgradeProration stamps at create time (creditTax =
// RoundHalfToEven(T×credit, net); chargeTax = T − creditTax), so a retried
// invoice reproduces the create-time per-line split to the cent.
//
// The partition is checked against int64: a member's share is worked in 128
// bits, and a member's or the absorber's share that does not fit is
// ErrAmountOverflow. (A credit larger than the group's net takes a tax
// larger than the group's, and the absorber the rest, so the shares can
// outgrow the group's tax.)
func expandTaxResultLines(resLines []tax.ResultLine, groups [][]int, lineItems []domain.InvoiceLineItem) ([]tax.ResultLine, error) {
	out := make([]tax.ResultLine, len(lineItems))
	for k := range groups {
		if k >= len(resLines) {
			break
		}
		rl := resLines[k]
		group := groups[k]
		if len(group) == 1 {
			out[group[0]] = rl
			continue
		}

		groupNet := int64(0)
		for _, gi := range group {
			var err error
			if groupNet, err = addCents(groupNet, lineItems[gi].AmountCents); err != nil {
				return nil, fmt.Errorf("tax split of line %d: group net: %w", group[0], err)
			}
		}
		// collapseTaxRequestLines refuses a group that nets negative.
		if groupNet < 0 {
			return nil, fmt.Errorf("tax split of line %d: group nets negative (%d¢)", group[0], groupNet)
		}

		var taxAssigned, netAssigned int64
		for _, gi := range group[1:] { // absorbed (negative) members first
			memberTax := int64(0)
			memberNet := lineItems[gi].AmountCents
			if groupNet != 0 {
				var ok bool
				if memberTax, ok = money.MulDivRoundHalfToEvenSigned(rl.TaxAmountCents, lineItems[gi].AmountCents, groupNet); !ok {
					return nil, fmt.Errorf("tax split of line %d: tax share of line %d: %w", group[0], gi, domain.ErrAmountOverflow)
				}
				if rl.NetAmountCents != groupNet {
					// Inclusive mode carved the group's net; scale members.
					if memberNet, ok = money.MulDivRoundHalfToEvenSigned(rl.NetAmountCents, lineItems[gi].AmountCents, groupNet); !ok {
						return nil, fmt.Errorf("tax split of line %d: net share of line %d: %w", group[0], gi, domain.ErrAmountOverflow)
					}
				}
			}
			member := rl
			member.NetAmountCents = memberNet
			member.TaxAmountCents = memberTax
			out[gi] = member
			var err error
			if taxAssigned, err = addCents(taxAssigned, memberTax); err != nil {
				return nil, fmt.Errorf("tax split of line %d: %w", group[0], err)
			}
			if netAssigned, err = addCents(netAssigned, memberNet); err != nil {
				return nil, fmt.Errorf("tax split of line %d: %w", group[0], err)
			}
		}
		absorber := rl
		var err error
		if rl.NetAmountCents == groupNet { // exclusive: keep stored amounts exact
			absorber.NetAmountCents = lineItems[group[0]].AmountCents
		} else if absorber.NetAmountCents, err = subCents(rl.NetAmountCents, netAssigned); err != nil {
			return nil, fmt.Errorf("tax split of line %d: absorber net: %w", group[0], err)
		}
		if absorber.TaxAmountCents, err = subCents(rl.TaxAmountCents, taxAssigned); err != nil {
			return nil, fmt.Errorf("tax split of line %d: absorber tax: %w", group[0], err)
		}
		out[group[0]] = absorber
	}
	return out, nil
}
