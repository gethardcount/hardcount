package domain

import (
	"fmt"
	"strings"
	"time"
)

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

// AttentionReason is the closed, typed code naming *why* an invoice
// needs operator attention. Public-API contract — once shipped, codes
// are never repurposed; deprecations keep the code reserved and add a
// successor.
//
// Closed enum so the dashboard can switch on it for icon, copy, and
// CTA layout. The open variant — Attention.Code — carries the
// underlying provider-specific reason (e.g. tax.customer_data_invalid)
// for programmatic clients.
//
// Anchored to industry precedent: Stripe's invoice event-name
// granularity (invoice.payment_failed, invoice.finalization_failed),
// Chargebee's dunning_status, Lago's payment_status. Five codes today
// matching the signal we actually have; the enum is open for
// extension as we ship dunning, disputes, and 3DS support.
//
// Reserved-but-not-yet-emitted (planned, do not reuse):
//
//	finalization_failed         — non-tax finalization error
//	payment_action_required     — 3DS / SCA pending
//	payment_method_required     — no PM on file
//	dunning_exhausted           — shipped 2026-07-22 (payment-surfacing audit)
//	dispute_lost                — Lago parity
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
	// Velox-specific (no direct industry parity); shipped because the
	// PaymentIntent + reconciler architecture creates this state and
	// operators benefit from a quiet "the system has it" signal rather
	// than silence.
	AttentionReasonPaymentUnconfirmed AttentionReason = "payment_unconfirmed"

	// AttentionReasonCommitExposure: this invoice funded a prepaid-commit
	// grant that is still live, and the invoice was never paid. ADR-078 funds
	// commits at ISSUE (finalize), not at payment — the negotiated-B2B default,
	// deliberately chosen and unchanged — which means the customer holds
	// spendable credit against cash that has not arrived.
	//
	// That disposition is fine; its INVISIBILITY was not. The only recovery is
	// operator void, which retires the REMAINING balance only (consumed stays
	// consumed), so every hour the customer keeps drawing is recovery
	// permanently lost. Nothing surfaced the state, so nobody could void in
	// time — observed live on VLX-000060: $96.52 invoiced, $0.00 collected,
	// $100.00 still drawable, dunning since Jul 30, and no surface said so.
	// docs/design/prepaid-commits.md D3 named this exact surfacing a
	// "fast-follow candidate"; this is that fast-follow.
	//
	// This reason is only ever SYNTHESIZED when nothing else fired — see
	// withCommitExposure, which folds the exposure into whichever cause banner
	// won rather than competing with it. A commit invoice is unpaid-with-live-
	// grant from finalize until payment, so a reason that pre-empted the chain
	// would fire on every healthy Net-30 commit sale AND mask the declined card
	// underneath it. The fold is what makes it safe to report a state that is,
	// most of the time, simply the deal working as agreed.
	AttentionReasonCommitExposure AttentionReason = "commit_exposure"

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
	// because nothing is scheduled — saying otherwise promises a charge
	// that will never fire. Reached when a subscription is paused AFTER
	// its invoice was queued, which is what dunning's `pause` final
	// action does to every invoice already in flight.
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
	// sweep skips paused subscriptions (notPausedForCollection), so after
	// the default pause action collection must be resumed too. Chargebee/Stripe
	// parity: the end-of-dunning terminal is an explicit, visible state.
	AttentionReasonDunningExhausted AttentionReason = "dunning_exhausted"
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
	// AttentionActionVoidInvoice opens the invoice's void confirmation. It is
	// the ONLY recovery for an unpaid invoice whose prepaid credit is still
	// live: voiding retires the unspent balance inside the void tx (ADR-078
	// D3). Offered on commit exposure alone — void destroys an invoice, so it
	// must never become a generic "something is wrong here" suggestion.
	//
	// Reusing an existing code with a relabelled button was rejected: audit
	// logs key off the code, so an action reading `add_payment_method` on a
	// credit-exposure banner would be a lie in the machine-readable field, and
	// unlike the label nothing downstream would ever correct it.
	AttentionActionVoidInvoice AttentionAction = "void_invoice"
	// AttentionActionMarkUncollectible opens the invoice's write-off
	// confirmation. Offered only on a PARKED invoice (payment status unknown
	// and no PaymentIntent to look up, ADR-107): there it is the one action
	// the product allows, and before this code existed the notice could only
	// name a menu item.
	AttentionActionMarkUncollectible AttentionAction = "mark_uncollectible"
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
// surface. Computed on read by ClassifyInvoiceAttention; never
// persisted. Durable fields (tax_status, tax_error_code,
// payment_status, last_payment_error) are the source
// of truth.
//
// Wire shape mirrors Stripe's last_finalization_error /
// last_payment_error envelope (`code`, `message`, `doc_url`, `param`)
// for parity with what design partners already integrate against, plus
// three Velox additions:
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
	// three lines in all: the setup-link email on a no-payment-method or
	// recovery-ended notice, and the prepaid credit at risk on a commit
	// invoice. A no-card commit invoice carries all three. How the engine
	// works and longer instructions go in Detail.
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

	// Detail is Velox's own explanation behind Message — always operator-
	// safe and always our framing: why the state holds, what Velox does on
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
	// the upstream service, not from Velox. Stripe's own API
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

