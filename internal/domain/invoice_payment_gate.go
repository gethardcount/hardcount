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
// There are three cases:
//   - No payment in flight: nothing is blocked here. The caller's own status
//     rules (paid, voided, draft) still apply. They stay out of this function
//     on purpose, because they differ per action.
//   - Parked (payment_status 'unknown' with no PaymentIntent ID): Stripe's
//     answer was unclear and named no payment. Hardcount checks Stripe's list
//     of the customer's payments within a day (ADR-127 amendment 2026-10-20).
//     Only mark-uncollectible is allowed, for a customer who paid another way.
//     Every other action is refused, and the message says what Hardcount does
//     next.
//   - In flight with a known PaymentIntent: the charge is resolving, so every
//     action is refused and the operator is told to wait.
//
// See ADR-107 and ADR-127 for the parked state.
func PaymentBlocksAction(inv Invoice, action InvoiceAction) PaymentBlock {
	if !inv.PaymentInFlight() {
		return PaymentBlock{}
	}

	// Parked: Stripe's answer was unclear and named no payment. Hardcount finds
	// out from Stripe's list of the customer's payments, within a day.
	if inv.Parked() {
		if action == ActionMarkUncollectible {
			// The deliberate carve-out, for a customer who paid another way.
			// Writing it off moves no money, and a payment Stripe completes
			// later still settles the invoice as paid. It is also the exit
			// when Stripe cannot be asked at all (the key revoked, the
			// customer deleted there).
			return PaymentBlock{}
		}
		return PaymentBlock{
			Blocked: true,
			Code:    "payment_unidentifiable",
			Message: parkedMessage(action, inv.Status == InvoiceUncollectible),
		}
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
// The hold counts as in flight because every charge claim writes only the
// hold. payment_status stays 'pending' or 'failed' until Stripe answers, so
// PaymentBlocksAction alone sees nothing while Stripe may be taking the
// money. A void then leaves a successful charge on a voided invoice, and an
// offline payment makes the customer pay twice.
//
// How the hold is read:
//   - On a 'pending' or 'failed' invoice, a live hold refuses every action.
//     A stale hold refuses too (ADR-127): its charge may have been sent and
//     its result lost, so Stripe may hold a payment Hardcount has not
//     recorded. It stays set until the stale-hold sweep has asked Stripe,
//     within about an hour. Write-off is the one action a stale hold allows.
//     It moves no money, a payment the sweep finds later still settles the
//     invoice (uncollectible → paid), and it is the exit when Stripe can
//     never answer (disconnected, the customer deleted). This is ADR-107's
//     carve-out reasoning.
//   - On a parked invoice, only a live hold counts. A park keeps its claim's
//     hold, so a stale hold there is the normal wait. The stale-hold sweep
//     asks Stripe about a parked invoice within a day. Write-off stays
//     allowed during that wait: the exit for a customer who paid another way
//     (ADR-107). A live hold on a park belongs to a charge request made for
//     it. Today that is the charge that parked it, in its first five
//     minutes. Once the stale-hold sweep resends a parked charge, it is that
//     resend. A write-off must not land while such a request may still reach
//     Stripe, or the request can charge an invoice written off because the
//     customer paid another way. So write-off waits a few minutes there, like
//     every other action (ADR-127 amendment 2026-10-29).
//   - Any other invoice ignores the hold. PaymentBlocksAction already
//     decides 'processing'.
func PaymentBlocksActionHeld(inv Invoice, action InvoiceAction, hold ChargeHoldState) PaymentBlock {
	if b := PaymentBlocksAction(inv, action); b.Blocked {
		return b
	}
	if inv.Parked() && hold == ChargeHoldLive {
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
// for the stale-hold sweep to confirm it with Stripe (ADR-127).
const ChargeHoldRefusalMessage = "A charge on this invoice is being confirmed with Stripe. Try again in a few minutes; an interrupted charge is confirmed within about an hour."

// parkedMessage says why the action is refused while Stripe may still
// complete the charge, and what Hardcount does next on its own (ADR-127
// amendment 2026-10-20). Hardcount records a payment that appears, and it
// charges again as usual when none has appeared 24 hours after the attempt.
// The message ends by naming mark-uncollectible, the one action allowed
// meanwhile, for a customer who paid another way. alreadyWrittenOff swaps
// that ending, because the advice is impossible on an invoice that already
// is uncollectible. Writing off a parked invoice is the carve-out in
// PaymentBlocksAction.
func parkedMessage(action InvoiceAction, alreadyWrittenOff bool) string {
	const why = "Stripe didn't confirm this invoice's last charge and may still complete it"
	if alreadyWrittenOff {
		return why + ". The invoice is already written off, so Hardcount will not charge it again. If Stripe completes the charge, the payment is recorded here"
	}
	const next = ". Hardcount checks Stripe every hour and records the payment if it appears; if none appears within 24 hours, the invoice is charged again as usual. If the customer paid another way, mark the invoice uncollectible instead"
	switch action {
	case ActionVoid:
		return why + ", and voiding it could annul an invoice that is in fact paid" + next
	case ActionRecordOfflinePayment:
		return why + ", and recording an offline payment would label a card charge as out-of-band" + next
	case ActionIssueCreditNote:
		return why + ", and crediting it could refund money that was never collected" + next
	case ActionCollectPayment:
		return why + ", and charging again now risks a second charge" + next
	case ActionResendSetupLink:
		return why + ", and that email tells the customer we will collect automatically while this charge is unresolved" + next
	case ActionEmailInvoice:
		// The invoice email gets its own sentence. Its call to action is
		// "View & pay invoice", not a promise to collect, so the
		// resend-setup-link sentence would describe the wrong email. Sending
		// it is wrong because it asks for money we may already have taken,
		// and it lands the customer on a page with no Pay button.
		return why + ", and that email asks the customer to pay an invoice we may already have charged them for, and the hosted page will not offer them a Pay button when they arrive" + next
	default:
		return why + next
	}
}

// inFlightMessage covers the case where the charge is really resolving, so
// waiting is the right advice. That advice differs from the parked case on
// purpose.
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

// There is no RecoveryBlocksCharge: nothing charges a written-off invoice
// (ADR-113), so no check for that lives here. Charging in place would aim
// money at an invoice that carries stale state. A customer who returns later
// gets a fresh recovery invoice on the normal payment paths, and a fresh
// invoice carries no stale state.
// RecoveryWarnsOnOfflinePayment below still applies: recording money that
// already arrived is never refused, but the operator is told when it will
// not reconcile.

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
