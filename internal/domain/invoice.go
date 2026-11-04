package domain

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

// InvoiceStatus is where an invoice is in its life: draft, finalized, paid,
// voided or uncollectible.
type InvoiceStatus string

const (
	InvoiceDraft     InvoiceStatus = "draft"
	InvoiceFinalized InvoiceStatus = "finalized"
	InvoicePaid      InvoiceStatus = "paid"
	InvoiceVoided    InvoiceStatus = "voided"
	// InvoiceUncollectible marks an invoice that no further collection is
	// attempted on, while it stays in financial reporting. These are
	// Stripe's standard semantics. It differs from Voided, which annuls the
	// invoice. Dunning's mark_uncollectible terminal invoice action sets it,
	// and so does an operator (ADR-036 amendment).
	InvoiceUncollectible InvoiceStatus = "uncollectible"
)

// InvoiceBillingReason classifies the trigger that produced an invoice. It
// mirrors Stripe's invoice.billing_reason. The invoices table stores it as
// nullable, and older rows are NULL.
//
// SubscriptionCycle is the invoice the cycle scan emits at the end of a
// natural period. SubscriptionCreate is the prorated first invoice when a
// subscription starts mid-cycle. Manual is a standalone invoice an operator
// creates. Threshold is the early invoice that the threshold scan tick
// finalizes when usage crosses a spend threshold.
type InvoiceBillingReason string

const (
	BillingReasonSubscriptionCycle  InvoiceBillingReason = "subscription_cycle"
	BillingReasonSubscriptionCreate InvoiceBillingReason = "subscription_create"
	// BillingReasonSubscriptionCancel is the final partial-period invoice
	// emitted at a mid-period immediate cancel. It covers the in_arrears
	// prorated base and the usage from current_period_start to canceled_at.
	// It is distinct from subscription_cycle, so reporting and the dashboard
	// can label it as a true-up at cancel, not a normal cycle close.
	BillingReasonSubscriptionCancel InvoiceBillingReason = "subscription_cancel"
	// BillingReasonSubscriptionUpdate is the immediate proration invoice cut
	// by a mid-period subscription change: a plan upgrade, a quantity
	// increase or an item add. Stripe stamps the same `subscription_update`
	// reason for all of these. With it, the dashboard can label what
	// triggered the invoice.
	BillingReasonSubscriptionUpdate InvoiceBillingReason = "subscription_update"
	BillingReasonManual             InvoiceBillingReason = "manual"
	BillingReasonThreshold          InvoiceBillingReason = "threshold"
)

// InvoicePaymentStatus is the state of collecting an invoice's payment.
type InvoicePaymentStatus string

const (
	PaymentPending    InvoicePaymentStatus = "pending"
	PaymentProcessing InvoicePaymentStatus = "processing"
	PaymentSucceeded  InvoicePaymentStatus = "succeeded"
	PaymentFailed     InvoicePaymentStatus = "failed"
	// PaymentUnknown marks a Stripe charge attempt that returned an ambiguous
	// error (5xx, timeout, connection reset), where the response cannot tell
	// us whether Stripe processed the charge. A reconciler resolves these by
	// asking Stripe after a cool-off window.
	PaymentUnknown InvoicePaymentStatus = "unknown"
)

// IsInFlight reports whether a charge attempt is still settling. Its outcome
// is not final yet, so the captured amount is unknown. This is the one
// definition of "in flight", and it has two states:
//   - processing: the charge is open at the provider (for example, an
//     off-session SCA requires_action maps here).
//   - unknown: an ambiguous provider error that may or may not have charged.
//     A reconciler resolves it.
//
// Any path that changes an invoice must refuse or defer while this is true.
// That covers void, mark-uncollectible, record-offline-payment, and a credit
// note or clawback that lowers amount_due. Changing an invoice with an open
// payment strands captured money, reverses tax on a sale that may complete,
// or records too little amount_paid against a lowered amount_due. Stripe
// enforces the same rule ("you can't void, edit, or mark uncollectible an
// invoice with open payments").
//
// Code outside this file asks Invoice.PaymentInFlight instead, which answers
// for the whole invoice row. A test in internal/arch enforces that.
func (s InvoicePaymentStatus) IsInFlight() bool {
	return s == PaymentProcessing || s == PaymentUnknown
}

