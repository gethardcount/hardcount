package domain

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

type InvoiceStatus string

const (
	InvoiceDraft     InvoiceStatus = "draft"
	InvoiceFinalized InvoiceStatus = "finalized"
	InvoicePaid      InvoiceStatus = "paid"
	InvoiceVoided    InvoiceStatus = "voided"
	// InvoiceUncollectible marks an invoice as no-further-collection-
	// attempted while preserving it in financial reporting (Stripe-
	// standard semantics; distinct from Voided which annuls the
	// invoice). Set by dunning's mark_uncollectible terminal invoice action and
	// by operator action. ADR-036 amendment.
	InvoiceUncollectible InvoiceStatus = "uncollectible"
)

// InvoiceBillingReason classifies the trigger that produced an invoice.
// Mirrors Stripe's invoice.billing_reason. Persisted as nullable on
// the invoices table — legacy rows pre-FEAT-Week5c are NULL.
//
// SubscriptionCycle is the natural-cycle end invoice the cycle scan
// emits at period boundaries. SubscriptionCreate is the prorated
// initial invoice when a subscription starts mid-cycle. Manual is an
// operator-initiated standalone invoice. Threshold is the early-invoice
// early finalize fired by the threshold scan tick.
type InvoiceBillingReason string

const (
	BillingReasonSubscriptionCycle  InvoiceBillingReason = "subscription_cycle"
	BillingReasonSubscriptionCreate InvoiceBillingReason = "subscription_create"
	// BillingReasonSubscriptionCancel is the final partial-period invoice
	// emitted at mid-period immediate cancel — covers in_arrears prorated
	// base + usage from current_period_start to canceled_at. Distinct
	// from subscription_cycle so reporting / dashboard can label this as
	// a cancel-time true-up, not a normal cycle close.
	BillingReasonSubscriptionCancel InvoiceBillingReason = "subscription_cancel"
	// BillingReasonSubscriptionUpdate is the immediate proration invoice cut
	// by a mid-period subscription change — plan upgrade, quantity increase,
	// or item add (Stripe stamps the same `subscription_update` reason for
	// all of these). Previously these invoices persisted a NULL reason, so
	// the dashboard couldn't label what triggered them.
	BillingReasonSubscriptionUpdate InvoiceBillingReason = "subscription_update"
	BillingReasonManual             InvoiceBillingReason = "manual"
	BillingReasonThreshold          InvoiceBillingReason = "threshold"
)

type InvoicePaymentStatus string

const (
	PaymentPending    InvoicePaymentStatus = "pending"
	PaymentProcessing InvoicePaymentStatus = "processing"
	PaymentSucceeded  InvoicePaymentStatus = "succeeded"
	PaymentFailed     InvoicePaymentStatus = "failed"
	// PaymentUnknown marks a Stripe charge attempt that returned an ambiguous
	// error (5xx, timeout, connection reset) where we cannot tell from the
	// response whether Stripe actually processed the charge. A reconciler
	// resolves these by querying Stripe after a cool-off window.
	PaymentUnknown InvoicePaymentStatus = "unknown"
)

// IsInFlight reports whether a charge attempt is still settling — the
// outcome is not yet terminal, so the captured amount is unknown. This is
// the single canonical definition of "in flight": processing (the charge
// is open at the provider, e.g. off-session SCA requires_action maps here)
// or unknown (an ambiguous provider error that may or may not have charged;
// a reconciler resolves it). Any path that MUTATES an invoice (void,
// mark-uncollectible, record-offline-payment, credit-note / clawback
// amount_due reduction) must refuse or defer while this is true — mutating
// an invoice with an open payment strands captured money, reverses tax on a
// sale that may complete, or under-records amount_paid against a lowered
// amount_due. Stripe enforces the same rule ("you can't void, edit, or mark
// uncollectible an invoice with open payments").
//
// Code outside this file asks Invoice.PaymentInFlight instead, which answers
// for the whole invoice row; internal/arch pins that.
func (s InvoicePaymentStatus) IsInFlight() bool {
	return s == PaymentProcessing || s == PaymentUnknown
}

// PaymentInFlight reports whether a payment may be open on this invoice: one
// that may still take money, so its outcome is not known yet. Every action
// that must refuse or wait while that is true asks here: void, write-off,
// offline payment, credit notes, Collect, invoice emails, the hosted Pay
// button, proration's credit path and dunning's final action.
//
// Today the answer is the payment status alone (IsInFlight). It lives on the
// invoice rather than on the status so that a fact stored elsewhere on the
// invoice row can join it here, once, instead of at each caller (#976 adds a
// record of a charge sent to Stripe whose result is not saved yet).
func (i Invoice) PaymentInFlight() bool {
	return i.PaymentStatus.IsInFlight()
}

