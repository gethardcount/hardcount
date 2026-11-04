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
// (ADR-071 §2). Every other invoice tests expiry at the instant it is
// collected.
//
// A grant that was live when the period ended pays that period's invoice:
// Hardcount draws it when the period ends at or before expires_at. Stripe's
// eligibility list says "before" ("The invoice's period_end occurs before the
// credit grant's expires_at time"). Hardcount includes the boundary for two
// reasons. All of the period's usage happened before its end. And the
// dashboard stores "valid through Nov 30" as the next local midnight, which
// is the instant the November period ends.
func DrawsCreditAsOfIssue(reason InvoiceBillingReason) bool {
	return slices.Contains(creditDrawAsOfIssueReasons, reason)
}

// CreditDrawOwedSQL returns a SQL expression that is true while a draw that
// can still use the credit grant row aliased g is owed. While it is true, the
// expiry sweeps must not retire the grant (ADR-071 §2). A draw is owed in two
// cases:
//
//   - (i) An active or trialing subscription of the grant's customer is due
//     at or before the grant expires. Its close draws on the grant.
//   - (ii) A period or threshold invoice of the customer, issued at or before
//     the grant expires, is a draft or is queued for collection. Its
//     collection draws on the grant (DrawsCreditAsOfIssue).
//
// Both cases compare with `<=`, because a block is live at t when
// t <= expires_at, which is the rule the drain uses. With `<`, a sweep that
// runs before the close would retire a grant that expires at the due
// instant, and the close would charge the card in full.
//
// The hand-off between the two cases has no gap. The close moves the due
// instant and inserts the invoice (a draft, or queued) in one transaction.
// Finalize queues the invoice in the same write that finalizes it. So a sweep
// that reads this expression once, with no lock, never retires a grant that
// an owed draw still needs.
//
// Both terms are EXISTS, so the expression is never NULL and safe under NOT.
func CreditDrawOwedSQL(g string) string {
	return CreditDrawOwedExceptSQL(g, "")
}

// CreditDrawOwedExceptSQL is CreditDrawOwedSQL with one invoice left out of
// the owed-invoice term. exceptInvoice is a SQL expression for its id, or ""
// for none. A credit return (ADR-132) leaves out the invoice it is handing
// credit back for. That invoice is the draw being undone, so it cannot be a
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