// PaymentInFlight reports whether a payment may be open on this invoice: one
// that may still take money, so its outcome is not known yet. Every action
// that must refuse or wait while that is true asks here. These are void,
// write-off, offline payment, credit notes, Collect, invoice emails, the
// hosted Pay button, proration's credit path and dunning's final action.
//
// Today the answer is the payment status alone (IsInFlight). It lives on the
// invoice, not on the status, so that a fact stored elsewhere on the invoice
// row can join it here once, not at each caller. #976 adds such a fact: a
// record of a charge sent to Stripe whose result is not saved yet.
func (i Invoice) PaymentInFlight() bool {
	return i.PaymentStatus.IsInFlight()
}

// Parked reports whether the invoice's last charge got an unclear answer from
// Stripe that named no PaymentIntent (ADR-107): payment_status 'unknown' with
// no PaymentIntent ID. No charge path takes it, and no sweep can look the
// payment up by ID. The stale-hold sweep finishes it from Stripe's list of
// the customer's payments. It records or follows a payment that appears. If
// none has appeared ParkedNothingWait after the attempt, it puts the invoice
// back where a charge picks it up (ADR-127 amendment 2026-10-20).
func (i Invoice) Parked() bool {
	return i.PaymentStatus == PaymentUnknown && i.StripePaymentIntentID == ""
}

// HasCardPayment reports whether this invoice's settled money is refundable to
// a card: a Stripe PaymentIntent backs it. False for a never-charged invoice,
// one paid by credits or with nothing due, and one paid out of band
// (PaidOutOfBand).
//
// It is the one answer to "can money go back the way it came?". A bare
// emptiness check gives the same answer, because the PaymentIntent column
// holds only Stripe's IDs, and an offline payment leaves it empty (migration
// 00203 refuses anything else).
func (i Invoice) HasCardPayment() bool {
	return i.StripePaymentIntentID != ""
}

// CardRefundableCents is the most a credit note may push back to the card:
// what the card settled, less the refunds already committed by earlier
// credit notes that are not voided. It is zero when no card backs the
// payment. That puts the offline case under the same cap the over-refund
// path already enforces, with no second guard beside it.
func (i Invoice) CardRefundableCents(priorRefundCents int64) int64 {
	if !i.HasCardPayment() {
		return 0
	}
	return max(0, i.AmountPaidCents-priorRefundCents)
}

// UnpaidRelievableCents is the most a credit note can relieve on an invoice
// that has not been paid: what is still due, plus the customer credits
// applied to it. A note first reduces the amount due. The rest of it
// displaces applied credits, which go back to the customer's balance when
// the note is issued (creditnote.Issue, ADR-061 amendment 2026-10-12).
// Sizing a note against the amount due alone would drop those credits.
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

// Provider PaymentIntent statuses, verbatim from Stripe. These name the
// values that can land in Invoice.ProviderPaymentStatus (migration 0167), so
// stored data and tests read against symbols, not loose string literals. No
// logic branches on them today. The column is recorded for queue ordering
// and for diagnosing a long-running payment.
const (
	ProviderPIRequiresPaymentMethod = "requires_payment_method"
	ProviderPIRequiresConfirmation  = "requires_confirmation"
	ProviderPIRequiresAction        = "requires_action"
	ProviderPIProcessing            = "processing"
	ProviderPIRequiresCapture       = "requires_capture"
	ProviderPISucceeded             = "succeeded"
	ProviderPICanceled              = "canceled"
)

// InvoiceTaxStatus tracks whether tax has been calculated for an invoice.
// The normal case is ok: calculation succeeded, including the zero-tax
// outcomes of none, manual, exempt and reverse-charge. Pending means
// calculation failed and the invoice waits. tax_error_code says whether the
// retry worker tries again or the operator must act.
//
// A pending invoice cannot be finalized. Sending a wrong tax amount creates
// audit and compliance exposure, so the invoice waits and never silently
// falls back to an incorrect rate.
type InvoiceTaxStatus string

const (
	InvoiceTaxOK      InvoiceTaxStatus = "ok"
	InvoiceTaxPending InvoiceTaxStatus = "pending"
)