// Parked reports whether the invoice's last charge got an unclear answer from
// Stripe that named no PaymentIntent (ADR-107): payment_status 'unknown' with
// no PaymentIntent id. No charge path takes it, and no sweep can look the
// payment up by id. The stale-hold sweep finishes it from Stripe's list of the
// customer's payments: it records or follows a payment that appears, and with
// none ParkedNothingWait after the attempt it puts the invoice back where a
// charge picks it up (ADR-127 amendment 2026-10-20).
func (i Invoice) Parked() bool {
	return i.PaymentStatus == PaymentUnknown && i.StripePaymentIntentID == ""
}

// HasCardPayment reports whether this invoice's settled money is refundable to
// a card: a Stripe PaymentIntent backs it. False for a never-charged invoice,
// one paid by credits or with nothing due, and one paid out of band
// (PaidOutOfBand).
//
// It is the one answer to "can money go back the way it came?", and a bare
// emptiness check gives the same answer: the PaymentIntent column holds only
// Stripe's ids, and an offline payment leaves it empty (migration 00203
// refuses anything else). Before, an offline payment wrote a synthetic
// "out_of_band:<time>" id there, every reader had to know the prefix, and
// one that did not offered a card refund on a bank transfer (HC-000068,
// 2026-08-02).
func (i Invoice) HasCardPayment() bool {
	return i.StripePaymentIntentID != ""
}

// CardRefundableCents is the ceiling on what a credit note may push back to the
// card: what the card actually settled, less refunds already committed by
// prior (non-voided) credit notes. Zero when no card backs the payment, which
// collapses the offline case into the SAME cap the over-refund path already
// enforces rather than adding a second, parallel guard.
func (i Invoice) CardRefundableCents(priorRefundCents int64) int64 {
	if !i.HasCardPayment() {
		return 0
	}
	return max(0, i.AmountPaidCents-priorRefundCents)
}

// UnpaidRelievableCents is the most a credit note can relieve on an invoice
// that has not been paid: what is still due, plus the customer credits applied
// to it. A note first reduces the amount due; the rest of it displaces applied
// credits, which go back to the customer's balance when the note is issued
// (creditnote.Issue, ADR-061 amendment 2026-10-12). Sizing a note against the
// amount due alone dropped those credits.
func (i Invoice) UnpaidRelievableCents() int64 {
	return max(0, i.AmountDueCents) + max(0, i.CreditsAppliedCents)
}

// IssuedAtOrZero is the issue instant, or the zero Time on an invoice that was
// never issued. Dunning start reads it to anchor a clock-pinned invoice's first
// retry (see dunning.Service.StartDunning).
func (i Invoice) IssuedAtOrZero() time.Time {
	if i.IssuedAt == nil {
		return time.Time{}
	}
	return *i.IssuedAt
}

// Provider PaymentIntent statuses, verbatim from Stripe. These name the values
// that can land in Invoice.ProviderPaymentStatus (migration 0167) so stored
// data and tests read against symbols rather than loose string literals. No
// logic branches on them today — the column is recorded for queue ordering and
// for diagnosing a long-running payment.
const (
	ProviderPIRequiresPaymentMethod = "requires_payment_method"
	ProviderPIRequiresConfirmation  = "requires_confirmation"
	ProviderPIRequiresAction        = "requires_action"
	ProviderPIProcessing            = "processing"
	ProviderPIRequiresCapture       = "requires_capture"
	ProviderPISucceeded             = "succeeded"
	ProviderPICanceled              = "canceled"
)

// InvoiceTaxStatus tracks whether tax has been successfully calculated for
// an invoice. The happy path is ok: calculation succeeded (including
// zero-tax outcomes from none/manual/exempt/reverse-charge). Pending means
// calculation failed and the invoice waits; tax_error_code says whether the
// retry worker tries again or the operator must act.
//
// A pending invoice is blocked from finalize: sending a wrong tax amount
// creates audit/compliance exposure, so we defer rather than silently fall
// back to an incorrect rate.
type InvoiceTaxStatus string

const (
	InvoiceTaxOK      InvoiceTaxStatus = "ok"
	InvoiceTaxPending InvoiceTaxStatus = "pending"
)

// InvoiceFinalizationStatus returns the invoice Status that must be
// stamped at creation time given the tax + pause-collection state.
// Single source of truth across all four invoice-emitting paths
// (engine.billOnePeriod, engine.BillOnCreate,
// engine.BillFinalOnImmediateCancel, subscription.handleItemProration)
// so the rule "tax_status=pending OR pause_collection set → draft"
// can't drift between flows.
//
//   - Tax pending: a finalized invoice implies authoritative amounts.
//     With tax deferred, the TotalAmountCents reflects subtotal +
//     zero tax — incorrect at finalize time. The retry worker
//     transitions draft → finalized when calculation succeeds.
//   - Pause-collection set: Stripe-parity behavior — cycle continues
//     and line items are captured, but the operator/customer-facing
//     finalize/charge/dunn flow is skipped until pause is cleared.
//
// pauseCollection is the *PauseCollection pointer from the
// subscription (nil = collection running normally). at is the instant
// the invoice bills to: a period's end, a cancel's canceled_at, or now.
// The pause is read as it stood then, because a close can run after
// resumes_at has passed for a period that ended while still paused.
func InvoiceFinalizationStatus(taxStatus InvoiceTaxStatus, pauseCollection *PauseCollection, at time.Time) InvoiceStatus {
	if taxStatus == InvoiceTaxPending {
		return InvoiceDraft
	}
	if pauseCollection.InForceAt(at) {
		return InvoiceDraft
	}
	return InvoiceFinalized
}

