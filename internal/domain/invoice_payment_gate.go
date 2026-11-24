package domain

// InvoiceAction names an operator action whose availability depends on the
// invoice's payment state.
type InvoiceAction string

const (
	ActionVoid                 InvoiceAction = "void"
	ActionMarkUncollectible    InvoiceAction = "mark_uncollectible"
	ActionRecordOfflinePayment InvoiceAction = "record_offline_payment"
	ActionIssueCreditNote      InvoiceAction = "issue_credit_note"
	ActionCollectPayment       InvoiceAction = "collect_payment"
	ActionResendSetupLink      InvoiceAction = "resend_setup_link"
	ActionEmailInvoice         InvoiceAction = "email_invoice"
)

// PaymentBlock is the answer to "does this invoice's payment state block this
// action, and what should the operator be told?"
type PaymentBlock struct {
	Blocked bool `json:"blocked"`
	// Code is machine-readable, for API consumers and the dashboard.
	Code string `json:"code,omitempty"`
	// Message is for the operator, and it must always answer both halves:
	// why the action is refused, and what to do instead. A refusal that names
	// no alternative leaves the operator stuck with an invoice and no next
	// step.
	Message string `json:"message,omitempty"`
}

// PaymentBlocksAction reports whether the invoice's payment state blocks an
// operator action, and what to tell the operator. It is the one statement of
// this rule: change it here, not at the call sites. The dashboard has one
// copy of the in-flight test, paymentIsUnresolved in
// web-v2/src/lib/status.ts.
//
// There are two cases:
//   - No payment in flight: nothing is blocked here. The caller's own status
//     rules (paid, voided, draft) still apply. They stay out of this function
//     on purpose, because they differ per action.
//   - In flight: the charge is resolving on the PaymentIntent the invoice
//     names, so every action is refused and the operator is told to wait.
//     Write-off of an 'unknown' charge is the one exception, below.
func PaymentBlocksAction(inv Invoice, action InvoiceAction) PaymentBlock {
	if !inv.PaymentInFlight() {
		return PaymentBlock{}
	}

	// An unpaid top-up is a draft, so the write-off below does not apply to
	// it (only a finalized invoice can be written off): while its charge is
	// in flight there is nothing an operator can do but wait for Stripe.
	if inv.UnpaidTopup() {
		return PaymentBlock{
			Blocked: true,
			Code:    "payment_in_flight",
			Message: "Hardcount is checking this top-up's charge with Stripe. Nothing else can be done to it until Stripe answers.",
		}
	}

	// An unclear charge (payment status unknown) can be
	// written off: the deliberate carve-out, for a customer who paid another
	// way. Writing it off moves no money, and a payment Stripe completes later
	// still settles the invoice as paid. It is also the exit when Stripe cannot
	// be asked at all (the key revoked, the PaymentIntent gone from the
	// account). A charge request that may still reach Stripe is refused by
	// PaymentBlocksActionHeld's live-hold rule.
	if inv.PaymentStatus == PaymentUnknown && action == ActionMarkUncollectible {
		return PaymentBlock{}
	}

	return PaymentBlock{
		Blocked: true,
		Code:    "payment_in_flight",
		Message: inFlightMessage(action),
	}
}

// PaymentBlocksActionHeld is PaymentBlocksAction plus the charge hold, for
// the three actions that settle what is owed: void, write-off and offline
// payment (#975, ADR-127). The charge hold
// (invoices.auto_charge_claimed_until) is the claim that a charge, or
// dunning's final action, writes on the invoice while it works. hold is the
// invoice's hold, read in the same transaction as inv with the invoice row
// locked (ChargeHeldTx). On any other copy it answers a question about the
// past. The card settlement path must never call this, because the charge
// that holds the hold must be able to record its own success.
//
// The hold counts as in flight because a charge claim writes only the hold.
// payment_status stays 'pending' or 'failed' until the charge has created its
// PaymentIntent and put it on the invoice as 'unknown' (ADR-137), so until
// then PaymentBlocksAction alone sees nothing. A void or an offline payment
// that landed in that gap would make the charge's stamp fail, and nothing
// would be confirmed; refusing them here spares the charge.
//
// How the hold is read:
//   - On a 'pending' or 'failed' invoice, a live hold refuses every action.
//     A stale hold refuses too (ADR-127), until the stale-hold sweep clears
//     it, within about an hour: its charger is gone, and the sweep moves the
//     counter in the same write so the next charge sends a new key.
//     Write-off is the one action a stale hold allows.
//     It moves no money, a payment the sweep finds later still settles the
//     invoice (uncollectible → paid), and it is the exit when Stripe can
//     never answer (disconnected, the customer deleted). This is ADR-107's
//     carve-out reasoning.
//   - On an 'unknown' invoice only a live hold counts: write-off waits
//     while the charge that left the outcome unclear may still be resending
//     to Stripe (ADR-127 amendment 2026-10-29, ADR-137).
//   - Any other invoice ignores the hold. PaymentBlocksAction already
//     decides 'processing'.
func PaymentBlocksActionHeld(inv Invoice, action InvoiceAction, hold ChargeHoldState) PaymentBlock {
	if b := PaymentBlocksAction(inv, action); b.Blocked {
		return b
	}
	if inv.PaymentStatus == PaymentUnknown && hold == ChargeHoldLive {
		return PaymentBlock{Blocked: true, Code: "payment_in_flight", Message: ChargeHoldRefusalMessage}
	}
	if inv.PaymentStatus != PaymentPending && inv.PaymentStatus != PaymentFailed {
		return PaymentBlock{}
	}
	switch {
	case hold == ChargeHoldLive,
		hold == ChargeHoldStale && action != ActionMarkUncollectible:
		return PaymentBlock{
			Blocked: true,
			Code:    "payment_in_flight",
			// The same words for a live and a stale hold: the operator's next
			// step is the same (wait), and a stale hold is resolved within
			// about an hour without them.
			Message: ChargeHoldRefusalMessage,
		}
	}
	return PaymentBlock{}
}