// InvoiceFinalizationStatus returns the Status an invoice must be stamped
// with at creation, given its tax and pause-collection state. It is the one
// source of this rule for all four invoice-emitting paths
// (engine.billOnePeriod, engine.BillOnCreate,
// engine.BillFinalOnImmediateCancel, subscription.handleItemProration). So
// the rule "tax_status=pending or pause_collection set → draft" cannot drift
// between flows.
//
//   - Tax pending: a finalized invoice implies its amounts are final. With
//     tax deferred, TotalAmountCents is the subtotal plus zero tax, which is
//     wrong at finalize. The retry worker moves the invoice from draft to
//     finalized when calculation succeeds.
//   - Pause-collection set: this matches Stripe. The cycle continues and
//     line items are captured, but finalize, charge and dunning are skipped
//     until the pause is cleared.
//
// pauseCollection is the subscription's *PauseCollection (nil means
// collection runs normally). at is the instant the invoice bills to: a
// period's end, a cancel's canceled_at, or now. The pause is read as it
// stood then, because a close can run after resumes_at has passed for a
// period that ended while still paused.
func InvoiceFinalizationStatus(taxStatus InvoiceTaxStatus, pauseCollection *PauseCollection, at time.Time) InvoiceStatus {
	if taxStatus == InvoiceTaxPending {
		return InvoiceDraft
	}
	if pauseCollection.InForceAt(at) {
		return InvoiceDraft
	}
	return InvoiceFinalized
}

