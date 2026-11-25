package domain

import "time"

// AttentionSeverity is the urgency of an Attention surface. Operators
// sort/filter on this and the dashboard renders one colour per level.
//
// Priority (descending): Critical > Warning > Info.
//
// Severity is implicit elsewhere in the industry (Stripe/Recurly convey
// it via lifecycle state); shipping it explicit is a small "industry-
// grade plus" that drives chip colour and queue ordering without
// breaking precedent.
type AttentionSeverity string

const (
	AttentionSeverityInfo     AttentionSeverity = "info"
	AttentionSeverityWarning  AttentionSeverity = "warning"
	AttentionSeverityCritical AttentionSeverity = "critical"
)

// AttentionReason is the closed code naming why an invoice needs operator
// attention. It is part of the public API (the AttentionReason enum in
// api/openapi.yaml): a code is never given another meaning, and a retired
// code stays reserved. The dashboard switches on it. The open code,
// Attention.Code, names the exact cause (for example
// tax.customer_data_invalid).
//
// ADR-009 reserves three codes that are never sent: finalization_failed,
// payment_action_required and payment_method_required. commit_exposure is
// retired: only a top-up's payment grants prepaid credit, so no unpaid
// invoice has any. Do not use them for another meaning.
type AttentionReason string

const (
	// AttentionReasonTaxCalculationFailed: Stripe Tax (or another
	// provider) couldn't compute tax for this invoice. Underlying cause
	// (jurisdiction unsupported, provider outage, auth, unknown) lives
	// in Attention.Code. Severity = Warning while retries are pending,
	// Critical once exhausted.
	AttentionReasonTaxCalculationFailed AttentionReason = "tax_calculation_failed"

	// AttentionReasonTaxLocationRequired: customer's billing profile is
	// missing or malformed in a way the tax provider needs (postal_code
	// for US, country, tax_id). Distinguished from the generic
	// tax_calculation_failed because the operator action is concrete:
	// edit the customer's address. Mirrors Stripe's
	// `customer_tax_location_invalid` / `requires_location_inputs`.
	AttentionReasonTaxLocationRequired AttentionReason = "tax_location_required"

	// AttentionReasonPaymentFailed: Stripe (or another payment provider)
	// declined the charge. Underlying decline_code in Attention.Code,
	// human message in Attention.Message.
	AttentionReasonPaymentFailed AttentionReason = "payment_failed"

	// AttentionReasonPaymentUnconfirmed: charge attempt returned an
	// ambiguous error (5xx, timeout) — the reconciler resolves it once
	// Stripe reports a terminal outcome (in-flight PIs are skipped).
	// Hardcount-specific (no direct industry parity); shipped because the
	// PaymentIntent + reconciler architecture creates this state and
	// operators benefit from a quiet "the system has it" signal rather
	// than silence.
	AttentionReasonPaymentUnconfirmed AttentionReason = "payment_unconfirmed"

	// AttentionReasonPaymentAnomaly: the settle path detected money that
	// does not reconcile — a second different PaymentIntent succeeded on an
	// already-paid invoice (double charge), the captured amount differs
	// from the booked amount, or a payment landed on a voided invoice
	// (ADR-068). Critical, and deliberately surfaced EVEN ON TERMINAL
	// invoices: with auto-refund deferred the operator is the refund
	// mechanism, and the anomaly usually fires on an invoice that is
	// already paid. Attention.Code carries the anomaly kind; Message names
	// both PaymentIntents and the captured amount.
	AttentionReasonPaymentAnomaly AttentionReason = "payment_anomaly"

	// AttentionReasonPaymentProcessing: charge attempt is in flight at
	// the provider — Stripe has accepted the PaymentIntent but hasn't
	// returned a final outcome. Self-resolves; no operator action.
	// Stripe parity: `processing` status on the payment_intent.
	AttentionReasonPaymentProcessing AttentionReason = "payment_processing"

	// AttentionReasonPaymentScheduled: invoice is finalized and unpaid
	// with auto_charge_pending=true (every unpaid invoice is queued by its
	// finalize write, ADR-116) — the auto-charge sweep will collect it on
	// its next tick. Operators can short-circuit with
	// "Charge now". Mirrors Stripe's "Awaiting payment" with auto-retry
	// scheduled state.
	AttentionReasonPaymentScheduled AttentionReason = "payment_scheduled"

	// AttentionReasonCollectionPaused: the invoice is queued for
	// auto-charge but its subscription has pause_collection set, so the
	// sweeps deliberately skip it. Distinct from payment_scheduled
	// because the auto-charge is not scheduled. Reached when a subscription
	// is paused AFTER its invoice was queued, which is what dunning's
	// `pause` final action does to every invoice already in flight, and when
	// an operator finalizes a paused subscription's draft. A
	// dunning run already open on the invoice keeps retrying it through
	// the pause, and the message then says so.
	AttentionReasonCollectionPaused AttentionReason = "collection_paused"

	// AttentionReasonAwaitingPayment: invoice is finalized and unpaid but
	// not queued for collection (auto_charge_pending=false). Since
	// ADR-116 every finalize queues the invoice in the same write, so this
	// is reached only by rows written before that change. Operator can
	// trigger a charge or send a reminder. Kept in the enum: the reason
	// codes are a public contract.
	AttentionReasonAwaitingPayment AttentionReason = "awaiting_payment"

	// AttentionReasonNoPaymentMethod: invoice is finalized and unpaid,
	// and the customer has no PaymentSetup ready. The invoice IS queued
	// (auto_charge_pending) — the sweep re-resolves the payment setup
	// every tick and collects the moment a card attaches (charge-on-
	// attach), and dunning enrollment ages it out. What automation
	// CANNOT do is conjure the card: a card-less retry counts an
	// attempt and reminds, it never collects — so the operator (or the
	// customer, via the emailed setup link) is still the mover that
	// unblocks it. Surfaces distinct from awaiting_payment because the
	// action is concrete: "Add payment method", not "Wait and see".
	// Mirrors Stripe's `customer.invoice.requires_payment_method` flow.
	// (Pre-2026-07-22 this comment claimed "NO retry, NO sweep" — born
	// false: the sweep + dunning enrollment already existed.)
	AttentionReasonNoPaymentMethod AttentionReason = "no_payment_method"

	// AttentionReasonDunningExhausted: invoice is finalized and unpaid,
	// the customer STILL has no payment method, and its dunning run has
	// escalated — automatic recovery ended without collecting, and the
	// policy's final action fired (default: collection paused on the
	// subscription). Distinct from no_payment_method because the
	// reassuring "recovery is running" framing is no longer true: the
	// operator must know the engine has escalated. Charge-on-attach
	// works only while collection is not paused: the pending-charge
	// sweep skips paused subscriptions (NotPausedForCollectionSQL), so after
	// the default pause action collection must be resumed too. Chargebee/Stripe
	// parity: the end-of-dunning terminal is an explicit, visible state.
	AttentionReasonDunningExhausted AttentionReason = "dunning_exhausted"

	// AttentionReasonDisputeLost: the customer disputed a payment on this
	// invoice with their bank and the dispute closed as lost, so the bank
	// took the money back (#979). Hardcount leaves the invoice paid and moves no
	// money; the operator decides what the loss means for the invoice (a
	// credit note for money returned outside Hardcount, or a write-off in their
	// own books). Critical, and surfaced on paid invoices, which is where
	// every dispute lands. Lago parity.
	AttentionReasonDisputeLost AttentionReason = "dispute_lost"

	// AttentionReasonDisputeOpen: the customer's bank holds a chargeback on a
	// top-up's own payment (an inquiry raises none), while the credit the
	// payment bought can still be spent. Critical; it clears when the
	// dispute closes, and a loss raises dispute_lost.
	AttentionReasonDisputeOpen AttentionReason = "dispute_open"

	// AttentionReasonRefundedInStripe: a refund of this invoice's payment was
	// made in Stripe outside Hardcount, and Hardcount did not record it as a
	// credit note (ADR-134): the invoice was not paid yet, the currency
	// differs, it bought prepaid credit, Hardcount first saw the refund more
	// than a day late, or the refund is more than the invoice has left to
	// refund or credit. The invoice, its PDF and the hosted page still say
	// nothing was refunded until the operator records it with a credit note
	// linked to the refund, or dismisses it as handled outside Hardcount.
	// Warning, and surfaced on paid invoices, which is where refunds land. A
	// refund Hardcount did record raises nothing: the books are right.
	AttentionReasonRefundedInStripe AttentionReason = "refunded_in_stripe"

	// AttentionReasonAmountBelowMinimum: Stripe refused to charge the invoice
	// because its amount is below Stripe's minimum charge
	// (ErrBelowMinimumCharge). Nothing was charged, no automatic step will
	// collect it, and the customer was not told, so the operator decides:
	// void it, record a payment made another way, or add the amount to the
	// next invoice by hand. Lago parity (the invoice stays pending).
	AttentionReasonAmountBelowMinimum AttentionReason = BelowMinimumCode
)