type Invoice struct {
	ID             string               `json:"id"`
	TenantID       string               `json:"tenant_id,omitempty"`
	CustomerID     string               `json:"customer_id"`
	SubscriptionID string               `json:"subscription_id"`
	InvoiceNumber  string               `json:"invoice_number"`
	Status         InvoiceStatus        `json:"status"`
	PaymentStatus  InvoicePaymentStatus `json:"payment_status"`
	Currency       string               `json:"currency"`
	SubtotalCents  int64                `json:"subtotal_cents"`
	DiscountCents  int64                `json:"discount_cents"`
	// TaxFacts is the 13-field tax-calculation bundle, embedded untagged so
	// its JSON keys promote FLAT — the wire shape is byte-identical to the
	// former flat fields (pinned by TestInvoiceTaxFactsWireCompat). See
	// tax_facts.go for why this is one shared type instead of hand-copied
	// mirrors.
	TaxFacts
	// TaxTransactionID is the committed upstream transaction reference
	// (Stripe Tax: tx_xxx), captured at finalize time. Durable upstream
	// record of the tax decision and the handle the provider needs for
	// a later reversal when a credit note is issued against this invoice.
	// Empty for providers without durable state (none, manual) and for
	// invoices whose finalize has not committed tax yet. Invoice-only
	// lifecycle — deliberately NOT part of TaxFacts (written at Commit,
	// not produced by the calculation), like the retry bookkeeping below.
	TaxTransactionID string `json:"tax_transaction_id,omitempty"`
	TaxRetryCount    int    `json:"tax_retry_count,omitempty"`

	// PaymentAnomaly* is the durable settle-path anomaly marker (ADR-068):
	// kind is the per-cause event name (payment.duplicate_charge /
	// payment.amount_mismatch / payment.received_on_voided_invoice), the PI
	// is the offending PaymentIntent, captured the amount Stripe took.
	// Single-slot, last-wins; drives the Critical attention banner even on
	// terminal invoices.
	PaymentAnomalyKind            string `json:"payment_anomaly_kind,omitempty"`
	PaymentAnomalyPaymentIntentID string `json:"payment_anomaly_payment_intent_id,omitempty"`
	PaymentAnomalyCapturedCents   int64  `json:"payment_anomaly_captured_cents,omitempty"`
	// TaxNextRetryAt is the soonest the background reconciler may
	// re-run tax calculation. NULL means "ready now" (either never
	// tried or operator-driven retry just reset the schedule).
	// Future timestamp means a previous attempt failed with a
	// retryable code and the next attempt should wait until this
	// time per the exponential backoff curve in
	// internal/billing/tax_retry.go. Migration 0074. ADR-017.
	TaxNextRetryAt *time.Time `json:"tax_next_retry_at,omitempty"`
	// TaxReversedAt records when the upstream tax transaction was
	// successfully reversed on void / mark-uncollectible (or when there
	// was nothing left to reverse). NULL on a voided/uncollectible
	// stripe_tax invoice that still carries a TaxTransactionID means the
	// reversal has not been confirmed yet — RetryPendingTaxReversal keys
	// on this to re-drive a failed reversal (the symmetric sibling of the
	// #267 commit reconciler). Migration 0122.
	TaxReversedAt *time.Time `json:"tax_reversed_at,omitempty"`
	// PaymentCardBrand / PaymentCardLast4 capture the card used to
	// settle this invoice — populated at payment_intent.succeeded
	// time by looking up the PI's payment_method against the
	// payment_methods table. Empty when the PM is unknown to us
	// (one-off Checkout cards). Migration 0077. Surfaces in the
	// activity timeline as a sub-line on the "Invoice paid" row.
	// ADR-020.
	PaymentCardBrand string `json:"payment_card_brand,omitempty"`
	PaymentCardLast4 string `json:"payment_card_last4,omitempty"`
	// Attention is the unified "needs operator attention" surface,
	// computed on read by ClassifyInvoiceAttention. Never persisted —
	// always derived from the durable fields above (tax_status,
	// tax_error_code, payment_status, last_payment_error) plus
	// due_at. Nil when the invoice is healthy
	// (terminal-state or no failure mode active). See
	// docs/adr/009-invoice-attention.md for the wire-shape contract.
	Attention *Attention `json:"attention,omitempty"`
	// RecoveryWarning is what an operator must know before recording an
	// OFFLINE payment on a written-off invoice — a consequence that will go
	// unreconciled, not a refusal. Nil when there is nothing to say.
	RecoveryWarning     *PaymentBlock `json:"recovery_warning,omitempty"`
	TotalAmountCents    int64         `json:"total_amount_cents"`
	AmountDueCents      int64         `json:"amount_due_cents"`
	AmountPaidCents     int64         `json:"amount_paid_cents"`
	CreditsAppliedCents int64         `json:"credits_applied_cents"`
	BillingPeriodStart  time.Time     `json:"billing_period_start"`
	BillingPeriodEnd    time.Time     `json:"billing_period_end"`
	// BillingTimezone is the IANA timezone the period boundaries above are
	// civil-midnight in — copied from the subscription's snapshot (ADR-077) at
	// invoice creation and immutable thereafter. BillingPeriodDisplay is computed
	// in THIS zone, not the live tenant TZ, so the inclusive last-day step lands
	// on the right civil date even after a tenant changes its timezone. Manual
	// invoices stamp the org zone at create too (2026-10-16). Empty only on
	// legacy rows (engine rows before migration 0134, manual rows before that
	// fix); the read path then falls back to the live tenant TZ.
	BillingTimezone string `json:"billing_timezone,omitempty"`
	// BillToSnapshot and SupplierSnapshot are the buyer and seller details
	// this invoice was issued with (ADR-109), written in the transaction that
	// took it out of draft. Nil on drafts, which render live, and on invoices
	// issued before migration 0177, which keep rendering live because nothing
	// records what they showed.
	BillToSnapshot   *DocumentBillTo   `json:"bill_to_snapshot,omitempty"`
	SupplierSnapshot *DocumentSupplier `json:"supplier_snapshot,omitempty"`
	// BillingPeriodDisplay is the human period string with the INCLUSIVE last
	// covered day ("Jun 1, 2028 – Jun 30, 2028"), rendered date-only in the
	// tenant TZ — the industry-standard period display (ADR-058 follow-up).
	// Computed on read (never persisted); the raw half-open
	// BillingPeriodStart/End above are unchanged (SDK contract). Empty for
	// one-off / no-period invoices so callers omit the period row. Every
	// surface that shows the period (PDF, dashboard, list) shows this one value
	// verbatim so it can't drift across the Go and TS runtimes. The hosted
	// invoice page shows no period; its linked PDF does.
	BillingPeriodDisplay string     `json:"billing_period_display,omitempty"`
	IssuedAt             *time.Time `json:"issued_at,omitempty"`
	DueAt                *time.Time `json:"due_at,omitempty"`
	PaidAt               *time.Time `json:"paid_at,omitempty"`
	VoidedAt             *time.Time `json:"voided_at,omitempty"`
	UncollectibleAt      *time.Time `json:"uncollectible_at,omitempty"`
	// StripePaymentIntentID is the Stripe PaymentIntent behind the invoice's
	// latest charge, and only that: empty when no charge was sent, and for an
	// invoice paid out of band.
	StripePaymentIntentID string `json:"stripe_payment_intent_id,omitempty"`
	// PaidOutOfBand is set when the operator recorded the payment as made
	// outside Hardcount (RecordOfflinePayment: a bank transfer, a cheque, cash).
	// Stripe's paid_out_of_band. Such an invoice has no PaymentIntent (a
	// CHECK holds both), so there is no card to refund.
	PaidOutOfBand     bool   `json:"paid_out_of_band"`
	LastPaymentError  string `json:"last_payment_error,omitempty"`
	AutoChargePending bool   `json:"auto_charge_pending,omitempty"`
	// NoPMNotifiedAt is the send-once marker for the "payment method needed"
	// setup-link email: stamped by whichever sender delivered it (finalize-time
	// collect or the auto-charge retry sweep), checked by the sweep so a no-PM
	// invoice is emailed exactly once, not per tick (ADR-087 follow-up).
	NoPMNotifiedAt *time.Time `json:"no_pm_notified_at,omitempty"`
	// BelowMinimumAt is when Stripe last refused to charge this invoice
	// because its amount is below Stripe's minimum charge (ErrBelowMinimumCharge).
	// The charge path stamps it with payment_status 'pending' and the
	// auto-charge queue cleared, in one write. It is Stripe's latest answer,
	// not a permanent fact (the minimum of a converted charge follows the
	// account's settlement currency): the next charge outcome clears it.
	// Readers check Invoice.BelowStripeMinimum. Internal: the notice is the
	// API's view.
	BelowMinimumAt *time.Time `json:"-"`
	// ChargeAttemptSeq seeds the Stripe charge idempotency key
	// (payment.ChargeIdempotencyKey). It moves ONLY when a charge attempt
	// outcome is recorded — every UPDATE that stamps stripe_payment_intent_id
	// bumps it, nothing else touches it. That is what makes a crashed
	// attempt's retry converge on the SAME PaymentIntent (a crash records no
	// outcome, so the seq and therefore the key are unchanged) while a retry
	// after a RECORDED decline gets a fresh one. Internal: never serialised —
	// it is a payment-protocol detail, not invoice data an operator reads.
	ChargeAttemptSeq int64 `json:"-"`

	// ProviderPaymentStatus is Stripe's verbatim PaymentIntent status at the
	// last reconciler poll, and ProviderSyncedAt is when we looked (migration
	// 0167). They are OBSERVATIONS. PaymentStatus stays the authoritative money
	// state — these never override it, and nothing derives money from them.
	//
	// They exist because the reconciler already fetched this on every sweep and
	// discarded it whenever the PaymentIntent was non-terminal, which is exactly
	// the case that needed it: a payment waiting on the customer looked
	// identical to one settling normally. Recording it is what lets the sweep
	// rotate instead of jamming on a frozen row, lets the banner say "waiting on
	// the customer since <date>" instead of promising resolution, and lets the
	// gate offer a cancel when the PROVIDER — not a guess here — says no money
	// moved.
	//
	// Empty/zero means never polled: every row predating 0167, and every
	// invoice whose payment has not yet been in flight long enough for a sweep.
	// Treat unknown-because-unpolled as "no information", never as "not stuck".
	ProviderPaymentStatus string     `json:"provider_payment_status,omitempty"`
	ProviderSyncedAt      *time.Time `json:"provider_synced_at,omitempty"`

	PDFObjectKey string `json:"-"`
	// PublicToken is the hosted-invoice-URL credential (Stripe-parity
	// hosted_invoice_url). Generated at finalize; drafts have an empty
	// token. Rotatable via the rotate-public-token endpoint if the URL
	// ever leaks. Omitted from JSON when empty so list responses stay
	// clean on pre-addendum invoices that haven't been backfilled.
	PublicToken        string `json:"public_token,omitempty"`
	NetPaymentTermDays int    `json:"net_payment_term_days"`
	Memo               string `json:"memo,omitempty"`
	// Footer is the tenant's invoice footer as it was when this invoice was
	// issued, copied by the issue stamp (ADR-109). Empty on a draft, and on
	// an invoice issued with no footer set or before footers were copied.
	Footer    string         `json:"footer,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`

	// SourcePlanChangedAt + SourceSubscriptionItemID + SourceChangeType, when
	// set, identify this invoice as a proration artifact generated by an
	// immediate item mutation (plan change, quantity change, item add, item
	// remove). Combined with TenantID + SubscriptionID they form the natural
	// key used to dedup retries of proration generation (see migrations 0026
	// + 0027 — the item id + change_type disambiguate simultaneous mutations
	// of different kinds that coincidentally share a wall-clock timestamp).
	SourcePlanChangedAt      *time.Time     `json:"source_plan_changed_at,omitempty"`
	SourceSubscriptionItemID string         `json:"source_subscription_item_id,omitempty"`
	SourceChangeType         ItemChangeType `json:"source_change_type,omitempty"`

	// BillingReason classifies the trigger that produced this invoice.
	// Stamped at create time and never mutated. Threshold-fired invoices
	// (BillingReasonThreshold) participate in the partial unique index
	// on (tenant, subscription, billing_period_start) so the threshold
	// scan re-tick is idempotent under retry.
	BillingReason InvoiceBillingReason `json:"billing_reason,omitempty"`

	// StripeInvoiceID is the source Stripe invoice id (in_xxx) for an invoice
	// imported from a Stripe account. Empty for Hardcount-native invoices. The
	// column and its partial unique index (idx_invoices_stripe_invoice_id;
	// migration 0063) remain from the Stripe importer that was deliberately
	// removed in the 2026-04-29 lean-cut — nothing currently writes this
	// field. The importer is deferred until a design partner announces a
	// cutover; the index already enforces dedup so a rebuilt importer is
	// idempotent on rerun without ever overwriting an existing row.
	StripeInvoiceID string `json:"stripe_invoice_id,omitempty"`

	// IsSimulated records, at write time, whether this invoice's domain
	// timestamps (created/issued/due/paid) were stamped on a test clock's
	// simulated time rather than wall-clock. Set true when the creating
	// context was bound to a frozen clock (engine: the subscription carries a
	// test_clock_id; manual composer: the customer is clock-pinned). The
	// activity timeline and invoice header read this authoritative flag to
	// render the "simulated" badge — NEVER a timestamp-vs-wall-clock heuristic
	// or a read-time re-derivation from the (mutable) parent's test_clock_id.
	// Always false in live mode (test clocks are test-mode only).
	IsSimulated bool `json:"is_simulated"`

	// TestClockID is the test clock this invoice's dates follow: the clock
	// its customer is pinned to. The pin is set when the customer is created,
	// never changes, and every subscription inherits it (ADR-027), so it
	// covers cycle and one-off invoices alike. Read from the customer, never
	// stored on the invoice. The list and get reads fill it; empty (left out)
	// for a wall-clock customer. The dashboard reads "Due in N days" from
	// this clock.
	TestClockID string `json:"test_clock_id,omitempty"`
}