// Invoice is one bill to a customer: its totals, its status, the state of
// its payment and its tax.
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
	// TaxFacts is the 13-field tax-calculation bundle. It is embedded
	// untagged, so its JSON keys sit flat on the invoice, byte for byte as
	// the flat fields it replaced (TestInvoiceTaxFactsWireCompat pins this).
	// tax_facts.go says why this is one shared type and not hand-copied
	// mirrors.
	TaxFacts
	// TaxTransactionID is the committed upstream transaction reference
	// (Stripe Tax: tx_xxx), captured at finalize. It is the provider's
	// durable record of the tax decision, and the handle the provider needs
	// for a later reversal when a credit note is issued against this
	// invoice. It is empty for providers without durable state (none,
	// manual), and for invoices whose finalize has not committed tax yet. It
	// belongs to the invoice's lifecycle, so it is not part of TaxFacts on
	// purpose: Commit writes it, and the calculation does not produce it.
	// The retry bookkeeping below is the same.
	TaxTransactionID string `json:"tax_transaction_id,omitempty"`
	TaxRetryCount    int    `json:"tax_retry_count,omitempty"`

	// The PaymentAnomaly* fields are the durable marker of an anomaly on the
	// settle path (ADR-068). Kind is the per-cause event name
	// (payment.duplicate_charge, payment.amount_mismatch or
	// payment.received_on_voided_invoice). The PaymentIntent ID is the
	// offending PaymentIntent, and captured is the amount Stripe took. There
	// is one slot, and the last anomaly wins. It drives the Critical
	// attention banner, even on terminal invoices.
	PaymentAnomalyKind            string `json:"payment_anomaly_kind,omitempty"`
	PaymentAnomalyPaymentIntentID string `json:"payment_anomaly_payment_intent_id,omitempty"`
	PaymentAnomalyCapturedCents   int64  `json:"payment_anomaly_captured_cents,omitempty"`
	// TaxNextRetryAt is the soonest the background reconciler may re-run the
	// tax calculation. NULL means "ready now": never tried, or an operator's
	// retry just reset the schedule. A future time means an earlier attempt
	// failed with a retryable code, and the next attempt waits until then,
	// per the exponential backoff curve in internal/billing/tax_retry.go
	// (migration 0074, ADR-017).
	TaxNextRetryAt *time.Time `json:"tax_next_retry_at,omitempty"`
	// TaxReversedAt records when the upstream tax transaction was reversed
	// on void or mark-uncollectible, or when there was nothing left to
	// reverse. NULL on a voided or uncollectible stripe_tax invoice that
	// still carries a TaxTransactionID means the reversal is not confirmed
	// yet. RetryPendingTaxReversal keys on this to re-drive a failed
	// reversal, as the #267 commit reconciler does for commits (migration
	// 0122).
	TaxReversedAt *time.Time `json:"tax_reversed_at,omitempty"`
	// PaymentCardBrand and PaymentCardLast4 record the card that settled
	// this invoice. They are filled at payment_intent.succeeded, by looking
	// up the PaymentIntent's payment_method in the payment_methods table.
	// They are empty when the payment method is unknown to us (one-off
	// Checkout cards). The activity timeline shows them as a sub-line on the
	// "Invoice paid" row (migration 0077, ADR-020).
	PaymentCardBrand string `json:"payment_card_brand,omitempty"`
	PaymentCardLast4 string `json:"payment_card_last4,omitempty"`
	// Attention is the one "needs operator attention" field, computed on
	// read by ClassifyInvoiceAttention. It is never stored. It is always
	// derived from the durable fields above (tax_status, tax_error_code,
	// payment_status, last_payment_error) plus due_at. It is nil when the
	// invoice is healthy (terminal, or no failure mode active).
	// docs/adr/009-invoice-attention.md has the wire-shape contract.
	Attention *Attention `json:"attention,omitempty"`
	// RecoveryWarning is what an operator must know before recording an
	// offline payment on a written-off invoice. It names a consequence that
	// will go unreconciled, and it is not a refusal. It is nil when there is
	// nothing to say.
	RecoveryWarning     *PaymentBlock `json:"recovery_warning,omitempty"`
	TotalAmountCents    int64         `json:"total_amount_cents"`
	AmountDueCents      int64         `json:"amount_due_cents"`
	AmountPaidCents     int64         `json:"amount_paid_cents"`
	CreditsAppliedCents int64         `json:"credits_applied_cents"`
	BillingPeriodStart  time.Time     `json:"billing_period_start"`
	BillingPeriodEnd    time.Time     `json:"billing_period_end"`
	// BillingTimezone is the IANA timezone in which the period boundaries
	// above fall on local midnight. It is copied from the subscription's
	// snapshot (ADR-077) when the invoice is created, and it never changes
	// after that. BillingPeriodDisplay is computed in this zone, not the live
	// tenant timezone. So the step back to the inclusive last day lands on
	// the right local date, even after a tenant changes its timezone. Manual
	// invoices stamp the org zone at creation too. It is empty only on older
	// rows: engine rows written before migration 0134, and manual rows
	// written before manual invoices stamped it. The read path then falls
	// back to the live tenant timezone.
	BillingTimezone string `json:"billing_timezone,omitempty"`
	// BillToSnapshot and SupplierSnapshot are the buyer and seller details
	// this invoice was issued with (ADR-109), written in the transaction that
	// took it out of draft. Nil on drafts, which render live, and on invoices
	// issued before migration 0177, which keep rendering live because nothing
	// records what they showed.
	BillToSnapshot   *DocumentBillTo   `json:"bill_to_snapshot,omitempty"`
	SupplierSnapshot *DocumentSupplier `json:"supplier_snapshot,omitempty"`
	// BillingPeriodDisplay is the period as people read it, ending on the
	// inclusive last covered day ("Jun 1, 2028 – Jun 30, 2028"). It shows
	// dates only, in the tenant timezone, which is the industry-standard
	// period display (ADR-058). It is computed on read and never stored. The
	// raw half-open BillingPeriodStart and BillingPeriodEnd above stay as
	// they are (the SDK contract). It is empty for one-off and no-period
	// invoices, so callers leave out the period row. Every surface that shows
	// the period (PDF, dashboard, list) shows this one value as is, so the Go
	// and TS code cannot drift apart. The hosted invoice page shows no
	// period, and its linked PDF does.
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
	// outside Hardcount (RecordOfflinePayment: a bank transfer, a cheque,
	// cash). It is Stripe's paid_out_of_band. Such an invoice has no
	// PaymentIntent (a CHECK enforces both), so there is no card to refund.
	PaidOutOfBand     bool   `json:"paid_out_of_band"`
	LastPaymentError  string `json:"last_payment_error,omitempty"`
	AutoChargePending bool   `json:"auto_charge_pending,omitempty"`
	// NoPMNotifiedAt is the send-once marker for the "payment method needed"
	// setup-link email. Whichever sender delivered the email stamps it: the
	// collect at finalize, or the auto-charge retry sweep. The sweep checks
	// it, so an invoice with no payment method is emailed exactly once, not
	// on every tick (ADR-087).
	NoPMNotifiedAt *time.Time `json:"no_pm_notified_at,omitempty"`
	// InvoiceEmailedAt is the send-once marker for the invoice email that the
	// auto-charge collector sends when Stripe is not connected for the
	// invoice's mode (ADR-060 amendment 2026-10-30). It is stamped after the
	// email is queued and checked so the next visit sends nothing more. An
	// operator's "Email invoice" does not stamp it. The finalize response
	// carries it, so "Save & send" can skip its own send when the collector
	// already emailed the invoice.
	InvoiceEmailedAt *time.Time `json:"invoice_emailed_at,omitempty"`
	// BelowMinimumAt is when Stripe last refused to charge this invoice
	// because its amount is below Stripe's minimum charge
	// (ErrBelowMinimumCharge). The charge path stamps it, sets
	// payment_status 'pending' and clears the auto-charge queue, in one
	// write. It is Stripe's latest answer, not a permanent fact, because the
	// minimum of a converted charge follows the account's settlement
	// currency. The next charge outcome clears it. Readers check
	// Invoice.BelowStripeMinimum. It stays internal, and the notice is the
	// API's view of it.
	BelowMinimumAt *time.Time `json:"-"`
	// ChargeAttemptSeq seeds the Stripe charge idempotency key
	// (payment.ChargeIdempotencyKey). It moves only when a charge attempt's
	// outcome is recorded: every UPDATE that stamps stripe_payment_intent_id
	// bumps it, and nothing else touches it. A crash records no outcome, so
	// the seq and the key stay the same, and the retry of a crashed attempt
	// lands on the same PaymentIntent. A retry after a recorded decline gets
	// a fresh one. It is never serialized, because it is a payment-protocol
	// detail, not invoice data an operator reads.
	ChargeAttemptSeq int64 `json:"-"`

	// ProviderPaymentStatus is Stripe's verbatim PaymentIntent status at the
	// last reconciler poll, and ProviderSyncedAt is when we looked (migration
	// 0167). They are observations. PaymentStatus stays the authoritative
	// money state. These fields never override it, and nothing derives money
	// from them.
	//
	// Without them, a payment waiting on the customer looks the same as one
	// settling normally. Recording them lets the sweep rotate instead of
	// sticking on a frozen row. It lets the banner say "waiting on the
	// customer since <date>" instead of promising resolution. And it lets
	// the gate offer a cancel when the provider itself, not a guess here,
	// says no money moved.
	//
	// Empty or zero means never polled: every row from before 0167, and
	// every invoice whose payment has not been in flight long enough for a
	// sweep. Treat unknown-because-unpolled as "no information", never as
	// "not stuck".
	ProviderPaymentStatus string     `json:"provider_payment_status,omitempty"`
	ProviderSyncedAt      *time.Time `json:"provider_synced_at,omitempty"`

	PDFObjectKey string `json:"-"`
	// PublicToken is the credential in the hosted invoice URL, like Stripe's
	// hosted_invoice_url. It is generated at finalize, and drafts have an
	// empty token. The rotate-public-token endpoint replaces it if the URL
	// ever leaks. It is left out of the JSON when empty, so list responses
	// stay clean on older invoices that were never given one.
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

	// SourcePlanChangedAt, SourceSubscriptionItemID and SourceChangeType,
	// when set, mark this invoice as a proration invoice from an immediate
	// item change (plan change, quantity change, item add, item remove).
	// With TenantID and SubscriptionID they form the natural key that dedups
	// retries of proration (migrations 0026 and 0027). The item ID and
	// change_type tell apart changes of different kinds that happen to share
	// a wall-clock timestamp.
	SourcePlanChangedAt      *time.Time     `json:"source_plan_changed_at,omitempty"`
	SourceSubscriptionItemID string         `json:"source_subscription_item_id,omitempty"`
	SourceChangeType         ItemChangeType `json:"source_change_type,omitempty"`

	// BillingReason classifies the trigger that produced this invoice. It is
	// stamped at creation and never changed. Invoices from a threshold
	// (BillingReasonThreshold) are in the partial unique index on (tenant,
	// subscription, billing_period_start), so a repeated threshold scan tick
	// is idempotent under retry.
	BillingReason InvoiceBillingReason `json:"billing_reason,omitempty"`

	// IsSimulated records, at write time, whether this invoice's domain
	// timestamps (created, issued, due, paid) came from a test clock's
	// simulated time instead of wall-clock time. It is true when the
	// creating context was bound to a frozen clock. For the engine, the
	// subscription carries a test_clock_id. For the manual composer, the
	// customer is clock-pinned (on a test clock). The activity timeline and
	// the invoice header read this flag to show the "simulated" badge. They
	// never use a timestamp-versus-wall-clock heuristic, or re-derive it at
	// read time from the parent's test_clock_id, which can change. It is
	// always false in live mode, because test clocks exist only in test mode.
	IsSimulated bool `json:"is_simulated"`

	// TestClockID is the test clock this invoice's dates follow: the clock
	// its customer is pinned to. The pin is set when the customer is
	// created, never changes, and every subscription inherits it (ADR-027).
	// So it covers cycle and one-off invoices alike. It is read from the
	// customer and never stored on the invoice. The list and get reads fill
	// it, and it is empty (left out) for a wall-clock customer. The
	// dashboard reads "Due in N days" from this clock.
	TestClockID string `json:"test_clock_id,omitempty"`
}

