package domain

// Relief is what a cancel or plan swap owes the customer for the unused,
// prepaid part of the period, as written on the change's own transaction:
// issue_pending credit-note drafts (paid shares, and partly-used unpaid
// shares) and invoices voided in place (fully-unused unpaid shares).
// CreditedCents is the customer-balance credit (paid shares only) for the
// activity timeline. After the transaction commits, FinishRelief issues the
// drafts and runs the voids' post-commit legs; a crash before that loses
// nothing — RetryPendingClawbackIssue issues the drafts and
// RetryPendingTaxReversal reverses the voids' tax.
type Relief struct {
	DraftIDs      []string
	Voided        []Invoice
	CreditedCents int64
}
