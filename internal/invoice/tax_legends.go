package invoice

import (
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// exemptionLegend returns one to three lines of legend text triggered by
// per-line taxability_reason values, or "" if nothing applies. Order:
// customer_exempt, then product_exempt, then a prepaid credit purchase.
//
// Decoupled from the existing reverse-charge legend (driven by the calc-level
// TaxReverseCharge boolean) — the two coexist on the PDF when both apply,
// because they disclose materially different tax treatments. customer_exempt
// is "this customer holds an exemption certificate" while reverse_charge is
// "the recipient self-accounts for VAT/GST". Don't conflate them.
//
// A prepaid credit purchase is not an exemption: the line is untaxed because
// buying credit is an advance payment, and the usage the credit pays for is
// taxed on its own invoice (ADR-078 amendment 2026-10-19). The legend says
// so, so the missing tax does not read as a mistake.
//
// Pure helper — no PDF state, fully unit-testable.
func exemptionLegend(lineItems []domain.InvoiceLineItem) string {
	var hasCustomerExempt, hasProductExempt, hasPrepaidCredit bool
	for _, li := range lineItems {
		switch strings.TrimSpace(li.TaxabilityReason) {
		case "customer_exempt":
			hasCustomerExempt = true
		case "product_exempt":
			hasProductExempt = true
		case domain.TaxReasonPrepaidCreditPurchase:
			hasPrepaidCredit = true
		}
	}
	var parts []string
	if hasCustomerExempt {
		parts = append(parts, "One or more lines are exempt from tax based on the customer's exemption certificate.")
	}
	if hasProductExempt {
		parts = append(parts, "One or more lines are exempt from tax in this jurisdiction by product category.")
	}
	if hasPrepaidCredit {
		parts = append(parts, "Prepaid credit is not taxed when bought; any tax is charged on the usage it pays for.")
	}
	return strings.Join(parts, "\n")
}