// ItemChangeType classifies per-item proration invoices, so the dedup index
// tells apart a plan change and a quantity change that happen to share the
// same (subscription, item, timestamp). The values mirror the CHECK
// constraint in migration 0027.
type ItemChangeType string

const (
	ItemChangeTypePlan     ItemChangeType = "plan"
	ItemChangeTypeQuantity ItemChangeType = "quantity"
	ItemChangeTypeAdd      ItemChangeType = "add"
	ItemChangeTypeRemove   ItemChangeType = "remove"
)

// InvoiceLineItemType is what a line bills: the base fee, usage, or an
// add-on.
type InvoiceLineItemType string

const (
	LineTypeBaseFee InvoiceLineItemType = "base_fee"
	LineTypeUsage   InvoiceLineItemType = "usage"
	LineTypeAddOn   InvoiceLineItemType = "add_on"
)

// InvoiceTaxRetryUpdate is the snapshot UpdateTaxAtomic writes to a pending
// invoice when an operator (or the retry worker) triggers a tax recompute.
// It carries the new tax decision and the resulting headline totals. The
// per-line tax stamps travel in the lineItems argument.
//
// An empty TaxCalculationID with TaxStatus=pending means the retry itself
// failed. The row stays blocked from finalize, and the dashboard banner
// picks up the new error code.
type InvoiceTaxRetryUpdate struct {
	// SubtotalCents and DiscountCents carry the net values read off the tax
	// application. In tax-inclusive mode the provider carves tax out of the
	// gross, so these are smaller than the gross the operator entered.
	// Storing them keeps subtotal − discount + tax == gross. In exclusive
	// mode they equal the stored header values, and the write changes
	// nothing.
	SubtotalCents int64
	DiscountCents int64
	// TaxFacts is the shared 13-field calculation bundle (see tax_facts.go).
	TaxFacts
	TotalAmountCents int64
	// TaxNextRetryAt schedules the next reconciler attempt. nil means "ready
	// now". A future time keeps the row out of the worker's scan until the
	// backoff window ends. It is set per the exponential schedule in
	// internal/billing/tax_retry.go while Status stays pending, and cleared
	// (nil) when the status moves to ok or exempt. Auto-finalize on a clean
	// tax outcome happens at the service layer, not inside this atomic
	// update. That way it shares the public-token and tax-commit side
	// effects of a manual Finalize.
	TaxNextRetryAt *time.Time
	// InitialAttempt marks the first tax computation of an
	// operator-composed invoice, at finalize. It is not a retry, so it must
	// not bump tax_retry_count. With this flag, the counter means "attempts
	// after the initial computation" on every invoice type, as it does for
	// cycle invoices, whose computation at build time never counts. The
	// operator's Retry-tax button and the ADR-017 reconciler still count
	// every attempt they make.
	InitialAttempt bool
	// BasisSubtotalCents and BasisDiscountCents are the invoice header the
	// computation started from. UpdateTaxAtomic refuses the write when,
	// under its lock, the header no longer holds them or the invoice's lines
	// are not the ones taxed. Otherwise a line added during the computation
	// would be left out of the total the write replaces (#1298).
	BasisSubtotalCents int64
	BasisDiscountCents int64
}