// AttentionAction names the operator's recommended next step. Closed
// enum because every code maps to a concrete server endpoint or a
// concrete frontend route, and audit logs key off the code. Mirrors
// the Stripe / Recurly / Chargebee verb cluster (Retry / Edit / Mark
// Paid / Void / Stop Dunning).
type AttentionAction string

const (
	AttentionActionEditBillingProfile AttentionAction = "edit_billing_profile"
	AttentionActionRetryTax           AttentionAction = "retry_tax"
	AttentionActionRetryPayment       AttentionAction = "retry_payment"
	AttentionActionWaitProvider       AttentionAction = "wait_provider"
	AttentionActionRotateAPIKey       AttentionAction = "rotate_api_key"
	// AttentionActionReconcilePayment opens the payment in the provider's
	// dashboard, where the refund or correction is made. Offered on payment
	// anomalies (ADR-068): the operator is the refund mechanism there.
	AttentionActionReconcilePayment   AttentionAction = "reconcile_payment"
	AttentionActionReviewRegistration AttentionAction = "review_registration"
	// AttentionActionChargeNow triggers an immediate auto-charge (calls
	// the existing collect-payment endpoint). Operators use this to
	// short-circuit the scheduler when they want to bill now.
	AttentionActionChargeNow AttentionAction = "charge_now"
	// AttentionActionSendReminder dispatches an invoice email to the
	// customer (calls the existing send-invoice-email endpoint). Used
	// in customer-pay collection mode when the invoice is sitting
	// unpaid and a nudge is appropriate.
	AttentionActionSendReminder AttentionAction = "send_reminder"
	// AttentionActionAddPaymentMethod deep-links the dashboard to the
	// customer's billing surface to attach a PM. Surfaced when an
	// invoice is awaiting payment AND the customer has no PaymentSetup
	// ready — the only state where adding a PM unblocks collection.
	AttentionActionAddPaymentMethod AttentionAction = "add_payment_method"
	// AttentionActionUpdatePaymentMethod opens a Stripe-hosted setup
	// flow (Checkout in setup mode) so the customer can replace the
	// card that just declined. Distinct from AddPaymentMethod (which
	// deep-links the dashboard for the no-PM case): there's already a
	// PM on file but it's broken — the verb is "replace this card",
	// not "set up your first card". Surfaced on payment.declined.
	// Operator clicking it opens the send-setup-link dialog
	// (email the customer / copy the link) backed by the operator
	// setup-session endpoint — the customer completes the Stripe-
	// hosted setup themselves; nothing opens in the operator's tab.
	AttentionActionUpdatePaymentMethod AttentionAction = "update_payment_method"
	// AttentionActionConnectTaxProvider deep-links the dashboard to
	// Settings → Payments so the operator can connect the tax
	// provider's credentials. Distinct from RotateAPIKey: nothing
	// to rotate, the connection has never been made.
	AttentionActionConnectTaxProvider AttentionAction = "connect_tax_provider"
	// AttentionActionVoidInvoice opens the invoice's void confirmation.
	// Offered only on an amount below Stripe's minimum charge (which Stripe
	// will never charge): void destroys an invoice, so it must never become a
	// generic "something is wrong here" suggestion.
	AttentionActionVoidInvoice AttentionAction = "void_invoice"
)

