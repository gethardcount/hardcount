package domain

import (
	"slices"
	"strings"
)

// creditDrawAsOfIssueReasons are the billing reasons of the invoices that bill
// a stretch of time ending at their issued_at: the close of a period
// (subscription_cycle) and a spend-threshold fire (threshold). Both keep that
// issued_at through a later finalize.
var creditDrawAsOfIssueReasons = []InvoiceBillingReason{BillingReasonSubscriptionCycle, BillingReasonThreshold}

// DrawsCreditAsOfIssue reports whether an invoice with this billing reason
// tests credit grant expiry at its issued_at, however late it is collected
// (ADR-071 §2). A grant that was live when the period ended pays that
// period's invoice: Hardcount draws it when the period ends at or before
// expires_at. Stripe's eligibility list says "before" ("The invoice's
// period_end occurs before the credit grant's expires_at time"). Hardcount
// includes the boundary because all of the period's usage happened before
// its end, and because the dashboard stores "valid through Nov 30" as the
// next local midnight, the instant the November period ends. Every other
// invoice tests expiry at the instant it is collected.
func DrawsCreditAsOfIssue(reason InvoiceBillingReason) bool {
	return slices.Contains(creditDrawAsOfIssueReasons, reason)
}

// CreditDrawOwedSQL is true while a draw that can still use the credit grant
// row aliased g is owed, so the expiry sweeps must not retire it (ADR-071 §2):
//
//   - (i) an active or trialing subscription of the grant's customer is due
//     at or before the grant expires: its close draws on the grant;
//   - (ii) a period or threshold invoice of the customer issued at or before
//     the grant expires is a draft or is queued for collection: its
//     collection draws on the grant (DrawsCreditAsOfIssue).
//
// Both are `<=` because a block is live at t when t <= expires_at, the rule
// the drain uses. With `<`, a grant expiring at the due instant would be
// retired by a sweep that runs before the close, and the close would charge
// the card in full.
//
// The hand-off between the two has no gap. The close moves the due instant
// and inserts the invoice (a draft, or queued) in one transaction, and
// finalize queues it in the write that finalizes. So a sweep that reads this
// once, with no lock, never retires a grant an owed draw still needs.
//
// Both terms are EXISTS, so the expression is never NULL and safe under NOT.
func CreditDrawOwedSQL(g string) string {
	return CreditDrawOwedExceptSQL(g, "")
}

// CreditDrawOwedExceptSQL is CreditDrawOwedSQL with one invoice left out of
// the owed-invoice term: exceptInvoice is a SQL expression for its id, or ""
// for none. A credit return (ADR-132) leaves out the invoice it is handing
// credit back for: that invoice is the draw being undone, so it cannot be a
// draw the returned credit is still owed to.
func CreditDrawOwedExceptSQL(g, exceptInvoice string) string {
	except := ""
	if exceptInvoice != "" {
		except = `
			   AND i.id <> ` + exceptInvoice
	}
	reasons := make([]string, len(creditDrawAsOfIssueReasons))
	for i, r := range creditDrawAsOfIssueReasons {
		reasons[i] = "'" + string(r) + "'"
	}
	return `(EXISTS (
			SELECT 1 FROM subscriptions s
			 WHERE s.tenant_id = ` + g + `.tenant_id AND s.customer_id = ` + g + `.customer_id
			   AND s.status IN ('active','trialing')
			   AND ` + SubscriptionDueInstantSQL("s") + ` <= ` + g + `.expires_at)
		OR EXISTS (
			SELECT 1 FROM invoices i
			 WHERE i.tenant_id = ` + g + `.tenant_id AND i.customer_id = ` + g + `.customer_id
			   AND i.billing_reason IN (` + strings.Join(reasons, ",") + `)
			   AND i.issued_at <= ` + g + `.expires_at` + except + `
			   AND (i.status = 'draft' OR i.auto_charge_pending)))`
}