// InvoiceLineItem is one line of an invoice: what it bills, its quantity
// and price, its amount and its tax.
type InvoiceLineItem struct {
	ID        string              `json:"id"`
	InvoiceID string              `json:"invoice_id"`
	TenantID  string              `json:"tenant_id,omitempty"`
	LineType  InvoiceLineItemType `json:"line_type"`
	MeterID   string              `json:"meter_id,omitempty"`
	// MeterUnit is the meter's unit ("tokens", "seconds", …), stamped on
	// usage lines at write time. Invoice renderers use it to apply the
	// pricing display-scale convention (token rates are quoted per 1M, see
	// MeterUnitDisplayScale). A join would re-label old documents when a
	// meter is edited, so the unit is stamped. It is empty on non-usage
	// lines, and on lines written before migration 0173's backfill ran.
	MeterUnit   string `json:"meter_unit,omitempty"`
	Description string `json:"description"`
	// Quantity and QuantityDecimal are the two stored quantity columns. The
	// integer is the decimal truncated. QuantityDecimal is the exact
	// (possibly fractional) usage quantity. It is zero on lines that store
	// none (base-fee, proration and manual lines), which then use Quantity.
	// Neither is on the wire: MarshalJSON sends the one exact value,
	// ExactQuantity, as `quantity` (#1374).
	Quantity        int64           `json:"-"`
	QuantityDecimal decimal.Decimal `json:"-"`
	// UnitAmountCents is the stored whole-cent unit price. It is not on the
	// wire, because a sub-cent rate collapses to 0 in it. The line sends
	// DisplayUnitAmountDecimal as `unit_amount_decimal` instead (#1374).
	UnitAmountCents int64   `json:"-"`
	AmountCents     int64   `json:"amount_cents"`
	TaxRate         float64 `json:"tax_rate"` // Percent rate (4-decimal precision). ADR-042/043.
	TaxAmountCents  int64   `json:"tax_amount_cents"`
	TaxJurisdiction string  `json:"tax_jurisdiction,omitempty"`
	TaxCode         string  `json:"tax_code,omitempty"`
	// TaxabilityReason carries Stripe's structured reason (for example
	// "standard_rated", "reverse_charge", "not_collecting",
	// "customer_exempt", "product_exempt", "zero_rated"). On a
	// prepaid-commit purchase line under the manual or Stripe Tax provider,
	// it carries Hardcount's own TaxReasonPrepaidCreditPurchase. The
	// dashboard shows a badge for non-trivial values, and the PDF adds a
	// legend when a line is customer-exempt, product-exempt or a prepaid
	// credit purchase. Otherwise it is empty for non-Stripe providers.
	TaxabilityReason string `json:"tax_reason,omitempty"`
	TotalAmountCents int64  `json:"total_amount_cents"`
	Currency         string `json:"currency"`
	PricingMode      string `json:"pricing_mode,omitempty"`
	// PriceID is the plan price that billed a usage line, and DimensionMatch
	// the events that price takes (ADR-130 §16). Empty on other lines.
	PriceID        string            `json:"price_id,omitempty"`
	DimensionMatch map[string]string `json:"dimension_match,omitempty"`
	// NominalUnitAmountDecimal is the configured per-unit rate that was
	// billed (decimal cents) on per_unit usage lines. It is the clean
	// rate-card figure (0.0003 = $0.000003/token). EffectiveUnitAmountDecimal
	// (amount÷quantity) differs by the cent rounding of the line total: 6,000
	// tokens billed 2¢ give an effective 0.00033¢ and a nominal 0.0003¢.
	//
	// It is stamped at build from the override-applied rule.UnitAmountCents,
	// not derived on read. A CustomerPriceOverride keeps the price ID while
	// it swaps the rate, so re-reading the price gives the list price, not
	// the negotiated price billed (ADR-054).
	//
	// It is nil on every line with no single configured rate, and those
	// lines fall back to the effective rate:
	//   - base_fee and proration lines, where the effective per-seat rate is
	//     the true figure;
	//   - graduated and package usage, which is blended, with no single rate;
	//   - add_on, discount and tax lines, whose whole-cent unit already
	//     equals the nominal rate.
	//
	// It is internal (json:"-"). The wire's unit_amount_decimal already
	// carries the display value through DisplayUnitAmountDecimal, so sending
	// this too would duplicate it on per_unit lines.
	NominalUnitAmountDecimal *decimal.Decimal `json:"-"`
	BillingPeriodStart       *time.Time       `json:"billing_period_start,omitempty"`
	BillingPeriodEnd         *time.Time       `json:"billing_period_end,omitempty"`
	Metadata                 map[string]any   `json:"metadata,omitempty"`
	CreatedAt                time.Time        `json:"created_at"`

	// CommitGrantedCents marks this line as a prepaid-commit purchase
	// (ADR-078). When the invoice finalizes, the customer is granted a
	// credit block of this many cents (grant_kind='commit'). It may differ
	// from AmountCents, because a discounted commit sells $10k of credits
	// for $9k. It is valid only on add_on lines of manual invoices, at most
	// one per invoice. buildLineItem enforces that, and a CHECK constraint
	// in migration 0136 backs it up. Nil means not a commit line.
	CommitGrantedCents *int64 `json:"commit_granted_cents,omitempty"`
	// CommitExpiresAt is the granted block's expiry. Nil means it never
	// expires, the phase-1 default. Term-aligned defaults arrive with
	// recipe-carried commits in phase 2.
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

// EffectiveUnitAmountDecimal is the per-unit price in decimal cents, derived
// from the line's amount and quantity (amount_cents / quantity). It is the
// full-precision unit price that reconciles with the rounded line amount
// (quantity × unit ≈ amount). Unlike the whole-cent UnitAmountCents, it
// does not collapse a sub-cent rate to 0: 1000 units billed $3.00 give
// 0.3¢/unit = $0.003, not $0.00. It is derived on read from the stored
// values and never stored itself, so it cannot drift from the amount. It is
// the effective rate on purpose, because that stays well defined for
// blended, tiered and multi-dimensional lines with no single nominal rate.
// Precision is capped at 12 decimal places, like Stripe's
// unit_amount_decimal. It returns 0 when the quantity is 0, where a
// per-unit price means nothing.
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

// EffectiveUnitAmountDecimalFor is the per-unit price in decimal cents,
// derived from an amount and a quantity (amount ÷ quantity). It is capped at
// 12 decimal places, like Stripe's unit_amount_decimal. It is the
// full-precision rate that reconciles with the rounded line amount, and it
// never collapses a sub-cent rate to 0. It is well defined for blended and
// tiered lines with no single nominal rate. It returns 0 when quantity ≤ 0.
// The invoice line, the usage Activity panel and the public cost dashboard
// all use it, so the effective rate is computed the same way on every
// surface.
func EffectiveUnitAmountDecimalFor(amountCents int64, quantity decimal.Decimal) decimal.Decimal {
	if quantity.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return decimal.NewFromInt(amountCents).DivRound(quantity, 12)
}

// DisplayUnitAmountDecimalFor is the per-unit price to show for a price that
// is resolved live (decimal cents): the configured nominal rate for
// per_unit, else the effective amount÷quantity. It mirrors
// InvoiceLineItem.DisplayUnitAmountDecimal, which reads the nominal rate
// stamped at invoice build. It serves the surfaces that resolve the price
// live (the usage Activity panel and the public cost dashboard), so the unit
// price matches the invoice on every screen (ADR-054).
func DisplayUnitAmountDecimalFor(rule PriceShape, amountCents int64, quantity decimal.Decimal) decimal.Decimal {
	if nominal := NominalUnitAmountDecimal(rule); nominal != nil {
		return *nominal
	}
	return EffectiveUnitAmountDecimalFor(amountCents, quantity)
}

// DisplayUnitAmountDecimal is the per-unit price to show on invoices (decimal
// cents): the stamped nominal configured rate when present (per_unit usage
// lines), else the effective amount÷quantity rate. The dashboard wire form,
// the hosted DTO and the PDF all show this one value. So the rate is the
// same on every surface, and clean for sub-cent per_unit rates: $0.000003,
// not the effective $0.00000333333333 that the line's cent rounding gives
// (ADR-054). The field NominalUnitAmountDecimal says why the nominal rate is
// stamped, not derived on read.
func (li InvoiceLineItem) DisplayUnitAmountDecimal() decimal.Decimal {
	if li.NominalUnitAmountDecimal != nil {
		return *li.NominalUnitAmountDecimal
	}
	return li.EffectiveUnitAmountDecimal()
}

// MarshalJSON gives the line one quantity and one unit price on the wire
// (#1374). `quantity` is ExactQuantity as a decimal string.
// `unit_amount_decimal` is the full-precision per-unit price in decimal
// cents from DisplayUnitAmountDecimal: the nominal configured rate on
// per_unit usage lines, else the effective amount÷quantity rate. So a
// sub-cent rate never collapses to "$0.00" (ADR-054). Both are computed on
// the fly.
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
