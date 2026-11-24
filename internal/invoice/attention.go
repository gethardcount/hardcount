package invoice

import (
	"fmt"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// attentionContext carries the facts the classifier needs that are not on
// the invoice row: about the customer, the tenant's and this server's setup,
// the subscription, the dunning run and the invoice's payments. The invoice
// service fills it when it reads an invoice.
type attentionContext struct {
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
	//
	// The no-payment-method notices read it too (ADR-060 amendment
	// 2026-10-30): without Stripe no card can be added online, so they say
	// the invoice is emailed instead of a setup link. Unlike the other
	// fields, its zero value (false) is not the old behaviour: it selects
	// the no-Stripe copy. The invoice service reads it for every page; a
	// read error leaves it false (logged), display only.
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
	// The pause does not stop a dunning run already open on the invoice
	// (Stripe keeps retrying invoices created before a pause too), so both
	// notices read DunningRetrying alongside it.
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

	// HostedPayOff is true when this server has no HOSTED_INVOICE_BASE_URL: the
	// hosted invoice page offers no Pay button (Stripe would have no page to
	// send the customer back to), and invoice emails carry no payment link. The
	// payment-failure notices then stop sending the operator to that page.
	// Zero value (false) means the page takes payment.
	HostedPayOff bool

	// SetupLinkCopyOff is true when this server has no CUSTOMER_PORTAL_URL:
	// the customer page cannot make a setup link to copy, because the setup
	// session is refused (return_address_not_set). The no-payment-method
	// notice then stops offering that. Zero value (false) means it works.
	SetupLinkCopyOff bool

	// InvoiceEmailOff is true when this server cannot send email at all (SMTP
	// is not set up). Read only by the no-payment-method notices while Stripe
	// is not connected (StripeConnected false): the collector then emails the
	// customer the invoice instead of a setup link (ADR-060 amendment
	// 2026-10-30), and refuses without SMTP. Zero value (false) means sending
	// works.
	InvoiceEmailOff bool

	// CommitGrantDrawableCents and CommitGrantConsumedCents describe the
	// prepaid-commit grant this invoice funded (ADR-078). Drawable is what the
	// customer can still spend right now; Consumed is what they already spent
	// and which no recovery can claw back. Both zero when the invoice funded no
	// commit. Two numbers, because they drive different sentences of the
	// dispute_lost notice (classifyDisputeLost).
	//
	// Drawable applies the SAME liveness predicate the drawdown itself uses
	// (drainPositiveBlocks: positive, not exhausted, not past expires_at,
	// evaluated at the request's instant) — so an expired-but-unswept grant
	// reports 0 and the notice cannot claim spendable credit the drain would
	// refuse to spend.
	//
	// Populated only for a paid top-up with a lost dispute (decorateInvoice).
	// Zero values name no prepaid credit, the safe default for every caller
	// that does not supply the signal and for a read that errored.
	CommitGrantDrawableCents int64
	CommitGrantConsumedCents int64

	// Now is wall-clock now, used only to age the in-flight payment banner
	// (processing → Info under the expected-settle window, Warning past it).
	// Staleness is a REAL-WORLD duration (the provider settles in wall-clock),
	// so this is wall-clock time. Zero value disables
	// the age escalation (the banner stays Info) — safe for callers that don't
	// set it.
	Now time.Time

	// PaymentPageOpen: the customer has a hosted payment page that may still
	// take their money, or a hosted payment Hardcount has not recorded yet
	// (HostedCheckoutLiveSQL). While it holds, every Hardcount charge refuses to
	// start (ADR-068 "one payer at a time"), so a banner offering Retry would
	// offer a button that is refused. The invoice service reads it for
	// finalized, unpaid invoices only.
	PaymentPageOpen bool

	// RetryBlockingDeclineCode is set when dunning retries skip the
	// customer's current default card on this invoice: Stripe's decline code,
	// recorded on the invoice's charge attempts with that card. Either the
	// bank refused the card for good (IsHardDecline), so the notice says the
	// customer needs a different card, or the bank asked the customer to
	// confirm the payment (IsAuthenticationRequired), so the notice offers
	// the payment link. Neither offers Retry. Empty once another card is the
	// default — a card the customer saves now becomes the default by itself
	// (ADR-126) — and the ordinary decline notice comes back. The invoice
	// service reads it for failed invoices only.
	RetryBlockingDeclineCode string

	// DunningRetrying is true when a charge retry on the invoice's dunning
	// run is still to come (InvoiceDunningRun.RetryScheduled: active, not
	// paused, attempts left under its policy). Once every attempt is used
	// (the final action waiting for the invoice grace, re-attempted after it
	// failed, or the run escalated), while it is paused, or with no run at
	// all (dunning off), no retry runs whatever card becomes the default. Read by the refused-card
	// notice ("dunning keeps its schedule" is true only then) and by the two
	// paused-collection notices: a collection pause does not stop an open
	// run, whose next retry charges the customer's default card. The invoice
	// service reads it for refused-card invoices and for every invoice it
	// reads the pause state for. Zero value (false) gives the no-retry copy,
	// which still sends the operator to get a new card rather than to wait
	// on a retry.
	DunningRetrying bool

	// LostDispute is a dispute on one of this invoice's payments that closed
	// as lost (#979), the newest one when there are several; nil when none
	// did. The invoice service reads it for every non-draft invoice, from the
	// invoice's dispute records. A read error leaves it nil: the notice is
	// display-only, and the dispute stays visible on the invoice's timeline
	// and in GET /v1/invoices/{id}/disputes.
	LostDispute *domain.InvoiceDispute

	// StripeRefundReview is the newest refund made in Stripe outside
	// Hardcount on one of this invoice's payments that Hardcount flagged
	// instead of recording (outcome needs_review, ADR-134); nil when none
	// waits. The invoice service reads it for every non-draft invoice. A read
	// error leaves it nil: the notice is display-only, and the refund stays
	// on the invoice's timeline and in GET /v1/invoices/{id}/stripe-refunds.
	StripeRefundReview *domain.InvoiceStripeRefund
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

// classifyInvoiceAttention returns the one notice an invoice shows its
// operator, or nil when it needs none: the CAUSE of the invoice's trouble,
// one reason, highest urgency first. Its first matching branch wins, in the
// order written here.
//
// It reads only its arguments and does no I/O.
func classifyInvoiceAttention(inv domain.Invoice, atc attentionContext) *domain.Attention {
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
	//
	// A lost dispute on the same invoice changes what the anomaly asks for:
	// the bank already gave one payment back, so "refund the extra charge"
	// could return the customer's money twice (#1287 review).
	if inv.PaymentAnomalyKind != "" {
		att := classifyPaymentAnomaly(inv)
		if atc.LostDispute != nil {
			att = withLostDispute(att, *atc.LostDispute)
		}
		if atc.StripeRefundReview != nil {
			att = withRefundedInStripe(att, inv, *atc.StripeRefundReview)
		}
		return att
	}
	// A lost dispute pierces it too: disputes land on paid invoices, so the
	// early return would hide every one of them.
	if atc.LostDispute != nil {
		return classifyDisputeLost(inv, *atc.LostDispute, atc)
	}
	// A refund made in Stripe that Hardcount did not record pierces it as
	// well: refunds land on paid invoices.
	if atc.StripeRefundReview != nil {
		return classifyRefundedInStripe(inv, *atc.StripeRefundReview)
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
	if inv.Status == domain.InvoiceUncollectible && inv.PaymentStatus == domain.PaymentProcessing {
		return classifyRecoveryInFlight(inv)
	}
	if inv.Status == domain.InvoicePaid || inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible {
		return nil
	}
	switch {
	// Before the failure and pending states: while the customer's payment page
	// is open no Hardcount charge can start, so their banners' Retry / Charge now
	// would be refused, and a dunning retry date reads as overdue when it is
	// held.
	case inv.Status == domain.InvoiceFinalized && atc.PaymentPageOpen:
		return classifyPaymentPageOpen()
	case inv.PaymentStatus == domain.PaymentFailed:
		return classifyPaymentFailure(inv, atc)
	case inv.TaxStatus == domain.InvoiceTaxPending:
		return classifyTaxAttention(inv, atc)
	case inv.PaymentStatus == domain.PaymentUnknown:
		return classifyPaymentUnconfirmed(inv)
	case inv.PaymentStatus == domain.PaymentProcessing:
		return classifyPaymentProcessing(inv, atc)
	// Before every pending state: Stripe refused this amount, so the
	// no-card, scheduled and awaiting notices would each promise a charge
	// that cannot happen.
	case inv.BelowStripeMinimum():
		return classifyAmountBelowMinimum(inv)
	// no_payment_method beats payment_scheduled: when both flags are
	// set (engine queued for retry but PM is still missing), the
	// scheduler will keep skipping until a PM is attached. The
	// operator-facing reality is "no PM" — that's the actionable
	// reason. Surfacing payment_scheduled instead would falsely tell
	// the operator "engine will retry on its next tick" when the
	// retry will skip again until a PM is attached.
	case inv.Status == domain.InvoiceFinalized && inv.PaymentStatus == domain.PaymentPending && !atc.HasPaymentMethod:
		return classifyNoPaymentMethod(inv, atc)
	// collection_paused beats payment_scheduled for the same reason
	// no_payment_method does: the flag says the invoice is queued, but the
	// sweeps skip paused subscriptions, so "the engine will charge this"
	// would be a promise nothing keeps.
	case inv.Status == domain.InvoiceFinalized && inv.PaymentStatus == domain.PaymentPending &&
		inv.AutoChargePending && atc.CollectionPaused:
		return classifyCollectionPaused(inv, atc.DunningRetrying)
	case inv.Status == domain.InvoiceFinalized && inv.PaymentStatus == domain.PaymentPending && inv.AutoChargePending:
		return classifyPaymentScheduled(inv)
	case inv.Status == domain.InvoiceFinalized && inv.PaymentStatus == domain.PaymentPending:
		return classifyAwaitingPayment(inv)
	}
	return nil
}

// classifyAmountBelowMinimum is the notice for an invoice Stripe refused to
// charge because its amount is below Stripe's minimum (ErrBelowMinimumCharge).
// Warning, not Critical: no money was lost and nothing is failing, but no
// automatic step will ever collect it, so it waits on the operator. Void is
// the one button; Record offline payment is already in the page's menu, and
// Collect payment stays on the page: it applies the customer's credit
// balance first and pays the invoice when that covers it, and otherwise asks
// Stripe again (its answer can change with the account's settlement
// currency; it answers at once and charges nothing if not). Stripe's sentence,
// which names the minimum, goes in ProviderResponse; the detail names no
// figure, because the minimum depends on the account's settlement currency.
func classifyAmountBelowMinimum(inv domain.Invoice) *domain.Attention {
	since := *inv.BelowMinimumAt
	return &domain.Attention{
		Severity: domain.AttentionSeverityWarning,
		Reason:   domain.AttentionReasonAmountBelowMinimum,
		Code:     "payment.amount_too_small",
		Message:  "This amount is below Stripe's minimum charge, so Hardcount won't charge it; void the invoice or record a payment made another way.",
		Detail: "Stripe won't charge this amount because it is below Stripe's minimum, which depends on the currency your Stripe account settles in; " +
			"Stripe's reply under Provider response names it. Nothing was charged. " +
			"Hardcount doesn't retry it on its own, ends any payment recovery on it, and doesn't email the customer about it. " +
			"Collect payment applies the customer's credit balance and pays the invoice if that covers it; otherwise it asks Stripe again. " +
			"To collect it later, add the amount to the customer's next invoice by hand, then void this one. Record offline payment is in the ⋯ menu.",
		DocURL:           docURL("amount-below-minimum"),
		Actions:          []domain.AttentionActionItem{{Code: domain.AttentionActionVoidInvoice, Label: "Void invoice"}},
		ProviderResponse: inv.LastPaymentError,
		Since:            &since,
		DueBy:            inv.DueAt,
	}
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
func classifyRecoveryInFlight(inv domain.Invoice) *domain.Attention {
	since := inv.UpdatedAt
	return &domain.Attention{
		Severity: domain.AttentionSeverityInfo,
		Reason:   domain.AttentionReasonPaymentProcessing,
		Code:     "payment.recovery_processing",
		Message:  "Stripe is still confirming a payment on this written-off invoice, so don't charge it again.",
		Detail:   "The invoice stays written off until that payment succeeds or fails.",
		DocURL:   docURL("payment-processing"),
		Since:    &since,
	}
}

// classifyTaxAttention branches on the typed tax_error_code persisted
// by internal/tax.Classify. Sub-cases differ in three dimensions:
// Reason (location-vs-generic), action verbs, and `param` pointer. The
// open `code` carries the full granularity (one per error_code value).
//
// Provenance routing per typed code (ADR-025):
//   - provider_not_configured: pre-flight failure, we never called the
//     provider. tax_pending_reason holds Hardcount's own string ("no client
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
//     unclassified content. If it turns out to be Hardcount-internal, we'd
//     just be mis-labelling it as a provider response, which is the
//     same as today's behaviour for known cases.
func classifyTaxAttention(inv domain.Invoice, atc attentionContext) *domain.Attention {
	errorCode := inv.TaxErrorCode
	if errorCode == "" {
		errorCode = "unknown"
	}

	att := &domain.Attention{
		Severity: domain.AttentionSeverityWarning,
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
	retryable := domain.TaxErrorCodeRetryable(errorCode)
	exhausted := retryable && inv.TaxRetryCount >= domain.MaxTaxRetryAttempts
	if exhausted || !retryable {
		att.Severity = domain.AttentionSeverityCritical
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
		att.Reason = domain.AttentionReasonTaxLocationRequired
		att.Message = "Tax can't be calculated because the customer's address or tax ID is missing or invalid, so fix it, then retry tax."
		att.Param = "customer.address.postal_code"
		att.DocURL = docURL("tax-location-required")
		att.Actions = []domain.AttentionActionItem{
			{Code: domain.AttentionActionEditBillingProfile, Label: "Edit billing profile"},
			{Code: domain.AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "jurisdiction_unsupported":
		att.Reason = domain.AttentionReasonTaxCalculationFailed
		att.Message = "The tax provider can't calculate tax for this customer's region, so register for tax there with the provider, then retry tax."
		att.DocURL = docURL("tax-jurisdiction-unsupported")
		att.Actions = []domain.AttentionActionItem{
			{Code: domain.AttentionActionReviewRegistration, Label: "Review tax registration"},
			{Code: domain.AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "provider_outage":
		att.Reason = domain.AttentionReasonTaxCalculationFailed
		if exhausted {
			att.Message = "The tax provider stayed unreachable through every automatic retry, so retry tax once it's back."
		} else {
			att.Message = fmt.Sprintf("The tax provider can't be reached right now, and Hardcount retries automatically (%d of %d attempts used).", inv.TaxRetryCount, domain.MaxTaxRetryAttempts)
		}
		att.DocURL = docURL("tax-provider-outage")
		att.Actions = []domain.AttentionActionItem{
			{Code: domain.AttentionActionWaitProvider, Label: "Check provider status"},
			{Code: domain.AttentionActionRetryTax, Label: "Retry now"},
		}
	case "provider_auth":
		att.Reason = domain.AttentionReasonTaxCalculationFailed
		att.Message = "The tax provider rejected the API key, so update the key in Settings, then retry tax."
		att.DocURL = docURL("tax-provider-auth")
		att.Actions = []domain.AttentionActionItem{
			{Code: domain.AttentionActionRotateAPIKey, Label: "Rotate API key"},
			{Code: domain.AttentionActionRetryTax, Label: "Retry tax"},
		}
	case "provider_not_configured":
		att.Reason = domain.AttentionReasonTaxCalculationFailed
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
			att.Severity = domain.AttentionSeverityInfo
			att.Message = "Tax calculation is queued now that Stripe is connected; use Retry now to run it right away."
			att.Actions = []domain.AttentionActionItem{
				{Code: domain.AttentionActionRetryTax, Label: "Retry now"},
			}
		} else if atc.StripeConnected {
			att.Message = "Automatic tax retries have stopped for this invoice, so use Retry now to calculate it again."
			att.Actions = []domain.AttentionActionItem{
				{Code: domain.AttentionActionRetryTax, Label: "Retry now"},
			}
		} else {
			att.Message = "Tax can't be calculated because Stripe isn't connected in this mode, so connect Stripe, then retry tax."
			att.Actions = []domain.AttentionActionItem{
				{Code: domain.AttentionActionConnectTaxProvider, Label: "Connect Stripe"},
				{Code: domain.AttentionActionRetryTax, Label: "Retry tax"},
			}
		}
	default:
		att.Reason = domain.AttentionReasonTaxCalculationFailed
		if exhausted {
			att.Message = "Tax calculation kept failing for an unknown reason and automatic retries have stopped, so retry tax to try again."
		} else {
			att.Message = fmt.Sprintf("Tax calculation failed for an unknown reason, and Hardcount retries automatically (%d of %d attempts used).", inv.TaxRetryCount, domain.MaxTaxRetryAttempts)
		}
		att.DocURL = docURL("tax-calculation-failed")
		att.Actions = []domain.AttentionActionItem{
			{Code: domain.AttentionActionRetryTax, Label: "Retry tax"},
		}
	}
	return att
}

func classifyPaymentFailure(inv domain.Invoice, atc attentionContext) *domain.Attention {
	// Operator voice, not provider voice: LastPaymentError is Stripe's
	// CARDHOLDER-facing sentence ("Your card was declined") — promoted
	// verbatim to the banner it read as Hardcount addressing the operator,
	// whose card was not declined. The banner states the fact in the
	// operator's frame; the provider's exact wording stays one
	// disclosure away (ProviderResponse below) and in Diagnostic
	// detail, where it is labeled as evidence.
	headline := "The customer's card was declined."
	if inv.LastPaymentError == "" {
		headline = "The customer's payment attempt failed."
	}
	since := inv.UpdatedAt
	switch code := atc.RetryBlockingDeclineCode; {
	case domain.IsHardDecline(code):
		return classifyRefusedCard(inv, code, atc.DunningRetrying, atc.HostedPayOff, since)
	case domain.IsAuthenticationRequired(code):
		return classifyNeedsConfirmation(inv, atc.DunningRetrying, atc.HostedPayOff, since)
	}
	// Update Payment Method is the primary action: the card on file
	// is broken, retrying with the same card will decline again.
	// Retry Payment is offered as secondary for transient declines
	// (network blip, temporary insufficient funds) where the
	// operator wants to re-attempt without changing the card.
	//
	// LastPaymentError is the Stripe last_payment_error message —
	// upstream payload from Stripe, not Hardcount's classification —
	// so it goes in ProviderResponse (ADR-025). The headline already
	// renders the truncated form; ProviderResponse holds the full
	// string for the disclosure.
	return &domain.Attention{
		Severity: domain.AttentionSeverityCritical,
		Reason:   domain.AttentionReasonPaymentFailed,
		Code:     "payment.declined",
		Message:  headline,
		DocURL:   docURL("payment-failed"),
		Actions: []domain.AttentionActionItem{
			{Code: domain.AttentionActionUpdatePaymentMethod, Label: "Update payment method"},
			{Code: domain.AttentionActionRetryPayment, Label: "Retry payment"},
		},
		ProviderResponse: inv.LastPaymentError,
		Since:            &since,
	}
}

// classifyRefusedCard is the payment-failure notice when the bank refused the
// customer's default card for good (#966): lost, stolen, revoked and the rest
// of IsHardDecline. Dunning retries skip that card until another card is the
// default, so the notice asks for a different card and drops Retry payment —
// a manual retry would charge the card the bank just refused, and the card
// networks charge merchants for retrying these declines.
//
// What the notice says about retries follows the dunning run: only an active,
// unpaused run has a retry to come. A card the customer saves now replaces
// the refused default by itself (ADR-126; a setup or update link's card
// always does, ADR-021); a card already on file stays beside it, so the
// operator is told to make that one the default. With hostedPayOff the hosted
// invoice page takes no payment, so the notice does not offer it.
func classifyRefusedCard(inv domain.Invoice, declineCode string, retrying, hostedPayOff bool, since time.Time) *domain.Attention {
	const final = "Stripe's decline code is %s, which Stripe and the card networks treat as final. "
	newCard := "A card the customer saves now, through a setup or update link or by paying on the hosted invoice page, becomes the default by itself; a card already on file needs Set as default on the customer page. "
	hostedPay := "The customer can also pay on the hosted invoice page."
	if hostedPayOff {
		newCard = "A card the customer saves now, through a setup or update link, becomes the default by itself; a card already on file needs Set as default on the customer page. "
		hostedPay = ""
	}
	message := "The customer's bank refused this card for good, so automatic retries won't charge it; the customer needs a different card."
	detail := fmt.Sprintf(final, declineCode) +
		"Dunning keeps its schedule and counts each retry; the next retry charges a different card once it is the customer's default. " +
		newCard + hostedPay
	if !retrying {
		message = "The customer's bank refused this card for good, and no automatic retry is scheduled; the customer needs a different card."
		detail = fmt.Sprintf(final, declineCode) + newCard +
			"Once another card is the default, Retry payment charges it. " + hostedPay
	}
	return &domain.Attention{
		Severity: domain.AttentionSeverityCritical,
		Reason:   domain.AttentionReasonPaymentFailed,
		Code:     "payment.card_refused",
		Message:  message,
		Detail:   strings.TrimSpace(detail),
		DocURL:   docURL("payment-failed"),
		Actions: []domain.AttentionActionItem{
			{Code: domain.AttentionActionUpdatePaymentMethod, Label: "Update payment method"},
		},
		ProviderResponse: inv.LastPaymentError,
		Since:            &since,
	}
}

// classifyNeedsConfirmation is the payment-failure notice when the bank asked
// the customer to confirm the last charge of their default card on this
// invoice (authentication_required, usually 3-D Secure). The card is not
// refused: the customer confirms the payment on the hosted invoice page,
// where Stripe runs 3-D Secure. So the one action is emailing them the
// payment link. Retry payment is not offered: it charges without the
// customer, and the bank asks again. Dunning retries skip the card, as
// Stripe Billing does for this code, and each reminder email carries the
// payment link.
//
// Warning, not Critical: nothing is broken, the customer has to act, as on
// the no-payment-method notice.
//
// With hostedPayOff the hosted invoice page takes no payment and the payment
// email carries no link, so the notice says to set the address first and
// offers no email action.
func classifyNeedsConfirmation(inv domain.Invoice, retrying, hostedPayOff bool, since time.Time) *domain.Attention {
	const (
		why     = "Stripe's code is authentication_required: the bank wants the customer to confirm this payment, usually with 3-D Secure, and a charge made without the customer can't do that. "
		newCard = "A different card the customer saves through a setup or update link becomes the default; "
	)
	howPay := "The customer pays on the hosted invoice page, where their bank may ask them to confirm. "
	message := "The customer's bank needs them to confirm this payment; email them the payment link to pay and confirm it."
	actions := []domain.AttentionActionItem{
		{Code: domain.AttentionActionSendReminder, Label: "Email payment link"},
	}
	if hostedPayOff {
		howPay = "The customer confirms on the hosted invoice page, but this server has no HOSTED_INVOICE_BASE_URL, so that page offers no Pay button and the payment email has no link. Set it to the page's address and restart, then email them the payment link. "
		message = "The customer's bank needs them to confirm this payment on the hosted invoice page, which this server can't offer until HOSTED_INVOICE_BASE_URL is set."
		actions = nil
	}
	detail := why + howPay +
		"Dunning keeps its schedule and counts each retry, but doesn't charge this card; each reminder email carries the payment link. " +
		newCard + "the next retry charges it."
	if !retrying {
		detail = why + howPay + "No automatic retry is scheduled. " +
			newCard + "Collect payment charges it."
	}
	return &domain.Attention{
		Severity:         domain.AttentionSeverityWarning,
		Reason:           domain.AttentionReasonPaymentFailed,
		Code:             "payment.authentication_required",
		Message:          message,
		Detail:           detail,
		DocURL:           docURL("payment-failed"),
		Actions:          actions,
		ProviderResponse: inv.LastPaymentError,
		Since:            &since,
	}
}

func classifyPaymentUnconfirmed(inv domain.Invoice) *domain.Attention {
	since := inv.UpdatedAt
	// No operator action: the reconciler re-queries the provider on the next
	// tick and now settles the outcome COMPLETELY — mark + dunning + email +
	// event, identical to the webhook (ADR-049 Phase 2). The prior disabled
	// "Check provider" button was non-functional (no endpoint); an on-demand
	// re-check is deferred until real stuck-payment pressure (ADR-049), so the
	// dead button is removed rather than shipped greyed-out.
	return &domain.Attention{
		Severity: domain.AttentionSeverityInfo,
		Reason:   domain.AttentionReasonPaymentUnconfirmed,
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
		// error and settles the invoice FAILED (classifyNeedsConfirmation),
		// never reaching this banner. A live requires_action PaymentIntent comes from
		// the hosted checkout, where the customer is present and mid-3DS, and
		// that one expires with its session inside the hour.
		Message: "Stripe hasn't confirmed this charge yet, and Hardcount re-checks automatically until it does.",
		Detail: "If it stays here for hours, the charge may be waiting on the customer (for example 3-D Secure). " +
			"Check the payment in Stripe, then cancel or retry it there.",
		DocURL: docURL("payment-unconfirmed"),
		Since:  &since,
	}
}

// classifyPaymentProcessing surfaces the in-flight charge state. Hardcount now
// confirms it automatically — inline from the synchronous charge response
// (ADR-049 Phase 3) or via the reconciler backstop if the provider callback is
// delayed/dropped (Phase 2) — so the healthy state needs no operator action.
//
// Age-aware: an invoice still `processing` past processingStaleAfter is
// anomalous for a card (it should have settled inline or via the reconciler),
// so escalate Info → Warning and point the operator at the provider — without
// falsely promising auto-resolution for the stuck case. Zero atc.Now keeps it
// Info (callers that don't set it).
func classifyPaymentProcessing(inv domain.Invoice, atc attentionContext) *domain.Attention {
	since := inv.UpdatedAt

	stale := !atc.Now.IsZero() && atc.Now.Sub(inv.UpdatedAt) > processingStaleAfter
	if stale {
		return &domain.Attention{
			Severity: domain.AttentionSeverityWarning,
			Reason:   domain.AttentionReasonPaymentProcessing,
			Code:     "payment.processing",
			Message:  fmt.Sprintf("Stripe still hasn't confirmed this payment after %d hours, so check it in Stripe.", int(processingStaleAfter.Hours())),
			Detail:   "Card payments are usually confirmed within seconds, so a payment this old needs a look.",
			DocURL:   docURL("payment-processing"),
			Since:    &since,
		}
	}

	return &domain.Attention{
		Severity: domain.AttentionSeverityInfo,
		Reason:   domain.AttentionReasonPaymentProcessing,
		Code:     "payment.processing",
		Message:  "Stripe is confirming this payment, and this page updates automatically when it's done.",
		DocURL:   docURL("payment-processing"),
		Since:    &since,
	}
}

// classifyPaymentPageOpen: the customer is paying on the hosted payment page,
// or has just paid there and Hardcount is recording it. Nothing is wrong and the
// operator has nothing to do: Hardcount holds its own charges and dunning retries
// until the page closes (ADR-068), so no actions are offered — Collect and
// Retry would be refused. Reuses payment_processing (the closed reason enum is
// a public contract): a payment may be in progress, and the Code carries the
// granularity.
func classifyPaymentPageOpen() *domain.Attention {
	return &domain.Attention{
		Severity: domain.AttentionSeverityInfo,
		Reason:   domain.AttentionReasonPaymentProcessing,
		Code:     "payment.page_open",
		Message:  "The customer has the payment page open or just paid on it, so Hardcount holds charges until that finishes, usually within an hour.",
		DocURL:   docURL("payment-processing"),
	}
}

// classifyPaymentScheduled surfaces the scheduler-will-retry state.
// Auto-charge is queued; the engine will fire on its next sweep. The
// operator can short-circuit with "Charge now" if they don't want to
// wait the scheduler interval.
func classifyPaymentScheduled(inv domain.Invoice) *domain.Attention {
	since := inv.UpdatedAt
	// payment_scheduled fires when auto_charge_pending=true and the
	// invoice is still unpaid: the finalize-time collection didn't finish
	// (a transient failure, or the process stopped), and the auto-charge
	// sweep will pick it up on its next run. We don't surface a precise
	// next_attempt_at; "usually within an hour" is honest because the
	// sweep runs on the billing tick, hourly by default (every 5 minutes in
	// local dev, cmd/hardcount/main.go).
	return &domain.Attention{
		Severity: domain.AttentionSeverityInfo,
		Reason:   domain.AttentionReasonPaymentScheduled,
		Code:     "payment.scheduled",
		Message:  "The card on file is charged automatically, usually within an hour; use Charge now to collect it right away.",
		Detail:   "Automatic charges run on a schedule, hourly by default.",
		DocURL:   docURL("payment-scheduled"),
		Actions: []domain.AttentionActionItem{
			{Code: domain.AttentionActionChargeNow, Label: "Charge now"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

// classifyCollectionPaused surfaces a queued invoice whose subscription is
// paused for collection. The queue flag is still set — it is what will make
// the sweep pick the invoice up the moment collection resumes — but the
// sweeps skip it in the meantime, so this must not borrow
// payment_scheduled's "the engine will charge this" copy.
//
// The pause does not stop a dunning run already open on the invoice: its
// retries keep their schedule and charge the card on file, as Stripe keeps
// retrying invoices created before a pause unless they are voided. So with
// retrying set the notice says the retries continue and how to stop them,
// and only without it does it say nothing charges the invoice.
//
// Severity info, not warning: the operator asked for this. Charge now stays
// offered because a manual collect is an explicit override and has no pause
// filter — the pause governs automation, not the operator.
func classifyCollectionPaused(inv domain.Invoice, retrying bool) *domain.Attention {
	since := inv.UpdatedAt
	message := "Collection is paused on the subscription, so this invoice won't be charged automatically; use Charge now to collect it anyway."
	detail := "Resume collection on the subscription page to go back to automatic charges. This invoice is then charged on the next run."
	if retrying {
		message = "Collection is paused, but payment retries for this invoice continue, so the next retry charges the card on file."
		detail = pausedRetriesDetail
	}
	return &domain.Attention{
		Severity: domain.AttentionSeverityInfo,
		Reason:   domain.AttentionReasonCollectionPaused,
		Code:     "payment.collection_paused",
		Message:  message,
		Detail:   detail,
		DocURL:   docURL("collection-paused"),
		Actions: []domain.AttentionActionItem{
			{Code: domain.AttentionActionChargeNow, Label: "Charge now"},
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
// This is NOT a steady state Hardcount has a collection mode for — every
// finalize write queues the invoice (auto_charge_pending, ADR-116),
// which routes to payment_scheduled instead. Reaching here means a row
// written before that change. It is operator-recoverable via Charge now,
// which is why the actions matter more than the copy here.
func classifyAwaitingPayment(inv domain.Invoice) *domain.Attention {
	since := inv.UpdatedAt
	return &domain.Attention{
		Severity: domain.AttentionSeverityInfo,
		Reason:   domain.AttentionReasonAwaitingPayment,
		Code:     "payment.awaiting",
		Message:  "This invoice hasn't been charged yet; use Charge now, or email the customer a payment link.",
		DocURL:   docURL("awaiting-payment"),
		Actions: []domain.AttentionActionItem{
			{Code: domain.AttentionActionChargeNow, Label: "Charge now"},
			{Code: domain.AttentionActionSendReminder, Label: "Email payment link"},
		},
		Since: &since,
		DueBy: inv.DueAt,
	}
}

// pausedRetriesDetail explains a collection pause beside an open dunning run:
// the pause keeps new invoices as drafts, and this invoice's retries keep
// going until it is paid, voided or written off. Both actions sit in the
// invoice page's ⋯ menu, above the banner that shows this detail.
const pausedRetriesDetail = "Pausing collection keeps new invoices as drafts. Payment retries already under way for this invoice keep their schedule. " +
	"To stop the retries, void this invoice or mark it uncollectible from the ⋯ menu above."

// classifyNoPaymentMethod surfaces the finalized-unpaid invoice whose
// customer has no PaymentSetup ready. The invoice IS queued
// (auto_charge_pending) and dunning ages it out — but automation cannot
// conjure the card, so the operator/customer is the mover.
//
// Message line 1 says what happens to THIS invoice when a card arrives,
// which depends on two real signals: collection paused (the sweep that
// charges on attach skips paused subscriptions, so nothing happens until
// resume) and an open dunning run (which the pause does not stop, so its next
// retry charges the new card). Line 2 says
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
func classifyNoPaymentMethod(inv domain.Invoice, atc attentionContext) *domain.Attention {
	since := inv.UpdatedAt
	if !atc.StripeConnected {
		return classifyNoPaymentMethodNoStripe(inv, atc, since)
	}
	// Resend can only send to an address, from a server that can send.
	canEmail := atc.CustomerHasEmail && !atc.SetupLinkEmailOff

	// What adding a card does. The charge-on-attach promise is the server's
	// own (post-ADR-013 the scheduler picks the invoice up the moment a
	// PaymentSetup flips to ready, Chargebee's "Collect Invoice on Card
	// Update"), and it lives HERE, server-owned, so the client can't drift a
	// duplicate copy of it (it used to hardcode one).
	charge := "This invoice is charged automatically once the customer adds a card."
	// A pause stops the charge on attach, not a dunning run already open on
	// the invoice: its next retry charges the card the customer adds.
	pausedRetrying := atc.CollectionPaused && atc.DunningRetrying
	switch {
	case pausedRetrying:
		charge = "Collection is paused, but payment retries for this invoice continue, so the next retry charges a card the customer adds."
	case atc.CollectionPaused:
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
	//
	// With SetupLinkCopyOff the customer page can't make a link to copy
	// (no CUSTOMER_PORTAL_URL), so the notice doesn't offer that.
	const copyFix = " To copy a setup link from the customer page, set CUSTOMER_PORTAL_URL to your dashboard's address."
	var emailLine, emailDetail string
	switch {
	case !atc.CustomerHasEmail && atc.SetupLinkCopyOff:
		emailLine = "Setup links are emailed only to customers with an email address, and this server can't make one to copy."
		emailDetail = "Adding an email address on the customer page lets Hardcount send the link." + copyFix
	case !atc.CustomerHasEmail:
		// False also when the customer read failed, so the sentence states
		// Hardcount's rule rather than this customer's state: honest either way,
		// and the customer page shows the real address.
		emailLine = "Setup links are emailed only to customers with an email address, so copy one from the customer page and send it."
		emailDetail = "Adding an email address on the customer page lets Hardcount send the link."
	case atc.SetupLinkEmailOff && atc.SetupLinkCopyOff:
		emailLine = "This server can't email setup links or make one to copy, so none was sent."
		emailDetail = "To let Hardcount email setup links, set up email sending (SMTP) and the payment-update page addresses (PAYMENT_UPDATE_URL and PAYMENT_UPDATE_RETURN_URL)." + copyFix
	case atc.SetupLinkEmailOff:
		emailLine = "This server can't email setup links, so none was sent; copy one from the customer page instead."
		emailDetail = "To let Hardcount email setup links, set up email sending (SMTP) and the payment-update page addresses (PAYMENT_UPDATE_URL and PAYMENT_UPDATE_RETURN_URL)."
	case atc.SetupLinkCopyOff:
		emailLine = "A setup link is emailed to the customer once; use Resend setup link to send it again."
		emailDetail = "The invoice's activity shows whether the email was sent." + copyFix
	default:
		emailLine = "A setup link is emailed to the customer once; use Resend setup link to send it again."
		emailDetail = "The invoice's activity shows whether the email was sent. The customer page can also copy the link to share directly."
	}

	actions := []domain.AttentionActionItem{
		{Code: domain.AttentionActionAddPaymentMethod, Label: "Open customer page"},
	}
	if canEmail {
		actions = append([]domain.AttentionActionItem{
			{Code: domain.AttentionActionSendReminder, Label: "Resend setup link"},
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
		return &domain.Attention{
			Severity: domain.AttentionSeverityWarning,
			Reason:   domain.AttentionReasonDunningExhausted,
			Code:     "payment.dunning_exhausted",
			Message:  msg,
			Detail:   detail,
			DocURL:   docURL("dunning-exhausted"),
			Actions:  actions,
			Since:    &since,
			DueBy:    inv.DueAt,
		}
	}

	noCardDetail := emailDetail
	if pausedRetrying {
		noCardDetail = joinDetail(pausedRetriesDetail, emailDetail)
	}
	return &domain.Attention{
		Severity: domain.AttentionSeverityWarning,
		Reason:   domain.AttentionReasonNoPaymentMethod,
		Code:     "payment.no_payment_method",
		Message:  charge + "\n" + emailLine,
		Detail:   noCardDetail,
		DocURL:   docURL("no-payment-method"),
		Actions:  actions,
		Since:    &since,
		DueBy:    inv.DueAt,
	}
}

// classifyNoPaymentMethodNoStripe is the no-payment-method notice while
// Stripe is not connected for the invoice's mode (ADR-060 amendment
// 2026-10-30). No card can be added online, so the collector emails the
// customer the invoice once instead of a setup link, and payment recovery
// reminds them to pay it. There is no setup link to resend; the customer
// page stays the operator's lever. Line 2 says what happened to the invoice
// email, from the same two facts as the setup-link copy: whether the customer
// has an email address, and whether this server can send email.
//
// StripeConnected is false also when its read failed (logged); the notice
// then says Stripe isn't connected, which is display only: the collector
// itself reads the connection again and refuses to act on an error.
func classifyNoPaymentMethodNoStripe(inv domain.Invoice, atc attentionContext, since time.Time) *domain.Attention {
	const noStripe = "Stripe isn't connected in this mode, so no card can be added online."
	var emailLine string
	switch {
	case !atc.CustomerHasEmail:
		emailLine = "The invoice is emailed only to customers with an email address; add one on the customer page."
	case atc.InvoiceEmailOff:
		emailLine = "This server can't send email, so the invoice was not emailed to the customer."
	default:
		emailLine = "The invoice is emailed to the customer once."
	}
	const detail = "Payment recovery reminds the customer to pay the invoice. To collect by card, connect Stripe under Settings. " +
		"When the customer pays another way, record the payment on this invoice. The invoice's activity shows whether the email was sent."
	actions := []domain.AttentionActionItem{{Code: domain.AttentionActionAddPaymentMethod, Label: "Open customer page"}}

	if atc.DunningEscalated {
		msg := "Retries stopped; " + noStripe
		if atc.CollectionPaused {
			msg = "Retries stopped and collection is paused; " + noStripe
		}
		const ended = "Automatic payment recovery ended without collecting, and the recovery policy's final step ran. " +
			"Check the subscription to see what that step did."
		return &domain.Attention{
			Severity: domain.AttentionSeverityWarning,
			Reason:   domain.AttentionReasonDunningExhausted,
			Code:     "payment.dunning_exhausted",
			Message:  msg + "\n" + emailLine,
			Detail:   joinDetail(ended, detail),
			DocURL:   docURL("dunning-exhausted"),
			Actions:  actions,
			Since:    &since,
			DueBy:    inv.DueAt,
		}
	}
	return &domain.Attention{
		Severity: domain.AttentionSeverityWarning,
		Reason:   domain.AttentionReasonNoPaymentMethod,
		Code:     "payment.no_payment_method",
		Message:  noStripe + "\n" + emailLine,
		Detail:   detail,
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
func classifyPaymentAnomaly(inv domain.Invoice) *domain.Attention {
	pi := inv.PaymentAnomalyPaymentIntentID
	var msg, detail string
	switch inv.PaymentAnomalyKind {
	case domain.EventPaymentDuplicateCharge:
		msg = "A second payment succeeded after this invoice was already paid, so refund the extra charge in Stripe."
		detail = "Extra payment: " + pi + "."
		if inv.StripePaymentIntentID != "" {
			detail += " Original payment: " + inv.StripePaymentIntentID + "."
		} else if inv.PaidOutOfBand {
			detail += " Original payment: recorded as paid outside Hardcount."
		}
		detail += " The extra charge exists only in Stripe; Hardcount records one payment."
	case domain.EventPaymentAmountMismatch:
		// Formatted, not raw minor units: this sentence used to read "Stripe
		// captured 9652 (minor units) … booked 10000", handing an operator two
		// unlabelled integers and a unit they do not think in, in the one banner
		// whose entire job is comparing two amounts.
		msg = fmt.Sprintf("Stripe captured %s but this invoice recorded %s as paid, so reconcile the difference.",
			domain.FormatMoneyMinor(inv.PaymentAnomalyCapturedCents, inv.Currency), domain.FormatMoneyMinor(inv.AmountPaidCents, inv.Currency))
		detail = "Payment: " + pi + ". The recorded paid amount sets how much can be refunded."
	case domain.EventPaymentReceivedOnVoidedInvoice:
		msg = "A payment succeeded on this voided invoice, so the customer paid money they don't owe; refund it in Stripe."
		detail = "Payment: " + pi + "."
	case domain.EventPaymentCurrencyMismatch:
		msg = "A payment was charged in a different currency than this invoice, so review it in Stripe and refund or correct it."
		detail = "Payment: " + pi + ". The amounts were not converted."
	default:
		msg = "Hardcount found a payment on this invoice that doesn't match its records, so review it in Stripe."
		detail = "Kind: " + inv.PaymentAnomalyKind + ". Payment: " + pi + "."
	}
	att := &domain.Attention{
		Reason:   domain.AttentionReasonPaymentAnomaly,
		Severity: domain.AttentionSeverityCritical,
		Code:     inv.PaymentAnomalyKind,
		Message:  msg,
		Detail:   detail,
	}
	// The refund or correction is made in Stripe, so the action opens the
	// payment there. Without an id there is nothing to open.
	if pi != "" {
		att.Actions = []domain.AttentionActionItem{{Code: domain.AttentionActionReconcilePayment, Label: "Open in Stripe"}}
	}
	return att
}

// classifyDisputeLost reports a payment the customer's bank took back after a
// lost dispute (#979). Hardcount records the loss and changes nothing else, so the
// invoice still reads paid and its books overstate what was collected until
// the operator acts. No action button: the decision is the operator's (a
// credit note, or a write-off in their own books), and none of the existing
// actions makes it. The Stripe dispute id goes in Detail, where the Stripe
// dashboard search needs it.
//
// A paid invoice that bought prepaid credit is different in two ways: the
// credit stays spendable after the bank took the money back, and an ordinary
// credit note is refused there (ADR-078 D4). Commit relief is the one action
// that retires the credit (ADR-080), so the notice names it, with the
// spendable amount up front. The service reads the grant for exactly this
// case (decorateInvoice). Relief is for paid invoices only, so the branch is
// too.
func classifyDisputeLost(inv domain.Invoice, d domain.InvoiceDispute, atc attentionContext) *domain.Attention {
	since := inv.UpdatedAt
	if d.ClosedAt != nil {
		since = *d.ClosedAt
	}
	taken := domain.FormatMoneyMinor(d.AmountCents, d.Currency)
	detail := "Stripe dispute " + d.StripeDisputeID + " closed as lost"
	if d.Reason != "" {
		detail += "; the customer's reason was " + d.Reason
	}
	detail += ". Hardcount leaves the invoice paid and moves no money."
	message := "The customer's bank took back " + taken +
		" after a lost dispute, so record it with a credit note or write it off in your books."

	drawable, spent := atc.CommitGrantDrawableCents, atc.CommitGrantConsumedCents
	switch {
	case inv.Status == domain.InvoicePaid && drawable > 0:
		message = "The customer's bank took back " + taken + " after a lost dispute, and the customer can still spend " +
			domain.FormatMoneyMinor(drawable, inv.Currency) + " of this invoice's prepaid credit." +
			"\nRetire that credit with commit relief, recorded as money returned outside Hardcount."
		detail += " The prepaid credit stays spendable until you retire it. An ordinary credit note is refused on an invoice that bought prepaid credit: " +
			"use POST /v1/credit-notes with commit_relief {\"retire_all\": true, \"out_of_band_amount_cents\": <amount>}, the amount inside commit_relief and never as a card refund, because the bank already returned the money. " +
			"If the amount you send differs, the answer names the right one. " +
			"Relief returns at most the price paid for the unspent credit; record any rest of the loss in your own books."
	case inv.Status == domain.InvoicePaid && spent > 0:
		message = "The customer's bank took back " + taken +
			" after a lost dispute, so bill the customer again or write it off in your books."
		detail += " The customer already spent all the credit this invoice bought, so there is none to retire, and an ordinary credit note is refused on an invoice that bought prepaid credit."
	default:
		detail += " To record the loss, issue a credit note with the amount as money returned outside Hardcount, not as a card refund: the bank already returned it."
	}
	return &domain.Attention{
		Severity: domain.AttentionSeverityCritical,
		Reason:   domain.AttentionReasonDisputeLost,
		Code:     "payment.dispute_lost",
		Message:  message,
		Detail:   detail,
		DocURL:   docURL("dispute-lost"),
		Since:    &since,
	}
}

// classifyRefundedInStripe reports a refund of the invoice's payment made in
// Stripe outside Hardcount that Hardcount flagged instead of recording
// (ADR-134). Warning: the money already moved, the invoice is not wrong in a
// way that loses more, but its documents and Stripe Tax say nothing was
// refunded until the operator acts. No action button yet: recording it is a
// credit note linked to the refund (POST /v1/credit-notes with
// stripe_refund_id), and dismissing it is POST
// /v1/invoices/{id}/stripe-refunds/{stripe_refund_id}/dismiss; Detail names
// both.
func classifyRefundedInStripe(inv domain.Invoice, r domain.InvoiceStripeRefund) *domain.Attention {
	since := r.RefundCreatedAt
	amount := domain.FormatMoneyMinor(r.AmountCents, r.Currency)
	why := ""
	record := "Record it with a credit note linked to the refund, or dismiss it if you handled it outside Hardcount."
	switch r.ReviewReason {
	case domain.StripeRefundReviewInvoiceNotPaid:
		why = "the invoice was not paid in Hardcount when Stripe reported the refund"
		record = "Check the invoice's payments in Stripe, then record the refund with a credit note once the invoice is paid, or dismiss it if you handled it outside Hardcount."
	case domain.StripeRefundReviewCurrencyMismatch:
		why = "the refund is in " + r.Currency + " and the invoice in " + inv.Currency
		record = "Record it in your own books and dismiss it here."
	case domain.StripeRefundReviewPrepaidCommit:
		why = "the invoice bought prepaid credit, which the customer can still spend"
		record = "Retire that credit with commit relief, recorded as money returned outside Hardcount (the refund already returned it), then dismiss the refund here."
	case domain.StripeRefundReviewSeenLate:
		why = "Hardcount first heard of it more than a day after it was made, so it may already be recorded"
		record = "If no credit note records it yet, record it with a credit note linked to the refund; otherwise dismiss it."
	case domain.StripeRefundReviewCreditNoteRefundFailed:
		why = "a credit note on this invoice has a card refund that failed, which this refund may replace"
		record = "If it replaces that refund, attach it to that credit note; otherwise record it with a credit note linked to the refund, or dismiss it. Retry refund on that credit note is refused until then."
	case domain.StripeRefundReviewExceedsCardRefundable:
		why = "it is more than the card payment had left to refund after the invoice's credit notes"
		record = "Check the payment in Stripe and record the difference in your own books, then dismiss it here."
	case domain.StripeRefundReviewExceedsCreditable:
		why = "the customer got back more than the invoice had left to credit, after a credit note that already gave part of it back another way"
		record = "If that credit note's credit was not meant as well, take it back from the customer's balance; then dismiss the refund here."
	default:
		why = "Hardcount could not match it to the invoice"
	}
	detail := "Stripe refund " + r.StripeRefundID + " on payment " + r.StripePaymentIntentID + ". " +
		"To record it: POST /v1/credit-notes with stripe_refund_id and refund_amount_cents set to the refund's amount; Hardcount calls nothing at Stripe for it. " +
		"To dismiss it: POST /v1/invoices/" + inv.ID + "/stripe-refunds/" + r.StripeRefundID + "/dismiss with a note."
	if r.ReviewReason == domain.StripeRefundReviewCreditNoteRefundFailed {
		detail += " To attach it to the credit note whose refund failed: POST /v1/credit-notes/{id}/attach-stripe-refund with stripe_refund_id; the amounts must match."
	}
	return &domain.Attention{
		Severity: domain.AttentionSeverityWarning,
		Reason:   domain.AttentionReasonRefundedInStripe,
		Code:     "payment.refunded_in_stripe." + string(r.ReviewReason),
		Message:  "A refund of " + amount + " was made in Stripe and not recorded here: " + why + ". " + record,
		Detail:   detail,
		DocURL:   docURL("refunded-in-stripe"),
		Since:    &since,
	}
}

// withRefundedInStripe adds a flagged refund made in Stripe (ADR-134) to a
// payment-anomaly notice, which wins the priority chain: an anomaly invoice
// is where a refund that does not fit the invoice is most likely (a refund
// of an amount-mismatch charge). The anomaly keeps its reason, code and
// "Open in Stripe" button; the message names the refund first, and Detail
// carries both, the refund's first.
func withRefundedInStripe(att *domain.Attention, inv domain.Invoice, r domain.InvoiceStripeRefund) *domain.Attention {
	if att == nil {
		return nil
	}
	refund := classifyRefundedInStripe(inv, r)
	att.Message = refund.Message + " Hardcount also flagged a payment on this invoice: check both in Stripe."
	att.Detail = joinDetail(refund.Detail, "Hardcount also flagged a payment on this invoice. "+att.Detail)
	return att
}

// withLostDispute rewrites a payment-anomaly notice for an invoice that also
// lost a dispute (#1287 review). The anomaly's own instruction (refund the
// extra charge, refund the payment on a voided invoice) assumes Stripe still
// holds the money, and after a chargeback it may not: refund the extra charge
// of a duplicate whose original the bank took back, and the customer has both
// payments back. Stripe refuses a refund of the charged-back payment itself
// (error charge_disputed), so the risk is the other one. The anomaly keeps
// its reason, code and "Open in Stripe" button; the message stops asking for
// a refund, and Detail puts the dispute and its payment before the anomaly's
// own facts, so the operator can see which payment the bank took back.
func withLostDispute(att *domain.Attention, d domain.InvoiceDispute) *domain.Attention {
	if att == nil {
		return nil
	}
	att.Message = "The customer's bank took back " + domain.FormatMoneyMinor(d.AmountCents, d.Currency) +
		" after a lost dispute, so check what Stripe still holds before you refund anything."
	att.Detail = joinDetail(
		"Stripe dispute "+d.StripeDisputeID+" on payment "+d.StripePaymentIntentID+
			" closed as lost: the customer already has that money back, and Stripe refuses a refund of it. "+
			"A refund of another payment on this invoice can return the same money twice.",
		"Hardcount also flagged a payment on this invoice. "+att.Detail)
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