// ItemChangeType classifies per-item proration artifacts so the dedup index
// distinguishes a plan-change and a quantity-change that happen to share the
// same (subscription, item, timestamp). Values mirror the CHECK constraint in
// migration 0027.
type ItemChangeType string

const (
	ItemChangeTypePlan     ItemChangeType = "plan"
	ItemChangeTypeQuantity ItemChangeType = "quantity"
	ItemChangeTypeAdd      ItemChangeType = "add"
	ItemChangeTypeRemove   ItemChangeType = "remove"
)

type InvoiceLineItemType string

const (
	LineTypeBaseFee InvoiceLineItemType = "base_fee"
	LineTypeUsage   InvoiceLineItemType = "usage"
	LineTypeAddOn   InvoiceLineItemType = "add_on"
)

// InvoiceTaxRetryUpdate is the snapshot UpdateTaxAtomic writes to a
// pending invoice when an operator (or the retry worker)
// triggers a tax recompute. Carries the new tax decision plus the
// resulting headline totals; per-line tax stamps travel via the
// lineItems argument.
//
// Empty TaxCalculationID + TaxStatus=pending signals the
// retry itself failed — the row stays blocked from finalize and the
// dashboard banner picks up the new error code.
type InvoiceTaxRetryUpdate struct {
	// SubtotalCents / DiscountCents carry the net values read off the tax
	// application. In tax-inclusive mode the provider carves tax out of the
	// gross, so these are smaller than the operator-entered gross; persisting
	// them keeps subtotal − discount + tax == gross. In exclusive mode they
	// equal the stored header values (a no-op write).
	SubtotalCents int64
	DiscountCents int64
	// TaxFacts: the shared 13-field calculation bundle (see tax_facts.go).
	TaxFacts
	TotalAmountCents int64
	// TaxNextRetryAt schedules the next reconciler attempt. nil
	// means "ready now"; a future timestamp gates the row out of
	// the worker's scan until the backoff window expires. Set
	// per the exponential schedule in internal/billing/tax_retry.go
	// when Status remains pending; cleared (nil) when
	// status flips to ok/exempt. Auto-finalize on a clean tax
	// outcome happens at the service layer (so it shares the same
	// public-token + tax-commit side-effects as manual Finalize),
	// not inside this atomic update.
	TaxNextRetryAt *time.Time
	// InitialAttempt marks the finalize-time FIRST computation of an
	// operator-composed invoice's tax — which is not a retry, so it
	// must not bump tax_retry_count. Before this flag every manual
	// invoice's first successful computation booked itself as
	// "retry #1" (through the shared UpdateTaxAtomic writer), making
	// tax_retry_count mean different things per invoice type: cycle
	// invoices' build-time computation never counted. With it, the
	// counter uniformly means "attempts AFTER the initial
	// computation" — the operator Retry-tax button and the ADR-017
	// reconciler still count every attempt they make.
	InitialAttempt bool
}