// AttentionContext carries ancillary signals needed by the classifier
// that aren't durable on the Invoice row itself. Today this is just
// the customer's payment-method state, but the struct is the seam for
// future signals (dunning policy retry schedule, collection mode, etc).
// Zero-value is safe — classifier falls back to existing behaviour
// when callers don't have the signal available.
type AttentionContext struct {
	// HasPaymentMethod is true when the customer has a PaymentSetup
	// row in `ready` state with a stripe_customer_id. Distinguishes
	// the operator-actionable no_payment_method state from the generic
	// awaiting_payment race window.
	HasPaymentMethod bool

	// CustomerHasEmail is true ONLY when the customer confirmably has an
	// email address on file. Consumed only by the no_payment_method
	// classifier: the engine's finalize-time setup-link email
	// (NotifyNoPaymentMethod) skips when there's no address (a typed,
	// reported skip since 2026-07-10 — but per-invoice send state still
	// isn't observable here), so
	// the banner must not claim "we emailed a link" — and must not offer a
	// resend that cannot send — in that case. Zero-value (false) is the
	// conservative default and also covers "couldn't determine" (read
	// error / no reader): the false-branch copy states engine behavior,
	// not this customer's email state, so it stays honest either way. The
	// invoice service populates it from the customer record.
	CustomerHasEmail bool

	// StripeConnected reports whether the invoice's tenant has Stripe
	// credentials connected for this invoice's mode (livemode). Used
	// by the tax-classification path to distinguish two states that
	// previously rendered identically:
	//   - tax_error_code='provider_not_configured' AND Stripe not
	//     connected → operator action required ("Connect Stripe").
	//   - tax_error_code='provider_not_configured' AND Stripe IS now
	//     connected → calculation will retry on the next scheduler
	//     tick (5 min dev, 1 hr prod). The previous "Stripe isn't
	//     connected" copy was misleading during this gap window.
	// Resolution: the operator just connected Stripe; the invoice's
	// stale tax_error_code hasn't been replaced yet because the
	// scheduler hasn't ticked. Banner now reads "calculation queued"
	// + "Retry now" so the operator gets agency without waiting.
	StripeConnected bool

	// DunningEscalated is true when the invoice's dunning run has reached
	// state='escalated' — automatic recovery exhausted its retries and the
	// policy's final action fired. Consumed only by the no_payment_method
	// classifier branch (the invoice service fetches the run lazily under
	// the same finalized-unpaid-no-PM guard as CustomerHasEmail, so list
	// reads don't pay a dunning lookup per row). Zero-value (false) keeps
	// the pre-escalation banner — safe for callers without the signal.
	DunningEscalated bool

	// CollectionPaused is true when the invoice's subscription has
	// pause_collection set. Both auto-charge sweeps skip paused
	// subscriptions, so a queued invoice on one is NOT scheduled — the
	// payment_scheduled copy would promise a charge that never fires.
	// Reached whenever a subscription is paused after its invoice was
	// already queued (dunning's `pause` final action does exactly that).
	// The no-payment-method notice reads it too: "charged once the customer
	// adds a card" is false while collection is paused, so the invoice
	// service populates it for card-less invoices as well as queued ones.
	// Zero-value (false) keeps the scheduled copy — safe for callers
	// without the signal, and the honest default for the common case
	// where nothing is paused.
	CollectionPaused bool

	// SetupLinkEmailOff is true when this server cannot email a setup link:
	// SMTP is not set up, or PAYMENT_UPDATE_URL (the page the link opens) is
	// unset, in which case the sender refuses. Read only by the
	// no-payment-method and recovery-ended notices: with it set, no setup
	// link can have been emailed and Resend would fail, so the notice says so
	// and offers the customer page instead. Zero value (false) means sending
	// works — what every production boot wires explicitly.
	SetupLinkEmailOff bool

	// CommitGrantDrawableCents and CommitGrantConsumedCents describe the
	// prepaid-commit grant this invoice funded (ADR-078). Drawable is what the
	// customer can still spend right now; Consumed is what they already spent
	// and which no recovery can claw back. Both zero when the invoice funded no
	// commit.
	//
	// TWO numbers, not one "outstanding", because they drive different
	// sentences and only one of them is recoverable. A single figure next to a
	// Void button misrepresents what Void does: void retires the UNSPENT
	// balance in the void tx and leaves consumed credit consumed (ADR-078 D3),
	// so an operator shown one number reads it as the amount voiding gets back.
	//
	// Drawable applies the SAME liveness predicate the drawdown itself uses
	// (drainPositiveBlocks: positive, not exhausted, not past expires_at,
	// evaluated against the resolver's now) — so an expired-but-unswept grant
	// reports 0 and the banner cannot claim spendable credit the drain would
	// refuse to spend.
	//
	// Populated lazily, only for the finalized/uncollectible manual invoices
	// that can carry a commit line: one hit on the unique partial index
	// idx_credit_ledger_commit_fund_dedup (tenant_id, source_invoice_id WHERE
	// grant_kind='commit'), never a scan. Zero values yield no attention, which
	// is the safe default for every caller that does not supply the signal and
	// for a read that errored — the alternative, a banner asserting a balance
	// we could not read, is the failure mode worth avoiding.
	CommitGrantDrawableCents int64
	CommitGrantConsumedCents int64

	// Now is wall-clock now, used only to age the in-flight payment banner
	// (processing → Info under the expected-settle window, Warning past it).
	// Staleness is a REAL-WORLD duration (the provider settles in wall-clock),
	// so this is wall-clock, not resolver-bound sim-time. Zero value disables
	// the age escalation (the banner stays Info) — safe for callers that don't
	// set it.
	Now time.Time

	// PaymentPageOpen: the customer has a hosted payment page that may still
	// take their money, or a hosted payment Velox has not recorded yet
	// (HostedCheckoutLiveSQL). While it holds, every Velox charge refuses to
	// start (ADR-068 "one payer at a time"), so a banner offering Retry would
	// offer a button that is refused. The invoice service reads it for
	// finalized, unpaid invoices only.
	PaymentPageOpen bool

	// LastFailedChargeAt is the simulated instant of the invoice's latest
	// failed charge, for a clock-pinned invoice only (nil otherwise). The
	// payment-failure banner dates "since" from it: attentionSince's issue
	// instant is wrong for an invoice charged on a later Advance (a paused
	// subscription's draft finalized later read "since 13d ago" four days
	// after its decline, found walking FLOW TC5, 2026-10-11).
	LastFailedChargeAt *time.Time
}

// processingStaleAfter is how long a payment may sit `processing` before the
// in-flight banner escalates from Info to Warning. Cards confirm in seconds
// (inline, ADR-049 Phase 3) or within the reconciler's window (Phase 2), so a
// card past this is genuinely anomalous — point the operator at Stripe. Tunable;
// will need to go method-specific (cards: hours; ACH/SEPA: days) when an async
// method is enabled (ADR-049 deferred tail).
const processingStaleAfter = 6 * time.Hour

// docURL returns the operator-facing documentation link for an attention
// reason. No such pages are published yet, so it returns "" and doc_url is
// omitted from the response; a link nobody can open is worse than none.
// Call sites keep their slug so the pages can be wired in one place later.
func docURL(slug string) string {
	_ = slug
	return ""
}

// ClassifyInvoiceAttention returns the most-pressing reason this
// invoice needs operator attention, or nil if everything is healthy.
//
// Priority order (first match wins, descending urgency):
//  1. Tax failed          — blocks finalize, Critical
//  2. Payment failed      — money flow broken, Critical
//  3. Tax pending         — engine retrying, Warning
//  4. Payment unconfirmed — reconciler resolves, Info
//  5. Payment processing  — charge in flight at provider, Info
//  6. Payment scheduled   — auto-charge will fire on next tick, Info
//  7. Awaiting payment    — finalized, no charge attempted yet, Info
//
// Terminal-state invoices (paid, voided) never raise attention.
// Drafts also skip — the page itself communicates draft state and
// adding a banner would be redundant noise.
//
// Pure function — no I/O, deterministic on the input Invoice +
// AttentionContext. Tested in invoice_attention_test.go across the
// cartesian product of (status, tax_status, tax_error_code,
// payment_status, auto_charge_pending, has_payment_method).
//
// The atc.HasPaymentMethod signal distinguishes two awaiting sub-
// states: a no-PM invoice (operator-actionable, the engine has nothing
// scheduled) vs a has-PM race window (transient, engine will fire on
// next sweep). Without the context, callers fall through to the
// generic awaiting_payment classification — backwards-compat for
// internal call sites that don't have a PaymentMethodReader handy.
func ClassifyInvoiceAttention(inv Invoice, atc AttentionContext) *Attention {
	return withCommitExposure(classifyAttentionCause(inv, atc), inv, atc)
}

