package domain

// InvoiceAction names an operator action whose availability depends on the
// invoice's PAYMENT state.
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
	// Message is operator-facing and must always answer BOTH halves: why this
	// is refused, and what to do instead. A refusal that names no alternative
	// is how an operator ends up stuck with an invoice and no next step.
	Message string `json:"message,omitempty"`
}

// PaymentBlocksAction is the SINGLE SOURCE for whether an invoice's payment
// state blocks an operator action.
//
// It exists because this one rule used to be re-expressed by hand in eight
// independent places — five service guards, two HTTP handlers and the
// dashboard's own menu conditions — each with its own wording.
//
// CORRECTION (2026-07-31): when that sentence was first written only the seven
// GO sites had been collapsed. The dashboard was named as if it derived from
// here and did not: it went on hand-writing `payment_status !== 'succeeded' &&
// !== 'processing'` at four call sites, every one omitting 'unknown', so a
// parked invoice rendered a green "Collect Payment" button directly beneath a
// Critical banner saying no further charge would be attempted. The scanner
// below could not catch it because it walked only .go files — the mechanisation
// was as good as its SCOPE, and its scope excluded the surface the comment was
// boasting about. The dashboard now derives from a single frontend helper
// (web-v2/src/lib/status.ts paymentIsUnresolved) and the scanner walks .ts/.tsx.
// The durable version is the server returning per-action availability so the
// dashboard derives rather than mirrors; one mirror beats four copies. When ADR-107
// changed what payment_status='unknown' MEANS (from a transient state that
// resolves in seconds to one that may never resolve), all eight became wrong
// independently, and finding them took a four-agent sweep plus a manual walk of
// the UI. Two of them still disagreed with each other afterwards: void said
// "wait for it to settle or cancel it", credit-note said "settle or cancel
// first, or wait for charge reconciliation" — the same rule, two authors, two
// sets of impossible advice.
//
// The fix is not better wording. It is having one statement of the rule that
// every surface derives from, so the next change to what a payment state means
// cannot leave seven stale copies behind.
//
// Three cases:
//
//   - NOT in flight — nothing here blocks; the caller's own status rules
//     (paid/voided/draft) still apply and are deliberately NOT folded in, since
//     those genuinely differ per action.
//   - PARKED (ADR-107: 'unknown' with no PaymentIntent id) — Stripe's answer
//     was unclear and named no payment. Hardcount finds out from Stripe's list of
//     the customer's payments within a day (ADR-127 amendment 2026-10-20).
//     Mark-uncollectible is ALLOWED here, for a customer who paid another way;
//     everything else is refused and says what Hardcount does next.
//   - IN FLIGHT with a known PaymentIntent — genuinely resolving, so waiting is
//     the correct advice.
//
// EXTENDING THIS IF ADR-106's LEDGER IS EVER RESUMED. That design adds a
// charge_intents row whose state answers the same question more precisely, and
// this function is where it plugs in — not into the seven call sites:
//
//	intent open        → an attempt is in flight and recovery is actively
//	                     retrying it, so this is the WAIT case, even though the
//	                     invoice has no PaymentIntent id yet. Today that shape
//	                     reads as parked; the ledger is exactly what lets it be
//	                     told apart.
//	intent needs_review → recovery gave up, so it is the PARKED case: refuse
//	                     everything but mark-uncollectible and say so.
//
// That is one branch here, and every surface — five service guards, two HTTP
// handlers, the attention banner, the dashboard menu — inherits it. The reason
// to say this out loud is that two adversarial review rounds found the ledger's
// needs_review state had NO operator exit and no honest copy, and the cause was
// structural: expressing a give-up state used to mean writing eight strings and
// wiring a menu, so it got written zero times. Extend the function; do not
// re-scatter the rule.
//
// The gate needs ONE more fact, and where that fact enters is the real decision.
// Two ways in, and they are not equally priced:
//
//   - ON THE READ MODEL (preferred): the invoice store LEFT JOINs the unresolved
//     charge_intents row and populates one more field, exactly as the invoice
//     already carries stripe_payment_intent_id, payment_status and
//     charge_attempt_seq. This signature does not change, the seven callers do
//     not change, and no domain boundary is crossed. Costs one join on the
//     invoice read path, hitting at most one row (the partial unique index on
//     unresolved intents already exists).
//   - VIA THE SIGNATURE: pass the intent state as a parameter. The edit here is
//     trivial, but every caller holds only an Invoice, so each must LOAD the
//     intent — and internal/invoice reaching into internal/payment is the
//     cross-domain coupling internal/arch/boundaries_test.go fails the build
//     over. It threads a new dependency through five services and two handlers
//     to answer a question they already believe they can answer.
//
// Neither is pre-built here. Speculative parameters for a parked design are how
// the ledger reached 1,900 lines in the first place.
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