type InvoiceLineItem struct {
	ID        string              `json:"id"`
	InvoiceID string              `json:"invoice_id"`
	TenantID  string              `json:"tenant_id,omitempty"`
	LineType  InvoiceLineItemType `json:"line_type"`
	MeterID   string              `json:"meter_id,omitempty"`
	// MeterUnit is the meter's unit string stamped at write time on usage
	// lines ("tokens", "seconds", …), so invoice renderers can apply the
	// pricing section's display-scale convention (tokens quote per 1M —
	// see MeterUnitDisplayScale) without a join that would re-label
	// historical documents when a meter is edited. Empty on non-usage
	// lines and on lines written before migration 0173's backfill ran.
	MeterUnit   string `json:"meter_unit,omitempty"`
	Description string `json:"description"`
	// Quantity and QuantityDecimal are the two stored quantity columns. The
	// integer is the decimal truncated; QuantityDecimal is the exact
	// (possibly fractional) usage quantity, zero on lines that store none
	// (base-fee/proration/manual lines), which then use Quantity. Neither is
	// on the wire: MarshalJSON sends the one exact value, ExactQuantity, as
	// `quantity` (B5).
	Quantity        int64           `json:"-"`
	QuantityDecimal decimal.Decimal `json:"-"`
	// UnitAmountCents is the stored whole-cent unit price. It is not on the
	// wire: a sub-cent rate collapses to 0 in it, so the line sends
	// DisplayUnitAmountDecimal as `unit_amount_decimal` instead (B5).
	UnitAmountCents int64   `json:"-"`
	AmountCents     int64   `json:"amount_cents"`
	TaxRate         float64 `json:"tax_rate"` // Percent rate (4-decimal precision). ADR-042/043.
	TaxAmountCents  int64   `json:"tax_amount_cents"`
	TaxJurisdiction string  `json:"tax_jurisdiction,omitempty"`
	TaxCode         string  `json:"tax_code,omitempty"`
	// TaxabilityReason carries the Stripe-canonical structured reason
	// (e.g. "standard_rated", "reverse_charge", "not_collecting",
	// "customer_exempt", "product_exempt", "zero_rated"), or Hardcount's own
	// TaxReasonPrepaidCreditPurchase on a prepaid-commit purchase line under
	// the manual or Stripe Tax provider. The dashboard renders a badge for
	// non-trivial values, and the PDF appends a legend when a line is
	// customer- or product-exempt or a prepaid credit purchase. Otherwise
	// empty for non-Stripe providers.
	TaxabilityReason string `json:"tax_reason,omitempty"`
	TotalAmountCents int64  `json:"total_amount_cents"`
	Currency         string `json:"currency"`
	PricingMode      string `json:"pricing_mode,omitempty"`
	// PriceID is the plan price that billed a usage line, and DimensionMatch
	// the events that price takes (ADR-130 §16). Empty on other lines.
	PriceID        string            `json:"price_id,omitempty"`
	DimensionMatch map[string]string `json:"dimension_match,omitempty"`
	// NominalUnitAmountDecimal is the actually-billed CONFIGURED per-unit rate
	// (decimal cents) for per_unit usage lines — the clean rate-card figure
	// (0.0003 = $0.000003/token), unlike EffectiveUnitAmountDecimal
	// (amount÷quantity) which drifts by the cent-rounding of the line total
	// (e.g. 6,000 tokens billed 2¢ → effective 0.00033¢, nominal 0.0003¢).
	// It is STAMPED at build from the override-applied rule.UnitAmountCents,
	// NOT derived on read: a CustomerPriceOverride keeps the price id while
	// swapping the rate, so re-reading the price
	// yields list price, not the negotiated price billed (ADR-054
	// re-examination). Nil for every line with no single configured rate —
	// base_fee/proration (effective per-seat is the honest figure),
	// graduated/package usage (blended, no single rate), add_on/discount/tax
	// (whole-cent unit already equals nominal) — which fall back to the
	// effective rate. Internal-only (json:"-"): the wire's unit_amount_decimal
	// already carries the display value via DisplayUnitAmountDecimal, so
	// exposing this separately would just duplicate it on per_unit lines.
	NominalUnitAmountDecimal *decimal.Decimal `json:"-"`
	BillingPeriodStart       *time.Time       `json:"billing_period_start,omitempty"`
	BillingPeriodEnd         *time.Time       `json:"billing_period_end,omitempty"`
	Metadata                 map[string]any   `json:"metadata,omitempty"`
	CreatedAt                time.Time        `json:"created_at"`

	// CommitGrantedCents marks this line as a prepaid-commit purchase
	// (ADR-078): when the invoice finalizes, a credit block of this many
	// cents is granted to the customer (grant_kind='commit'). May differ
	// from AmountCents — discounted commits sell $10k of credits for $9k.
	// Only valid on add_on lines of manual invoices, at most one per
	// invoice, enforced in buildLineItem (CHECK constraint backstop in
	// migration 0136). Nil = not a commit line.
	CommitGrantedCents *int64 `json:"commit_granted_cents,omitempty"`
	// CommitExpiresAt is the granted block's expiry. Nil = never expires
	// (phase-1 default; term-aligned defaults arrive with recipe-carried
	// commits in phase 2).
	CommitExpiresAt *time.Time `json:"commit_expires_at,omitempty"`
}