// classifyAttentionCause is the single-winner priority chain: the CAUSE of the
// invoice's trouble, one reason, highest urgency first.
//
// Split out from ClassifyInvoiceAttention so commit exposure can be folded onto
// its result instead of competing inside it. Exposure is not a cause — it is a
// consequence that co-occurs with any of these, and it is present on healthy
// invoices too (a Net-30 commit is unpaid-with-live-credit by design for its
// whole term). A reason that answered "why does this need attention?" with
// "because credit is live" would be both wrong and, since the chain returns the
// FIRST match, would suppress the declined card that is the actual answer.
func classifyAttentionCause(inv Invoice, atc AttentionContext) *Attention {
	// Terminal-ish statuses produce no attention surface. Uncollectible
	// is technically forward-transitionable (operator can still record
	// an offline payment or void it) but Stripe-parity treats it as a
	// settled state for dunning/diagnostic purposes — the operator has
	// already acknowledged "we're not collecting this." Surfacing a
	// payment_failed attention banner on top of that contradicts the
	// operator's decision.
	// Payment anomalies pierce the terminal early-return below (ADR-068):
	// the double-charge / amount-mismatch / paid-a-voided-invoice marker
	// fires almost exclusively on invoices that are ALREADY paid or voided —
	// exactly the rows the early-return hides. The operator must see it to
	// refund it.
	if inv.PaymentAnomalyKind != "" {
		return classifyPaymentAnomaly(inv)
	}
	// A BAD-DEBT RECOVERY charge in flight pierces the early-return, for the
	// same reason the anomaly does: the state lives only on a row the return
	// hides. An operator charged a written-off invoice, the provider has not
	// answered yet, and without this the page shows nothing at all — so a
	// second operator sees a written-off invoice with no sign that money is
	// moving, and charges it again.
	//
	// Deliberately NARROW: `processing` only. A written-off invoice sitting at
	// `failed` must stay silent — that is every dunning-exhausted write-off in
	// the system, and the silence there is a decision, not an oversight.
	if inv.Status == InvoiceUncollectible && inv.PaymentStatus == PaymentProcessing {
		return classifyRecoveryInFlight(inv)
	}
	if inv.Status == InvoicePaid || inv.Status == InvoiceVoided || inv.Status == InvoiceUncollectible {
		return nil
	}
	switch {
	case inv.TaxStatus == InvoiceTaxFailed:
		return classifyTaxAttention(inv, atc, AttentionSeverityCritical)
	// Before the failure and pending states: while the customer's payment page
	// is open no Velox charge can start, so their banners' Retry / Charge now
	// would be refused, and a dunning retry date reads as overdue when it is
	// held.
	case inv.Status == InvoiceFinalized && atc.PaymentPageOpen:
		return classifyPaymentPageOpen()
	case inv.PaymentStatus == PaymentFailed:
		return classifyPaymentFailure(inv, atc)
	case inv.TaxStatus == InvoiceTaxPending:
		return classifyTaxAttention(inv, atc, AttentionSeverityWarning)
	case inv.PaymentStatus == PaymentUnknown:
		return classifyPaymentUnconfirmed(inv)
	case inv.PaymentStatus == PaymentProcessing:
		return classifyPaymentProcessing(inv, atc)
	// no_payment_method beats payment_scheduled: when both flags are
	// set (engine queued for retry but PM is still missing), the
	// scheduler will keep skipping until a PM is attached. The
	// operator-facing reality is "no PM" — that's the actionable
	// reason. Surfacing payment_scheduled instead would falsely tell
	// the operator "engine will retry on its next tick" when the
	// retry will skip again until a PM is attached.
	case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending && !atc.HasPaymentMethod:
		return classifyNoPaymentMethod(inv, atc)
	// collection_paused beats payment_scheduled for the same reason
	// no_payment_method does: the flag says the invoice is queued, but the
	// sweeps skip paused subscriptions, so "the engine will charge this"
	// would be a promise nothing keeps.
	case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending &&
		inv.AutoChargePending && atc.CollectionPaused:
		return classifyCollectionPaused(inv)
	case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending && inv.AutoChargePending:
		return classifyPaymentScheduled(inv)
	case inv.Status == InvoiceFinalized && inv.PaymentStatus == PaymentPending:
		return classifyAwaitingPayment(inv)
	}
	return nil
}

// classifyRecoveryInFlight surfaces a bad-debt recovery charge that an operator
// started on a written-off invoice and the provider has not yet answered.
//
// Info, not Warning: nothing is wrong. The whole point of the banner is that
// the invoice STILL READS as written off everywhere else — status, badge,
// public page — so without it a second operator sees no sign that money is in
// motion and charges the card again.
//
// Reuses AttentionReasonPaymentProcessing rather than minting a reason: the
// fact is the same one that reason already names (a charge is in flight at the
// provider), and the closed reason enum is a public contract. The Code carries
// the recovery-specific granularity, which is what it is for.
func classifyRecoveryInFlight(inv Invoice) *Attention {
	since := attentionSince(inv)
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentProcessing,
		Code:     "payment.recovery_processing",
		Message:  "Stripe is still confirming a payment on this written-off invoice, so don't charge it again.",
		Detail:   "The invoice stays written off until that payment succeeds or fails.",
		DocURL:   docURL("payment-processing"),
		Since:    &since,
	}
}

// attentionSeverityRank orders severity for max(). Kept next to the fold that
// needs it — nothing else in the file compares two severities.
func attentionSeverityRank(s AttentionSeverity) int {
	switch s {
	case AttentionSeverityCritical:
		return 3
	case AttentionSeverityWarning:
		return 2
	default:
		return 1
	}
}