// PaymentBlocksActionHeld is PaymentBlocksAction plus the charge hold, for the
// three actions that settle what is owed: void, write-off and offline payment
// (#975, ADR-127). hold is the invoice's hold read in the same transaction as
// inv, with the invoice row locked (ChargeHeldTx). Called on any other copy it
// answers a question about the past.
//
// Why the hold counts as in flight here: every charge claim writes only the
// hold, and payment_status stays 'pending' or 'failed' until Stripe answers,
// so PaymentBlocksAction alone sees nothing while Stripe may be taking the
// money. A void that lands there leaves a successful charge on a voided
// invoice; an offline payment makes the customer pay twice.
//
// A STALE hold refuses too (ADR-127): its charge may have been sent and its
// result lost, so Stripe may hold a payment Hardcount has not recorded. It stays
// set until the stale-hold sweep has asked Stripe, within about an hour (a
// parked invoice's within a day, but 'unknown' is decided above).
// Write-off is the one action a stale hold allows: it moves no money, a
// payment the sweep finds later still settles the invoice (uncollectible →
// paid), and it is the exit when Stripe can never answer (disconnected, the
// customer deleted) — ADR-107's carve-out reasoning.
//
// Only 'pending' and 'failed' consult the hold. 'processing' and 'unknown' are
// decided above, and an unknown outcome keeps its hold, so applying the hold
// to 'unknown' would refuse the write-off of a parked invoice — the exit for a
// customer who paid another way (ADR-107).
//
// The card settlement path must never call this: the charge holding the hold
// has to be able to record its own success.
func PaymentBlocksActionHeld(inv Invoice, action InvoiceAction, hold ChargeHoldState) PaymentBlock {
	if b := PaymentBlocksAction(inv, action); b.Blocked {
		return b
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
// because a charge holds the invoice — live, or stale and waiting for the
// stale-hold sweep to confirm it with Stripe (ADR-127).
const ChargeHoldRefusalMessage = "A charge on this invoice is being confirmed with Stripe. Try again in a few minutes; an interrupted charge is confirmed within about an hour."

// parkedMessage says why the action is refused while Stripe may still
// complete the charge, what Hardcount does next on its own (ADR-127 amendment
// 2026-10-20: records a payment that appears, and charges again as usual when
// none has appeared 24 hours after the attempt), and ends by naming
// mark-uncollectible: the one action allowed meanwhile, for a customer who paid
// another way. alreadyWrittenOff swaps the tail: that advice is impossible on
// an invoice that already is uncollectible, and writing off a parked invoice
// is the carve-out above.
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
		// Split from resend-setup-link after a walk read this refusal back:
		// the invoice email's call to action is "View & pay invoice", not a
		// promise to collect, so the shared sentence described the wrong
		// email. What makes sending it wrong is that it asks for money we may
		// already have taken, and lands the customer on a page with no Pay
		// button.
		return why + ", and that email asks the customer to pay an invoice we may already have charged them for, and the hosted page will not offer them a Pay button when they arrive" + next
	default:
		return why + next
	}
}

