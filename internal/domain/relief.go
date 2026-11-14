package domain

// Relief is what a cancel or plan swap owes the customer for the unused,
// prepaid part of the period, as written in the change's own transaction.
// DraftIDs are issue_pending credit-note drafts (paid shares, and unpaid
// shares that are partly used). Voided are invoices voided in place (unpaid
// shares that are wholly unused). CreditedCents is the credit to the
// customer's balance (paid and in-flight shares), for the activity timeline.
// UncreditedCents is unused time, in the price's terms, that the funding
// invoices had no room left for: earlier notes credited them up to their
// totals, or an unpaid one had nothing more to relieve. It is not owed. The
// change records it (uncredited_cents) so the timeline can say so.
//
// After the transaction commits, FinishRelief issues the drafts and runs the
// voids' post-commit legs. A crash before that loses nothing:
// RetryPendingClawbackIssue issues the drafts, and RetryPendingTaxReversal
// reverses the voids' tax.
type Relief struct {
	DraftIDs        []string
	Voided          []Invoice
	CreditedCents   int64
	UncreditedCents int64
}