// withCommitExposure reports that this invoice bought prepaid credit and the
// cash never arrived (ADR-078 D3's named fast-follow).
//
// It FOLDS rather than competes. When a cause banner already won, its reason,
// its actions and their order are preserved and the exposure is appended to the
// message; only when nothing else fired does this synthesize a banner of its
// own. Two reasons, both learned the hard way:
//
//   - A commit invoice is unpaid-with-live-credit from finalize until payment.
//     On Net-30 that is thirty days of the deal working exactly as agreed. A
//     reason that fired there would cry wolf on every commit sale Velox makes.
//   - The chain returns the FIRST match. Winning it would have replaced
//     "the customer's card was declined" — the actionable answer — with a
//     restatement of the deal terms, on precisely the invoices that most need
//     the card fixed.
//
// Severity is raised to a floor, never lowered: exposure can make an existing
// banner more urgent, but a Critical cause stays Critical.
//
// The amount goes in Message, deliberately, not Detail: the dashboard renders
// detail inside a collapsed <details> (InvoiceAttention.tsx), and money leaving
// the building behind a disclosure triangle is not surfaced. It is a second
// line after the cause's sentence, so each line stays one plain sentence.
func withCommitExposure(att *Attention, inv Invoice, atc AttentionContext) *Attention {
	drawable, spent := atc.CommitGrantDrawableCents, atc.CommitGrantConsumedCents
	if drawable <= 0 && spent <= 0 {
		// No commit grant on this invoice — the overwhelmingly common case, and
		// the reason this costs nothing on ordinary invoices.
		return att
	}
	// Paid: the cash arrived, which is the whole point — nothing to report.
	// Voided: the operator already took the one recovery action there is, and
	// the void tx retired the unspent balance. Draft: a grant is funded at
	// finalize, so a draft cannot have one; if it somehow does, that is a
	// corruption for the ledger's own guards to raise, not a banner.
	switch inv.Status {
	case InvoicePaid, InvoiceVoided, InvoiceDraft:
		return att
	}

	// Spent credit is unrecoverable — void retires only what is left (ADR-078
	// D3). Money that is already gone is the more serious state, so it, not the
	// mere existence of a grant, is what escalates.
	floor := AttentionSeverityWarning
	if spent > 0 {
		floor = AttentionSeverityCritical
	}

	amount := FormatMoneyMinor(drawable, inv.Currency)
	var sentence, detail string
	switch {
	case spent <= 0:
		sentence = "The customer can already spend " + amount +
			" from this invoice, and voiding it cancels that credit."
		detail = "This invoice bought prepaid credit, and the credit was granted when the invoice was finalized, before any payment."
	case drawable > 0:
		sentence = "The customer can still spend " + amount + " (" +
			FormatMoneyMinor(spent, inv.Currency) + " already spent), and voiding cancels the unspent " + amount + "."
		detail = "Voiding cancels only credit that hasn't been spent; credit the customer already spent stays spent."
	default:
		sentence = "The customer already spent all " +
			FormatMoneyMinor(spent, inv.Currency) + " this invoice bought, so voiding cannot recover it."
		detail = "Voiding cancels only credit that hasn't been spent, and none is left."
	}

	// Void is offered only when there is something left to cancel. On a
	// fully-spent grant the button would destroy the invoice and recover
	// nothing, which is a worse outcome than no button at all.
	//
	// ...and never while a payment is in flight, because PaymentBlocksAction
	// refuses void in that state — the button would answer 409. Reachable
	// since bad-debt recovery shipped: a written-off commit invoice being
	// recovered sits at uncollectible+processing, where this banner folds onto
	// the recovery banner (pre-ADR-113 in-flight rows only)
	// immediately above a Void button that cannot run.
	var voidAction []AttentionActionItem
	if drawable > 0 && !inv.PaymentStatus.IsInFlight() {
		voidAction = []AttentionActionItem{{Code: AttentionActionVoidInvoice, Label: "Void invoice"}}
	}

	if att == nil {
		since := attentionSince(inv)
		return &Attention{
			Severity: floor,
			Reason:   AttentionReasonCommitExposure,
			Code:     "credit.commit_exposure",
			Message:  sentence,
			Detail:   detail,
			DocURL:   docURL("commit-exposure"),
			Actions:  voidAction,
			Since:    &since,
			DueBy:    inv.DueAt,
		}
	}

	folded := *att
	if attentionSeverityRank(floor) > attentionSeverityRank(folded.Severity) {
		folded.Severity = floor
	}
	// Labelled, because under another notice's title the line needs its own
	// name ("The customer can…" alone would read as part of the cause).
	folded.Message = strings.TrimSpace(folded.Message) + "\nUnpaid prepaid credit: t" + strings.TrimPrefix(sentence, "T")
	folded.Detail = joinDetail(folded.Detail, detail)
	// Appended, never prepended: the cause banner's first action is its primary
	// CTA (fix the card, add a payment method), and that is still the right
	// first move. Void is the fallback for when collection has failed.
	folded.Actions = append(append([]AttentionActionItem{}, folded.Actions...), voidAction...)
	return &folded
}

// classifyTaxAttention branches on the typed tax_error_code persisted
// by internal/tax.Classify. Sub-cases differ in three dimensions:
// Reason (location-vs-generic), action verbs, and `param` pointer. The
// open `code` carries the full granularity (one per error_code value).
//
// Provenance routing per typed code (ADR-025):
//   - provider_not_configured: pre-flight failure, we never called the
//     provider. tax_pending_reason holds Velox's own string ("no client
//     configured for livemode=…"). Both Detail and ProviderResponse
//     stay empty — the headline + code + Connect Stripe action are the
//     whole UI; surfacing the internal string under "Provider response"
//     would mislead operators into thinking we got a 4xx from Stripe.
//   - provider_auth / provider_outage / customer_data_invalid /
//     jurisdiction_unsupported: we made the call and got a response.
//     tax_pending_reason holds the upstream payload (Stripe Tax JSON
//     envelope, prefixed with "stripe_tax: "). Goes into
//     ProviderResponse.
//   - unknown: routes to ProviderResponse — the safer slot for
//     unclassified content. If it turns out to be Velox-internal, we'd
//     just be mis-labelling it as a provider response, which is the
//     same as today's behaviour for known cases.
//
// MaxTaxRetryAttempts is the reconciler's hard cap on automatic tax
// retries (the SQL predicate lives in billing/tax_retry.go, which
// sources this const). The attention banner asserts this policy to
// operators, so the classifier and the reconciler must share one
// number — a banner promising retries the reconciler stopped making
// was the 2026-07-19 truth-audit finding this closes.
const MaxTaxRetryAttempts = 8

// TaxRetryableErrorCodes is the single source for WHICH tax error
// codes the reconciler retries. provider_not_configured is retryable
// on purpose: it fails PRE-FLIGHT (no provider API call, no quota),
// and retrying self-heals the invoice that raced a Stripe connect —
// the banner already promised this behavior; now it is true. Codes
// outside this list are operator-action-required (auth, bad customer
// data, unregistered jurisdiction): retrying those burns provider
// quota with no chance of success.
func TaxRetryableErrorCodes() []string {
	return []string{"provider_outage", "unknown", "provider_not_configured"}
}