// inFlightMessage covers the genuinely-resolving case, where waiting IS the
// right advice — deliberately different from the parked case, which is the
// distinction that kept being lost.
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

// RecoveryBlocksCharge was removed by ADR-113 (2026-08-06). It was the
// single source for whether a WRITTEN-OFF invoice could be CHARGED — three
// refusals (tax reversed / threshold re-billed / relief unapplied) that
// existed only because charge-in-place aimed money at an object carrying
// stale state. Industry evidence killed the feature, not the gates: 1 of 6
// verified platforms (Stripe alone) charges the written-off object; the
// majority reverse the write-off artifact or have no such state, and
// recovery runs on NORMAL rails. Hardcount's shape for that is a fresh recovery
// invoice, which has none of the three hazards. RecoveryWarnsOnOfflinePayment
// below SURVIVES: recording money that already arrived is never refused, but
// the operator is told when it will not reconcile.

// RecoveryWarnsOnOfflinePayment is the OFFLINE twin of RecoveryBlocksCharge,
// and the difference between them is the whole rule for this area:
//
//	refuse to CREATE a bad money event; never refuse to RECORD one that
//	already happened.
//
// A card charge is Hardcount choosing to move money, so those conditions BLOCK. An
// offline payment means the wire already landed — refusing to record it would
// only make the books wrong too, on top of a payment that exists either way.
//
// But "do not refuse" is not "say nothing". Both conditions below mean the
// recorded payment does not reconcile with what the tenant has told the tax
// authority, or with what was already billed elsewhere — and nothing
// downstream will ever notice, because MarkPaid flips status to 'paid', which
// drops the invoice out of the tax-reversal sweep's own predicate
// (status IN ('voided','uncollectible')). The discrepancy becomes permanently
// invisible at the instant it is created. So the operator has to be told
// BEFORE they act, on the invoice, not in a log line an engineer reads later.
//
// Returns nil when there is nothing to say — the overwhelming majority.
func RecoveryWarnsOnOfflinePayment(inv Invoice, unreliefedClawbackCents int64) *PaymentBlock {
	if inv.Status != InvoiceUncollectible {
		return nil
	}
	// Tax was reversed at write-off and cannot be re-reported automatically.
	// Keyed on the reversal having HAPPENED (stamp set, or a stripe_tax invoice
	// carrying tax whose committed transaction is gone), not on the invoice
	// merely having tax.
	// Same key as the block above: the reversal having happened. The old
	// `|| TaxTransactionID == ""` arm fired on invoices whose tax was never
	// COMMITTED (a commit orphan) — a different defect with a different fix,
	// and warning about a reversal that never happened is a false statement.
	if inv.TaxProvider == "stripe_tax" && inv.TaxAmountCents > 0 && inv.TaxReversedAt != nil {
		return &PaymentBlock{
			Code:    "tax_reversed_unrecoverable",
			Message: "This invoice's tax was reversed with the tax provider when it was written off. Recording this payment does not re-report it, so the tax collected here will not appear in your provider's records — correct it with your provider.",
		}
	}
	// No threshold case. A written-off threshold invoice keeps its window
	// billed (ADR-066 amendment 2026-10-23): no later invoice bills that
	// usage again, so a payment on it settles the window once. The
	// recovery_superseded warning that said otherwise is gone.
	if unreliefedClawbackCents > 0 {
		return &PaymentBlock{
			Code: "relief_not_reissued",
			// Phrased for the moment it is READ. By the time an operator is
			// recording a wire the customer has already sent whatever they were
			// invoiced — so the over-collection has happened and cannot be
			// prevented here. What IS still actionable is the debt it creates in
			// the other direction, which is why the sentence ends on issuing the
			// credit rather than on stopping the payment.
			Message: "This invoice is owed a credit that was never applied, so the amount shown is higher than what the customer actually owed. If they paid this amount they have overpaid — issue the credit so it is returned or offset against their next invoice.",
		}
	}
	return nil
}
