package domain

// Relief is what a cancel or plan swap owes the customer for the unused,
// prepaid part of the period, as written on the change's own transaction:
// issue_pending credit-note drafts (paid shares, and partly-used unpaid
// shares) and invoices voided in place (fully-unused unpaid shares).
// CreditedCents is the customer-balance credit (paid and in-flight shares) for the
// activity timeline. UncreditedCents (net) is the unused time the funding
// invoices had no room left for: earlier notes credited them up to their
// totals, or an unpaid one had nothing more to relieve. It is not owed; the
// change records it (uncredited_cents) so the timeline can say so. After the transaction commits, FinishRelief issues the
// drafts and runs the voids' post-commit legs; a crash before that loses
// nothing — RetryPendingClawbackIssue issues the drafts and
// RetryPendingTaxReversal reverses the voids' tax.
type Relief struct {
	DraftIDs        []string
	Voided          []Invoice
	CreditedCents   int64
	UncreditedCents int64
}