// TaxErrorCodeRetryable reports whether the reconciler retries code.
func TaxErrorCodeRetryable(code string) bool {
	for _, c := range TaxRetryableErrorCodes() {
		if c == code {
			return true
		}
	}
	return false
}

func classifyTaxAttention(inv Invoice, atc AttentionContext, severity AttentionSeverity) *Attention {
	errorCode := inv.TaxErrorCode
	if errorCode == "" {
		errorCode = "unknown"
	}

	att := &Attention{
		Severity: severity,
		Since:    inv.TaxDeferredAt,
		Code:     "tax." + errorCode,
	}
	// Escalate whenever NO automatic mover remains. Exhaustion proves it
	// for retryable codes: after MaxTaxRetryAttempts the reconciler
	// permanently excludes the invoice (tax_retry.go predicate), so a
	// "retrying automatically" banner would soothe forever over an
	// invoice nothing will touch. A NON-retryable code is in that state
	// from birth — provider_auth (rotate the key) and
	// customer_data_invalid (fix the address) never enter the retry scan
	// at all — yet it used to sit at warning while an exhausted retryable
	// code read critical: the tier that means "self-healing in progress"
	// was shown precisely where nothing was in progress. The operator is
	// the only mover in both shapes, and the severity must say so.
	retryable := TaxErrorCodeRetryable(errorCode)
	exhausted := retryable && inv.TaxRetryCount >= MaxTaxRetryAttempts
	if exhausted || !retryable {
		att.Severity = AttentionSeverityCritical
	}
	// A real scheduled next attempt exists only while retries remain;
	// the reconciler stamps tax_next_retry_at (plumbed fact, not a
	// derived guess).
	if retryable && !exhausted && inv.TaxNextRetryAt != nil {
		att.NextAttemptAt = inv.TaxNextRetryAt
	}
	// provider_not_configured is the only tax code that's pre-flight
	// (no API call). Every other code reflects something the provider
	// actually returned. Route accordingly.
	if errorCode != "provider_not_configured" && inv.TaxPendingReason != "" {
		att.ProviderResponse = inv.TaxPendingReason
	}

	switch errorCode {
	case "customer_data_invalid":
		att.Reason = AttentionReasonTaxLocationRequired
		att.Message = "Tax can't be calculated because the customer's address or tax ID is missing or invalid, so fix it, then retry tax."
		att.Param = "customer.address.postal_code"
		att.DocURL = docURL("tax-location-required")
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionEditBillingProfile, Label: "Edit billing profile"},
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "jurisdiction_unsupported":
		att.Reason = AttentionReasonTaxCalculationFailed
		att.Message = "The tax provider can't calculate tax for this customer's region, so register for tax there with the provider, then retry tax."
		att.DocURL = docURL("tax-jurisdiction-unsupported")
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionReviewRegistration, Label: "Review tax registration"},
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "provider_outage":
		att.Reason = AttentionReasonTaxCalculationFailed
		if exhausted {
			att.Message = "The tax provider stayed unreachable through every automatic retry, so retry tax once it's back."
		} else {
			att.Message = fmt.Sprintf("The tax provider can't be reached right now, and Velox retries automatically (%d of %d attempts used).", inv.TaxRetryCount, MaxTaxRetryAttempts)
		}
		att.DocURL = docURL("tax-provider-outage")
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionWaitProvider, Label: "Check provider status"},
			{Code: AttentionActionRetryTax, Label: "Retry now"},
		}
	case "provider_auth":
		att.Reason = AttentionReasonTaxCalculationFailed
		att.Message = "The tax provider rejected the API key, so update the key in Settings, then retry tax."
		att.DocURL = docURL("tax-provider-auth")
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionRotateAPIKey, Label: "Rotate API key"},
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "provider_not_configured":
		att.Reason = AttentionReasonTaxCalculationFailed
		att.DocURL = docURL("tax-provider-not-configured")
		// Banner copy + actions split on whether the tenant has Stripe
		// connected NOW. The invoice's tax_error_code was stamped at
		// the time of the failed calculation; if the operator has
		// since connected Stripe, the only thing the invoice is
		// waiting for is the next scheduler tick — not operator
		// action. Telling them "Stripe isn't connected" in that
		// window is misleading and erodes trust in the diagnostic.
		// ADR-019 already wires a tenant-wide retry on Stripe connect,
		// so the gap is purely about UX during the 5-min/1-hr window
		// before the tick fires.
		if atc.StripeConnected && !exhausted {
			att.Severity = AttentionSeverityInfo
			att.Message = "Tax calculation is queued now that Stripe is connected; use Retry now to run it right away."
			att.Actions = []AttentionActionItem{
				{Code: AttentionActionRetryTax, Label: "Retry now"},
			}
		} else if atc.StripeConnected {
			att.Message = "Automatic tax retries have stopped for this invoice, so use Retry now to calculate it again."
			att.Actions = []AttentionActionItem{
				{Code: AttentionActionRetryTax, Label: "Retry now"},
			}
		} else {
			att.Message = "Tax can't be calculated because Stripe isn't connected in this mode, so connect Stripe, then retry tax."
			att.Actions = []AttentionActionItem{
				{Code: AttentionActionConnectTaxProvider, Label: "Connect Stripe"},
				{Code: AttentionActionRetryTax, Label: "Retry tax"},
			}
		}
	default:
		att.Reason = AttentionReasonTaxCalculationFailed
		if exhausted {
			att.Message = "Tax calculation kept failing for an unknown reason and automatic retries have stopped, so retry tax to try again."
		} else {
			att.Message = fmt.Sprintf("Tax calculation failed for an unknown reason, and Velox retries automatically (%d of %d attempts used).", inv.TaxRetryCount, MaxTaxRetryAttempts)
		}
		att.DocURL = docURL("tax-calculation-failed")
		att.Actions = []AttentionActionItem{
			{Code: AttentionActionRetryTax, Label: "Retry tax"},
		}
	}
	return att
}

// attentionSince anchors a banner's "since" instant. UpdatedAt is a
// wall-clock bookkeeping stamp (DB now()); on a SIMULATED invoice the
// attention state is born on the sim axis (finalize and charge
// outcomes happen at clock-advance), so rendering the wall stamp
// against the page's sim-time anchor produced cross-domain deltas —
// "since 70d ago" on an invoice finalized at sim-today (found live,
// FLOW I12 fixture NIM-000258). Simulated invoices anchor to IssuedAt
// (sim-domain, the birth of every finalize-derived attention state);
// wall-clock invoices keep UpdatedAt, which is fresher for
// failed-charge states.
func attentionSince(inv Invoice) time.Time {
	if inv.IsSimulated && inv.IssuedAt != nil {
		return *inv.IssuedAt
	}
	return inv.UpdatedAt
}