// ChargeHoldRefusalMessage is what an operator reads when an action is refused
// because a charge holds the invoice. The hold is live, or stale and waiting
// for the stale-hold sweep to clear it (ADR-127, ADR-137).
const ChargeHoldRefusalMessage = "A charge on this invoice is being confirmed with Stripe. Try again in a few minutes; an interrupted charge is cleared within about an hour."

// inFlightMessage says why the action is refused while a charge resolves,
// and that waiting for its outcome is the next step.
func inFlightMessage(action InvoiceAction) string {
	const why = "a charge on this invoice is still being confirmed by the payment provider"
	switch action {
	case ActionVoid:
		return why + " — wait for it to report an outcome before voiding, so a paid invoice is not annulled"
	case ActionMarkUncollectible:
		return why + " — wait for it to report an outcome before writing this off"
	case ActionRecordOfflinePayment:
		return why + " — wait for it to report an outcome before recording an offline payment"
	case ActionIssueCreditNote:
		return why + " — wait for it to report an outcome before crediting, so money that was never collected is not refunded"
	case ActionCollectPayment:
		return why + " — retry once it resolves"
	case ActionResendSetupLink, ActionEmailInvoice:
		return why + " — wait for it to report an outcome before asking the customer to pay again"
	default:
		return why + " — wait for it to report an outcome"
	}
}

// RecoveryWarnsOnOfflinePayment returns what an operator must be told before
// recording an offline payment on a written-off invoice. The rule for this
// area is:
//
//	refuse to create a bad money event; never refuse to record one that
//	already happened.
//
// A card charge is Hardcount choosing to move money, so Hardcount refuses a
// bad one. An offline payment means the wire already landed. Refusing to
// record it would only make the books wrong as well, on top of a payment
// that exists either way.
//
// Recording the payment can still call for a warning. Both conditions below
// mean the recorded payment does not reconcile with what the tenant has told
// the tax authority, or with what was already billed elsewhere. Nothing
// downstream will notice: MarkPaid sets status to 'paid', which takes the
// invoice out of the tax-reversal sweep's own predicate
// (status IN ('voided','uncollectible')). The mismatch becomes permanently
// invisible the moment it is created. So the operator must be told before
// acting, on the invoice, not in a log line an engineer reads later.
//
// It returns nil when there is nothing to say, which is almost always.
func RecoveryWarnsOnOfflinePayment(inv Invoice, unreliefedClawbackCents int64) *PaymentBlock {
	if inv.Status != InvoiceUncollectible {
		return nil
	}
	// Tax was reversed at write-off and cannot be re-reported automatically.
	// The check keys on the reversal having happened (stamp set, or a
	// stripe_tax invoice carrying tax whose committed transaction is gone),
	// not on the invoice merely having tax. An invoice whose tax was never
	// committed (a commit orphan) gets no warning. That is a different defect
	// with a different fix, and a warning about a reversal that never
	// happened would be false.
	if inv.TaxProvider == "stripe_tax" && inv.TaxAmountCents > 0 && inv.TaxReversedAt != nil {
		return &PaymentBlock{
			Code:    "tax_reversed_unrecoverable",
			Message: "This invoice's tax was reversed with the tax provider when it was written off. Recording this payment does not re-report it, so the tax collected here will not appear in your provider's records — correct it with your provider.",
		}
	}
	// No threshold case. A written-off threshold invoice keeps its window
	// billed (ADR-066 amendment 2026-10-23). No later invoice bills that
	// usage again, so a payment on it settles the window once.
	if unreliefedClawbackCents > 0 {
		return &PaymentBlock{
			Code: "relief_not_reissued",
			// The message is phrased for the moment it is read. By the time an
			// operator records a wire, the customer has already sent whatever
			// they were invoiced. So the over-collection has happened and
			// cannot be prevented here. What can still be acted on is the debt
			// it creates in the other direction. That is why the sentence ends
			// on issuing the credit, not on stopping the payment.
			Message: "This invoice is owed a credit that was never applied, so the amount shown is higher than what the customer actually owed. If they paid this amount they have overpaid — issue the credit so it is returned or offset against their next invoice.",
		}
	}
	return nil
}