// AttentionActionItem is one entry in Attention.Actions. Frontend
// renders one button per item; first item is the primary CTA, the
// rest are secondary. Label is a default rendering — UIs are free to
// substitute localized copy.
type AttentionActionItem struct {
	Code  AttentionAction `json:"code"`
	Label string          `json:"label,omitempty"`
}

// Attention is the unified "this invoice needs operator attention"
// surface. Computed on read by classifyInvoiceAttention in internal/invoice; never
// persisted. Durable fields (tax_status, tax_error_code,
// payment_status, last_payment_error) are the source
// of truth.
//
// Wire shape mirrors Stripe's last_finalization_error /
// last_payment_error envelope (`code`, `message`, `doc_url`, `param`)
// for parity with what design partners already integrate against, plus
// three Hardcount additions:
//
//   - reason   — closed enum the dashboard switches on
//   - severity — closed three-value enum for sort/filter
//   - actions  — prescribed verbs so third-party UIs and audit logs
//     have a machine-readable affordance for "what to do next"
//
// On the wire, the field is omitted entirely when the invoice is
// healthy (matches Stripe's `last_finalization_error: null` ergonomic
// — clients render `if (invoice.attention)` without nested checks).
type Attention struct {
	Severity AttentionSeverity `json:"severity"`
	Reason   AttentionReason   `json:"reason"`
	// Message is what the operator reads first: one plain sentence of 25
	// words or fewer saying what happens and what to do. Facts that must show
	// up front follow as more lines after "\n", one sentence each, up to
	// two lines in all, such as the setup-link email on a no-payment-method
	// or recovery-ended notice. How the engine works and longer instructions
	// go in Detail.
	Message string                `json:"message"`
	Actions []AttentionActionItem `json:"actions,omitempty"`

	// Code is the open, dotted, provider-specific code for programmatic
	// clients. New codes ship without a major-version bump. Examples:
	// tax.customer_data_invalid, tax.provider_outage, payment.declined.
	// Stripe parity (their `code` field is also open).
	Code string `json:"code,omitempty"`

	// DocURL deep-links to the operator-facing documentation page for
	// this reason. Stripe parity. Empty when no doc page exists yet.
	DocURL string `json:"doc_url,omitempty"`

	// Param points at the entity field the operator must edit to
	// resolve the issue, in dotted-path form (e.g.
	// "customer.address.postal_code"). Stripe parity — lets the
	// dashboard's Edit CTA deep-link to the broken field. Empty when
	// the resolution isn't field-scoped.
	Param string `json:"param,omitempty"`

	// Detail is Hardcount's own explanation behind Message — always operator-
	// safe and always our framing: why the state holds, what Hardcount does on
	// its own, and ids support may ask for. The dashboard shows it in the
	// notice's collapsed Details, so nothing the operator must act on lives
	// only here. NOT a place for upstream provider payloads — those go in
	// ProviderResponse so the UI can label them honestly. May be empty when
	// Message + actions cover everything.
	Detail string `json:"detail,omitempty"`

	// ProviderResponse is the raw upstream payload from a third-party
	// service (Stripe Tax JSON envelope, Stripe PaymentIntent
	// last_payment_error body, etc.). Populated ONLY when we actually
	// made an HTTP call and received a response. Pre-flight
	// classification errors (e.g. tax.provider_not_configured —
	// where we never called) leave this empty.
	//
	// The UI labels this as "Provider response" in a collapsible
	// section, so the operator knows the bytes literally came from
	// the upstream service, not from Hardcount. Stripe's own API
	// distinguishes error.message (their classification) from
	// error.payment_intent.last_payment_error (the upstream
	// provider's response); this field mirrors that contract.
	ProviderResponse string `json:"provider_response,omitempty"`

	// Since is when the attention condition started — operators triage
	// by age. tax_deferred_at for tax reasons; updated_at as a proxy
	// for payment_failed/unconfirmed (we don't track a precise
	// failed_at timestamp).
	Since *time.Time `json:"since,omitempty"`

	// NextAttemptAt is when the engine will retry an automatic action.
	// Populated ONLY when a real scheduled instant exists — today that
	// is the tax reconciler's tax_next_retry_at while automatic tax
	// retries remain. payment_scheduled deliberately does NOT populate
	// it (the sweep is tick-based; "on its next tick" is honest, a
	// fabricated precise instant is not), and dunning surfaces its own
	// next_action_at on the dunning run. Crucially, this is NOT due_at:
	// due_at is a deadline, not when the engine is scheduled to act.
	// Empty when the system has no automatic next action.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	// DueBy is the invoice's payment deadline (mirrors invoice.due_at).
	// Surfaced separately from NextAttemptAt so the operator-facing
	// banner can render "Due by Jun 16" without claiming the engine
	// will act on that date. Empty when the invoice has no due_at set.
	DueBy *time.Time `json:"due_by,omitempty"`
}