func classifyPaymentFailure(inv Invoice, atc AttentionContext) *Attention {
	// Operator voice, not provider voice: LastPaymentError is Stripe's
	// CARDHOLDER-facing sentence ("Your card was declined") — promoted
	// verbatim to the banner it read as Velox addressing the operator,
	// whose card was not declined. The banner states the fact in the
	// operator's frame; the provider's exact wording stays one
	// disclosure away (ProviderResponse below) and in Diagnostic
	// detail, where it is labeled as evidence.
	headline := "The customer's card was declined."
	if inv.LastPaymentError == "" {
		headline = "The customer's payment attempt failed."
	}
	since := attentionSince(inv)
	if atc.LastFailedChargeAt != nil {
		since = *atc.LastFailedChargeAt
	}
	// Update Payment Method is the primary action: the card on file
	// is broken, retrying with the same card will decline again.
	// Retry Payment is offered as secondary for transient declines
	// (network blip, temporary insufficient funds) where the
	// operator wants to re-attempt without changing the card.
	//
	// LastPaymentError is the Stripe last_payment_error message —
	// upstream payload from Stripe, not Velox's classification —
	// so it goes in ProviderResponse (ADR-025). The headline already
	// renders the truncated form; ProviderResponse holds the full
	// string for the disclosure.
	return &Attention{
		Severity: AttentionSeverityCritical,
		Reason:   AttentionReasonPaymentFailed,
		Code:     "payment.declined",
		Message:  headline,
		DocURL:   docURL("payment-failed"),
		Actions: []AttentionActionItem{
			{Code: AttentionActionUpdatePaymentMethod, Label: "Update payment method"},
			{Code: AttentionActionRetryPayment, Label: "Retry payment"},
		},
		ProviderResponse: inv.LastPaymentError,
		Since:            &since,
	}
}

func classifyPaymentUnconfirmed(inv Invoice) *Attention {
	since := attentionSince(inv)
	// No operator action: the reconciler re-queries the provider on the next
	// tick and now settles the outcome COMPLETELY — mark + dunning + email +
	// event, identical to the webhook (ADR-049 Phase 2). The prior disabled
	// "Check provider" button was non-functional (no endpoint); an on-demand
	// re-check is deferred until real stuck-payment pressure (ADR-049), so the
	// dead button is removed rather than shipped greyed-out.
	// PARKED (ADR-107) — a genuinely different situation, and the copy below
	// asserts the opposite of it. With no PaymentIntent id there is nothing for
	// the reconciler to re-query: it early-returns, the invoice is excluded from
	// every charge path, and it will NEVER resolve on its own. Rendering the
	// muted "we've got this" banner at Info severity, forever, with no actions,
	// is the system telling an operator to wait for something that cannot
	// happen (found by the honesty sweep, 2026-07-31).
	if inv.StripePaymentIntentID == "" {
		return &Attention{
			Severity: AttentionSeverityCritical,
			Reason:   AttentionReasonPaymentUnconfirmed,
			Code:     "payment.unidentifiable",
			// Bounded, not absolute (ADR-108): Velox now searches the provider
			// for the attempt, so SOME parked invoices resolve without a human
			// — but only a found PaymentIntent may resolve one, so "if it
			// cannot be found" keeps the old sentence's truth. The severity
			// stays Critical: until the search finds something, this invoice
			// still needs a human's eventual decision, and demoting it on the
			// hope of a future find would hide exactly the rows that never get
			// found.
			Message: "Velox can't tell if this charge went through, so it won't charge again; check Stripe, and if nothing was taken, mark it uncollectible.",
			Detail: "Velox keeps searching Stripe for the charge and records it here if found. If it cannot be found, this will not resolve on its own. " +
				"No new charge is made, so the customer is never charged twice. Search Stripe by customer and amount.",
			DocURL: docURL("payment-unconfirmed"),
			Since:  &since,
			// The way out sits in the notice, not only in the ⋯ menu: an
			// operator told "mark it uncollectible" should not have to go
			// looking for the control.
			Actions: []AttentionActionItem{
				{Code: AttentionActionMarkUncollectible, Label: "Mark uncollectible"},
			},
		}
	}

	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentUnconfirmed,
		Code:     "payment.unconfirmed",
		// Honest scope (2026-07-06 truth pass; cause corrected 2026-07-31):
		// the reconciler settles TERMINAL Stripe outcomes and skips the rest,
		// so a PaymentIntent that stays non-terminal keeps this banner and the
		// operator's lever is at the provider. The advice below is therefore
		// kept, deliberately — it is the honest thing to say about a state we
		// do not resolve for them.
		//
		// The CAUSE this note used to give was wrong, and the phrasing spread
		// into ADR-107 before anyone checked it: it blamed "off-session SCA
		// nobody completes". Off-session SCA is a DECLINE — Stripe's
		// authentication_required is a decline code, which arrives as a card
		// error and settles the invoice FAILED with dunning started, never
		// reaching this banner. A live requires_action PaymentIntent comes from
		// the hosted checkout, where the customer is present and mid-3DS, and
		// that one expires with its session inside the hour.
		Message: "Stripe hasn't confirmed this charge yet, and Velox re-checks automatically until it does.",
		Detail: "If it stays here for hours, the charge may be waiting on the customer (for example 3-D Secure). " +
			"Check the payment in Stripe, then cancel or retry it there.",
		DocURL: docURL("payment-unconfirmed"),
		Since:  &since,
	}
}

// classifyPaymentProcessing surfaces the in-flight charge state. Velox now
// confirms it automatically — inline from the synchronous charge response
// (ADR-049 Phase 3) or via the reconciler backstop if the provider callback is
// delayed/dropped (Phase 2) — so the healthy state needs no operator action.
//
// Age-aware: a REAL (non-simulated) invoice still `processing` past
// processingStaleAfter is anomalous for a card (it should have settled inline
// or via the reconciler), so escalate Info → Warning and point the operator at
// the provider — without falsely promising auto-resolution for the stuck case.
// Simulated invoices never escalate: their "age" is sim-time, not a real-world
// duration, so a wall-clock age check would false-positive (atc.Now is
// wall-clock). Zero atc.Now also keeps it Info (callers that don't set it).
func classifyPaymentProcessing(inv Invoice, atc AttentionContext) *Attention {
	since := attentionSince(inv)

	stale := !inv.IsSimulated && !atc.Now.IsZero() && atc.Now.Sub(inv.UpdatedAt) > processingStaleAfter
	if stale {
		return &Attention{
			Severity: AttentionSeverityWarning,
			Reason:   AttentionReasonPaymentProcessing,
			Code:     "payment.processing",
			Message:  fmt.Sprintf("Stripe still hasn't confirmed this payment after %d hours, so check it in Stripe.", int(processingStaleAfter.Hours())),
			Detail:   "Card payments are usually confirmed within seconds, so a payment this old needs a look.",
			DocURL:   docURL("payment-processing"),
			Since:    &since,
		}
	}

	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentProcessing,
		Code:     "payment.processing",
		Message:  "Stripe is confirming this payment, and this page updates automatically when it's done.",
		DocURL:   docURL("payment-processing"),
		Since:    &since,
	}
}

