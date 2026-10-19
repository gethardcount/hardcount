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
// period's invoice, by the expires_at condition of Stripe's eligibility list:
// "The invoice's period_end occurs before the credit grant's expires_at
// time". Every other invoice tests expiry at the instant it is collected.
func DrawsCreditAsOfIssue(reason InvoiceBillingReason) bool {
	return slices.Contains(creditDrawAsOfIssueReasons, reason)
}

// CreditDrawOwedSQL is true while a draw that can still use the credit grant
// row aliased g is owed, so the expiry sweeps must not retire it (ADR-071 §2):
//
//   - (i) an active or trialing subscription of the grant's customer is due
//     before the grant expires: its close draws on the grant;
//   - (ii) a period or threshold invoice of the customer issued before the
//     grant expires is a draft or is queued for collection: its collection
//     draws on the grant (DrawsCreditAsOfIssue).
//
// The hand-off between the two has no gap. The close moves the due instant
// and inserts the invoice (a draft, or queued) in one transaction, and
// finalize queues it in the write that finalizes. So a sweep that reads this
// once, with no lock, never retires a grant an owed draw still needs.
//
// Both terms are EXISTS, so the expression is never NULL and safe under NOT.
func CreditDrawOwedSQL(g string) string {
	reasons := make([]string, len(creditDrawAsOfIssueReasons))
	for i, r := range creditDrawAsOfIssueReasons {
		reasons[i] = "'" + string(r) + "'"
	}
	return `(EXISTS (
			SELECT 1 FROM subscriptions s
			 WHERE s.tenant_id = ` + g + `.tenant_id AND s.customer_id = ` + g + `.customer_id
			   AND s.status IN ('active','trialing')
			   AND ` + SubscriptionDueInstantSQL("s") + ` < ` + g + `.expires_at)
		OR EXISTS (
			SELECT 1 FROM invoices i
			 WHERE i.tenant_id = ` + g + `.tenant_id AND i.customer_id = ` + g + `.customer_id
			   AND i.billing_reason IN (` + strings.Join(reasons, ",") + `)
			   AND i.issued_at < ` + g + `.expires_at
			   AND (i.status = 'draft' OR i.auto_charge_pending)))`
}