// IsCommitLine reports whether this line funds a prepaid-commit grant.
func (li InvoiceLineItem) IsCommitLine() bool {
	return li.CommitGrantedCents != nil && *li.CommitGrantedCents > 0
}

// TaxReasonPrepaidCreditPurchase is the tax_reason on a prepaid-commit
// purchase line. The purchase is an advance payment: the line is left out of
// the tax request and carries no tax, and the usage the credit pays for is
// taxed when it is invoiced (ADR-078 amendment 2026-10-19).
const TaxReasonPrepaidCreditPurchase = "prepaid_credit_purchase"

// EffectiveUnitAmountDecimal is the per-unit price in DECIMAL CENTS, derived
// from the line's amount and quantity (amount_cents / quantity). It is the
// honest, full-precision unit price that reconciles with the rounded line
// amount (quantity × unit ≈ amount) and, unlike the whole-cent UnitAmountCents,
// does NOT collapse a sub-cent rate to 0 — e.g. 1000 units billed $3.00 →
// 0.3¢/unit = $0.003, not $0.00. It is derived on read from authoritative
// persisted values (never stored), so it can never drift from the amount;
// this is deliberately the EFFECTIVE rate, which stays well-defined for
// blended/tiered/multi-dimensional lines that have no single nominal rate.
// Precision is capped at 12 decimal places (Stripe unit_amount_decimal
// parity). Returns 0 when the quantity is 0 (no meaningful per-unit price).
func (li InvoiceLineItem) EffectiveUnitAmountDecimal() decimal.Decimal {
	return EffectiveUnitAmountDecimalFor(li.AmountCents, li.ExactQuantity())
}