// classifyPaymentPageOpen: the customer is paying on the hosted payment page,
// or has just paid there and Velox is recording it. Nothing is wrong and the
// operator has nothing to do: Velox holds its own charges and dunning retries
// until the page closes (ADR-068), so no actions are offered — Collect and
// Retry would be refused. Reuses payment_processing (the closed reason enum is
// a public contract): a payment may be in progress, and the Code carries the
// granularity.
func classifyPaymentPageOpen() *Attention {
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentProcessing,
		Code:     "payment.page_open",
		Message:  "The customer has the payment page open or just paid on it, so Velox holds charges until that finishes, usually within an hour.",
		DocURL:   docURL("payment-processing"),
	}
}

// classifyPaymentScheduled surfaces the scheduler-will-retry state.
// Auto-charge is queued; the engine will fire on its next sweep. The
// operator can short-circuit with "Charge now" if they don't want to
// wait the scheduler interval.
func classifyPaymentScheduled(inv Invoice) *Attention {
	since := attentionSince(inv)
	// payment_scheduled fires when auto_charge_pending=true and the
	// invoice is still unpaid: the finalize-time collection didn't finish
	// (a transient failure, or the process stopped), and the auto-charge
	// sweep will pick it up on its next run. We don't surface a precise
	// next_attempt_at; "usually within an hour" is honest because the
	// sweep runs on the billing tick, hourly by default (every 5 minutes in
	// local dev, cmd/velox/main.go).
	//
	// A clock-pinned (simulated) invoice is the exception: the wall-clock
	// sweep EXCLUDES it (ADR-028/029 disjoint flows — ListAutoChargePending
	// filters out subs with a test_clock_id), so the retry fires only when
	// the operator advances the test clock. "Within an hour" there is a
	// lie — in real time the invoice sits indefinitely, so adding a payment
	// method and waiting looks broken. Surface the clock-advance reality.
	message := "The card on file is charged automatically, usually within an hour; use Charge now to collect it right away."
	detail := "Automatic charges run on a schedule, hourly by default."
	if inv.IsSimulated {
		message = "The card on file is charged on the next clock advance; use Charge now to collect it right away."
		detail = "Simulated invoices are charged only when the test clock is advanced, never on the real-time schedule."
	}
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonPaymentScheduled,
		Code:     "payment.scheduled",
		Message:  message,
		Detail:   detail,
		DocURL:   docURL("payment-scheduled"),
		Actions: []AttentionActionItem{
			{Code: AttentionActionChargeNow, Label: "Charge now"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

// classifyCollectionPaused surfaces a queued invoice whose subscription is
// paused for collection. The queue flag is still set — it is what will make
// the sweep pick the invoice up the moment collection resumes — but nothing
// charges it in the meantime, so this must not borrow payment_scheduled's
// "the engine will charge this" copy.
//
// Severity info, not warning: the operator asked for this. Charge now stays
// offered because a manual collect is an explicit override and has no pause
// filter — the pause governs automation, not the operator.
func classifyCollectionPaused(inv Invoice) *Attention {
	since := attentionSince(inv)
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonCollectionPaused,
		Code:     "payment.collection_paused",
		Message:  "Collection is paused on the subscription, so this invoice won't be charged automatically; use Charge now to collect it anyway.",
		Detail:   "Resume collection on the subscription page to go back to automatic charges. This invoice is then charged on the next run.",
		DocURL:   docURL("collection-paused"),
		Actions: []AttentionActionItem{
			{Code: AttentionActionChargeNow, Label: "Charge now"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

// classifyAwaitingPayment surfaces a finalized, unpaid invoice with a
// card on file that nothing has queued for collection. Operators get
// two paths: trigger the charge themselves, or send the customer a
// reminder email.
//
// This is NOT a steady state Velox has a collection mode for — every
// finalize write queues the invoice (auto_charge_pending, ADR-116),
// which routes to payment_scheduled instead. Reaching here means a row
// written before that change. It is operator-recoverable via Charge now,
// which is why the actions matter more than the copy here.
func classifyAwaitingPayment(inv Invoice) *Attention {
	since := attentionSince(inv)
	return &Attention{
		Severity: AttentionSeverityInfo,
		Reason:   AttentionReasonAwaitingPayment,
		Code:     "payment.awaiting",
		Message:  "This invoice hasn't been charged yet; use Charge now, or email the customer a payment link.",
		DocURL:   docURL("awaiting-payment"),
		Actions: []AttentionActionItem{
			{Code: AttentionActionChargeNow, Label: "Charge now"},
			{Code: AttentionActionSendReminder, Label: "Email payment link"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

// classifyNoPaymentMethod surfaces the finalized-unpaid invoice whose
// customer has no PaymentSetup ready. The invoice IS queued
// (auto_charge_pending) and dunning ages it out — but automation cannot
// conjure the card, so the operator/customer is the mover.
//
// Message line 1 says what happens to THIS invoice when a card arrives,
// which depends on two real signals: collection paused (the sweep that
// charges on attach skips paused subscriptions, so nothing happens until
// resume) and simulated time (the wall-clock sweep excludes clock-pinned
// invoices, so the charge waits for the next clock advance). Line 2 says
// what happened to the setup link, which depends on two more: whether the
// customer has an email and whether this server can send email at all.
//
// Once dunning has escalated, the notice becomes "Payment recovery ended"
// and line 1 branches on pause the same way; line 2 appears only when no
// setup link can be emailed, to say why Resend is missing.
//
// Severity = warning (operator action required, not financially
// broken). Promoting to critical would make a perfectly normal "send-
// invoice" collection mode look alarming.
func classifyNoPaymentMethod(inv Invoice, atc AttentionContext) *Attention {
	since := attentionSince(inv)
	// Resend can only send to an address, from a server that can send.
	canEmail := atc.CustomerHasEmail && !atc.SetupLinkEmailOff

	// What adding a card does. The charge-on-attach promise is the server's
	// own (post-ADR-013 the scheduler picks the invoice up the moment a
	// PaymentSetup flips to ready, Chargebee's "Collect Invoice on Card
	// Update"), and it lives HERE, server-owned, so the client can't drift a
	// duplicate copy of it (it used to hardcode one).
	charge := "This invoice is charged automatically once the customer adds a card."
	if inv.IsSimulated {
		charge = "This invoice is charged on the next clock advance once the customer adds a card."
	}
	if atc.CollectionPaused {
		charge = "Collection is paused on the subscription, so adding a card won't charge this invoice until you resume it."
	}

	// What happened to the setup link. Card details are always a customer
	// action (PCI: entered by the cardholder on Stripe's hosted page); the
	// operator's levers are resending the emailed link and the customer
	// page, whose "Add payment method" dialog copies the link to share
	// directly or lets them add an email address.
	//
	// "is emailed … once" states the engine's send-once behavior
	// (no_pm_notified_at) rather than asserting this invoice's send, which
	// the outbox may still fail; the Details point at where delivery shows.
	var emailLine, emailDetail string
	switch {
	case !atc.CustomerHasEmail:
		// False also when the customer read failed, so the sentence states
		// Velox's rule rather than this customer's state: honest either way,
		// and the customer page shows the real address.
		emailLine = "Setup links are emailed only to customers with an email address, so copy one from the customer page and send it."
		emailDetail = "Adding an email address on the customer page lets Velox send the link."
	case atc.SetupLinkEmailOff:
		emailLine = "This server can't email setup links, so none was sent; copy one from the customer page instead."
		emailDetail = "To let Velox email setup links, set up email sending (SMTP) and the payment-update page address (PAYMENT_UPDATE_URL)."
	default:
		emailLine = "A setup link is emailed to the customer once; use Resend setup link to send it again."
		emailDetail = "The invoice's activity shows whether the email was sent. The customer page can also copy the link to share directly."
	}

	actions := []AttentionActionItem{
		{Code: AttentionActionAddPaymentMethod, Label: "Open customer page"},
	}
	if canEmail {
		actions = append([]AttentionActionItem{
			{Code: AttentionActionSendReminder, Label: "Resend setup link"},
		}, actions...)
	}

	// Dunning has escalated: automatic recovery ended without collecting and
	// the policy's final step ran (default: pause the subscription's
	// collection). The pre-escalation framing is no longer the whole story
	// (2026-07-22 audit, P1-4), and the fix depends on whether that final
	// step paused collection: if it did, a card alone collects nothing.
	if atc.DunningEscalated {
		var msg string
		switch {
		case atc.CollectionPaused:
			msg = "Retries stopped and collection is paused; ask the customer to add a card, then resume collection."
		case inv.IsSimulated:
			msg = "Retries stopped; this invoice is charged on the next clock advance once the customer adds a card."
		default:
			msg = "Retries stopped; this invoice is charged automatically once the customer adds a card."
		}
		// When no setup link can be emailed, the Resend button is missing and
		// the reason must show up front, not only in Details. When one can,
		// the Resend button is there and the email line stays in Details.
		const ended = "Automatic payment recovery ended without collecting, and the recovery policy's final step ran. " +
			"Check the subscription to see what that step did."
		detail := joinDetail(ended, emailLine+" "+emailDetail)
		if !canEmail {
			msg += "\n" + emailLine
			detail = joinDetail(ended, emailDetail)
		}
		return &Attention{
			Severity: AttentionSeverityWarning,
			Reason:   AttentionReasonDunningExhausted,
			Code:     "payment.dunning_exhausted",
			Message:  msg,
			Detail:   detail,
			DocURL:   docURL("dunning-exhausted"),
			Actions:  actions,
			Since:    &since,
			DueBy:    inv.DueAt,
		}
	}

	return &Attention{
		Severity: AttentionSeverityWarning,
		Reason:   AttentionReasonNoPaymentMethod,
		Code:     "payment.no_payment_method",
		Message:  charge + "\n" + emailLine,
		Detail:   emailDetail,
		DocURL:   docURL("no-payment-method"),
		Actions:  actions,
		Since:    &since,
		DueBy:    inv.DueAt,
	}
}

// classifyPaymentAnomaly renders the ADR-068 durable marker: Critical, and
// the action is manual reconciliation at Stripe (refund the duplicate /
// adjust the books). The sentence says what was detected in plain words;
// the PaymentIntent ids go in Detail, where support and the Stripe search
// need them, and the action opens the payment in Stripe.
func classifyPaymentAnomaly(inv Invoice) *Attention {
	pi := inv.PaymentAnomalyPaymentIntentID
	var msg, detail string
	switch inv.PaymentAnomalyKind {
	case EventPaymentDuplicateCharge:
		msg = "A second payment succeeded after this invoice was already paid, so refund the extra charge in Stripe."
		detail = "Extra payment: " + pi + "."
		if inv.StripePaymentIntentID != "" {
			detail += " Original payment: " + inv.StripePaymentIntentID + "."
		}
		detail += " The extra charge exists only in Stripe; Velox records one payment."
	case EventPaymentAmountMismatch:
		// Formatted, not raw minor units: this sentence used to read "Stripe
		// captured 9652 (minor units) … booked 10000", handing an operator two
		// unlabelled integers and a unit they do not think in, in the one banner
		// whose entire job is comparing two amounts.
		msg = fmt.Sprintf("Stripe captured %s but this invoice recorded %s as paid, so reconcile the difference.",
			FormatMoneyMinor(inv.PaymentAnomalyCapturedCents, inv.Currency), FormatMoneyMinor(inv.AmountPaidCents, inv.Currency))
		detail = "Payment: " + pi + ". The recorded paid amount sets how much can be refunded."
	case EventPaymentReceivedOnVoidedInvoice:
		msg = "A payment succeeded on this voided invoice, so the customer paid money they don't owe; refund it in Stripe."
		detail = "Payment: " + pi + "."
	case EventPaymentCurrencyMismatch:
		msg = "A payment was charged in a different currency than this invoice, so review it in Stripe and refund or correct it."
		detail = "Payment: " + pi + ". The amounts were not converted."
	default:
		msg = "Velox found a payment on this invoice that doesn't match its records, so review it in Stripe."
		detail = "Kind: " + inv.PaymentAnomalyKind + ". Payment: " + pi + "."
	}
	att := &Attention{
		Reason:   AttentionReasonPaymentAnomaly,
		Severity: AttentionSeverityCritical,
		Code:     inv.PaymentAnomalyKind,
		Message:  msg,
		Detail:   detail,
	}
	// The refund or correction is made in Stripe, so the action opens the
	// payment there. Without an id there is nothing to open.
	if pi != "" {
		att.Actions = []AttentionActionItem{{Code: AttentionActionReconcilePayment, Label: "Open in Stripe"}}
	}
	return att
}

// joinDetail appends one Detail paragraph to another, blank-line separated.
func joinDetail(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}
