package billing

import (
	"fmt"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/money"
	"github.com/sagarsuperuser/velox/internal/tax"
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
			amount += lineItems[gi].AmountCents
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
// collapseTaxRequestLines left out of the request: no tax, the price is the
// line total, and the reason says why.
func stampUntaxedCommitLine(li *domain.InvoiceLineItem) {
	li.TaxRate = 0
	li.TaxAmountCents = 0
	li.TaxJurisdiction = ""
	li.TaxabilityReason = domain.TaxReasonPrepaidCreditPurchase
	li.TotalAmountCents = li.AmountCents
}

// taxRequestDiscount is the share of an invoice-level discount that falls on
// the lines sent to the provider. A discount covers every line in proportion
// to its amount, so a commit line left out of the request takes its share
// with it; otherwise the whole discount would lower the taxable base. Without
// commit lines it is the discount unchanged.
func taxRequestDiscount(lineItems []domain.InvoiceLineItem, discount int64) int64 {
	if discount <= 0 {
		return discount
	}
	var all, taxable int64
	for _, li := range lineItems {
		all += li.AmountCents
		if !li.IsCommitLine() {
			taxable += li.AmountCents
		}
	}
	if taxable == all || all <= 0 {
		return discount
	}
	return money.RoundHalfToEven(discount*taxable, all)
}

// expandTaxResultLines maps the provider's per-request-line results back onto
// the original line items, one ResultLine per original index. Singleton
// groups pass through. For an absorbed group the returned tax (and, in
// inclusive mode, the returned net) is partitioned across the members
// proportionally by their amounts, remainder on the absorber — the same
// partition splitUpgradeProration stamps at create time (creditTax =
// RoundHalfToEven(T×credit, net); chargeTax = T − creditTax), so a retried
// invoice reproduces the create-time per-line split to the cent.
func expandTaxResultLines(resLines []tax.ResultLine, groups [][]int, lineItems []domain.InvoiceLineItem) []tax.ResultLine {
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
			groupNet += lineItems[gi].AmountCents
		}

		var taxAssigned, netAssigned int64
		for _, gi := range group[1:] { // absorbed (negative) members first
			memberTax := int64(0)
			memberNet := lineItems[gi].AmountCents
			if groupNet != 0 {
				memberTax = money.RoundHalfToEven(rl.TaxAmountCents*lineItems[gi].AmountCents, groupNet)
				if rl.NetAmountCents != groupNet {
					// Inclusive mode carved the group's net; scale members.
					memberNet = money.RoundHalfToEven(rl.NetAmountCents*lineItems[gi].AmountCents, groupNet)
				}
			}
			member := rl
			member.NetAmountCents = memberNet
			member.TaxAmountCents = memberTax
			out[gi] = member
			taxAssigned += memberTax
			netAssigned += memberNet
		}
		absorber := rl
		absorber.NetAmountCents = rl.NetAmountCents - netAssigned
		if rl.NetAmountCents == groupNet { // exclusive: keep stored amounts exact
			absorber.NetAmountCents = lineItems[group[0]].AmountCents
		}
		absorber.TaxAmountCents = rl.TaxAmountCents - taxAssigned
		out[group[0]] = absorber
	}
	return out
}