// ExactQuantity is the line's quantity: the stored decimal when the line has
// one, else the stored integer. It is the line's `quantity` on the wire, and
// the value the dashboard, the hosted page and the PDF show.
func (li InvoiceLineItem) ExactQuantity() decimal.Decimal {
	if !li.QuantityDecimal.IsZero() {
		return li.QuantityDecimal
	}
	return decimal.NewFromInt(li.Quantity)
}

// EffectiveUnitAmountDecimalFor is the per-unit price in DECIMAL CENTS derived
// from an amount and quantity (amount ÷ quantity, capped at 12 decimal places
// for Stripe unit_amount_decimal parity). It is the honest, full-precision rate
// that reconciles with the rounded line amount and never collapses a sub-cent
// rate to 0. Well-defined for blended/tiered lines with no single nominal rate.
// Returns 0 when quantity ≤ 0. Shared by the invoice line, the usage Activity
// panel, and the public cost dashboard so the effective rate is computed
// identically on every surface.
func EffectiveUnitAmountDecimalFor(amountCents int64, quantity decimal.Decimal) decimal.Decimal {
	if quantity.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return decimal.NewFromInt(amountCents).DivRound(quantity, 12)
}

// DisplayUnitAmountDecimalFor is the per-unit price to SHOW for a LIVE-resolved
// price (decimal cents): the configured nominal rate for per_unit, else the
// effective amount÷quantity. It mirrors InvoiceLineItem.DisplayUnitAmountDecimal
// — which reads the nominal stamped at invoice build — for surfaces that resolve
// the price live (the usage Activity panel and the public cost dashboard),
// so the unit price matches the invoice on every screen (ADR-054).
func DisplayUnitAmountDecimalFor(rule PriceShape, amountCents int64, quantity decimal.Decimal) decimal.Decimal {
	if nominal := NominalUnitAmountDecimal(rule); nominal != nil {
		return *nominal
	}
	return EffectiveUnitAmountDecimalFor(amountCents, quantity)
}

// DisplayUnitAmountDecimal is the per-unit price to SHOW on invoices (decimal
// cents): the stamped nominal configured rate when present (per_unit usage lines),
// else the effective amount÷quantity rate. This is the single value the
// dashboard wire form, the hosted DTO, and the PDF all render, so the
// displayed rate is consistent across surfaces and clean for sub-cent per_unit
// rates — $0.000003, not the effective $0.00000333333333 that the line's
// cent-rounding produces (ADR-054 re-examination). See
// NominalUnitAmountDecimal for why nominal is stamped, not derived on read.
func (li InvoiceLineItem) DisplayUnitAmountDecimal() decimal.Decimal {
	if li.NominalUnitAmountDecimal != nil {
		return *li.NominalUnitAmountDecimal
	}
	return li.EffectiveUnitAmountDecimal()
}

// MarshalJSON gives the line one quantity and one unit price on the wire
// (B5): `quantity` is ExactQuantity as a decimal string, and
// `unit_amount_decimal` is the full-precision per-unit price in decimal cents
// from DisplayUnitAmountDecimal (the nominal configured rate on per_unit usage
// lines, else the effective amount÷quantity rate), so a sub-cent rate never
// collapses to "$0.00" (ADR-054). Both are computed on the fly.
func (li InvoiceLineItem) MarshalJSON() ([]byte, error) {
	type alias InvoiceLineItem
	return json.Marshal(struct {
		alias
		Quantity          string `json:"quantity"`
		UnitAmountDecimal string `json:"unit_amount_decimal"`
	}{alias(li), li.ExactQuantity().String(), li.DisplayUnitAmountDecimal().String()})
}

// InvoiceKeyset is a position in a queue read oldest-first by (created_at, id).
// The zero value means "from the start". Both columns are fixed at insert, so
// the cursor only moves forward and never repeats a row. A sweep that pages by
// it visits every row queued when the pass reached its position, however many
// rows ahead of it never leave; a row queued behind the cursor mid-pass is
// visited on the next pass.
type InvoiceKeyset struct {
	CreatedAt time.Time
	ID        string
}

// KeysetOf returns inv's position, for the next page's cursor.
func KeysetOf(inv Invoice) InvoiceKeyset {
	return InvoiceKeyset{CreatedAt: inv.CreatedAt, ID: inv.ID}
}
