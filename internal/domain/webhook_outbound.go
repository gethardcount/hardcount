package domain

import (
	"context"
	"time"
)

type WebhookEndpoint struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id,omitempty"`
	Livemode    bool     `json:"livemode"`
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	Secret      string   `json:"-"` // Plaintext signing key. Only populated after create/rotate or after store decrypts it for dispatch.
	SecretLast4 string   `json:"secret_last4"`
	Events      []string `json:"events"`
	Active      bool     `json:"active"`
	// During a grace-period rotation, SecondarySecret carries the previous
	// signing key and SecondarySecretExpiresAt is set 72h in the future.
	// The dispatcher signs outbound events with both during the window
	// (two v1= entries in Hardcount-Signature, Stripe-style) so partners can
	// stage a verifier update without breaking production webhooks. After
	// expiry, both fields are skipped at sign time; the row keeps them as
	// cold data until the next rotation overwrites them.
	SecondarySecret          string     `json:"-"`
	SecondarySecretLast4     string     `json:"secondary_secret_last4,omitempty"`
	SecondarySecretExpiresAt *time.Time `json:"secondary_secret_expires_at,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

type WebhookEvent struct {
	ID        string         `json:"id"`
	TenantID  string         `json:"tenant_id,omitempty"`
	Livemode  bool           `json:"livemode"`
	EventType string         `json:"event_type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
	// ReplayOfEventID is set when this row was produced by an operator
	// clicking "Replay" on an earlier event in the dashboard. It points
	// back to the event the operator was looking at — not to the most
	// recent replay clone — so the audit chain stays single-pivot
	// (original → many clones, never original → A → B → …). The list-
	// deliveries endpoint stitches the original and its clones into one
	// unified attempt timeline.
	ReplayOfEventID *string `json:"replay_of_event_id,omitempty"`
	// ReplayOfCreatedAt is the created_at of the ReplayOfEventID event. A
	// replay goes to receivers under that event's id and this time, so a
	// receiver that skips seen ids skips it. Loaded only by GetEvent and
	// CreateReplayEvent, the two reads that feed a delivery attempt. Not
	// part of the API.
	ReplayOfCreatedAt time.Time `json:"-"`
}

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliverySucceeded DeliveryStatus = "succeeded"
	DeliveryFailed    DeliveryStatus = "failed"
)

type WebhookDelivery struct {
	ID                string         `json:"id"`
	TenantID          string         `json:"tenant_id,omitempty"`
	Livemode          bool           `json:"livemode"`
	WebhookEndpointID string         `json:"webhook_endpoint_id"`
	WebhookEventID    string         `json:"webhook_event_id"`
	Status            DeliveryStatus `json:"status"`
	HTTPStatusCode    int            `json:"http_status_code,omitempty"`
	ResponseBody      string         `json:"response_body,omitempty"`
	ErrorMessage      string         `json:"error_message,omitempty"`
	AttemptCount      int            `json:"attempt_count"`
	NextRetryAt       *time.Time     `json:"next_retry_at,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	CompletedAt       *time.Time     `json:"completed_at,omitempty"`
	// EndpointURL / EndpointDescription are hydrated only by the
	// delivery-listing JOIN, for historical attribution on the operator
	// timeline: a delivery must stay attributable to a human-readable
	// receiver even after that endpoint is deleted (0168 deletes are
	// soft, so the join still resolves). Writers never set these.
	EndpointURL         string `json:"endpoint_url,omitempty"`
	EndpointDescription string `json:"endpoint_description,omitempty"`
	// EventType / EventReplayOfID are hydrated only by the endpoint-scoped
	// delivery-listing JOIN (the endpoint drill-down): that surface lists
	// deliveries ACROSS events, so each row must say which business fact it
	// carried without a per-row event fetch. Writers never set these.
	EventType       string `json:"event_type,omitempty"`
	EventReplayOfID string `json:"event_replay_of_id,omitempty"`
}

// EventDispatcher fires outbound webhook events.
type EventDispatcher interface {
	Dispatch(ctx context.Context, tenantID, eventType string, payload map[string]any) error
}

// Well-known event types emitted by Hardcount.
const (
	EventInvoiceFinalized = "invoice.finalized"
	EventInvoicePaid      = "invoice.paid"
	EventInvoiceVoided    = "invoice.voided"
	// EventInvoiceMarkedUncollectible matches Stripe's same-named event
	// (https://docs.stripe.com/api/events/types#event_types-invoice.marked_uncollectible).
	// Fired when an invoice transitions to status=uncollectible —
	// either via dunning's mark_uncollectible final-action or via the
	// operator-driven ResolveRun(invoice_not_collectible) + the direct
	// API call. Subscribers should treat this as a bad-debt write-off
	// signal: stop expecting payment, exclude from AR/revenue rollups.
	EventInvoiceMarkedUncollectible = "invoice.marked_uncollectible"
	// EventInvoicePaymentRecorded fires when an operator records an
	// out-of-band payment (cheque, wire, cash). Distinct from
	// invoice.paid (engine-collected via PaymentIntent) so analytics
	// can tell the two apart — Stripe-parity (their paid_out_of_band
	// flag on the invoice surfaces the same distinction).
	EventInvoicePaymentRecorded = "invoice.payment_recorded"
	EventPaymentSucceeded       = "payment.succeeded"
	EventPaymentFailed          = "payment.failed"
	// EventPaymentMethodAttached fires when a payment method lands on a
	// customer via a completed Checkout setup (setup_intent.succeeded) —
	// the webhook-side signal that charge-on-attach will now proceed for
	// any queued invoices. Payload: customer_id, stripe_payment_method_id.
	EventPaymentMethodAttached = "payment_method.attached"
	// EventPaymentMethodUpdated fires when a customer's payment-method
	// setup completes/refreshes (hosted update-payment / portal flow) with
	// card metadata. Payload: customer_id, stripe_customer_id, card_brand,
	// card_last4. Both payment_method.* events were EMITTED long before
	// they were admitted to this catalog — subscriptions to them were
	// rejected as "Hardcount never emits it" (2026-07-10 design census).
	EventPaymentMethodUpdated = "payment_method.updated"
	// EventPaymentDuplicateCharge fires when a SECOND, different
	// PaymentIntent succeeds against an already-paid invoice (two devices,
	// a stale-but-live Checkout session): money was captured twice and
	// exists only in Stripe until the operator refunds. Per-cause event
	// (webhook_events_per_cause) — no auto-refund (ADR-068).
	EventPaymentDuplicateCharge = "payment.duplicate_charge"
	// EventPaymentAmountMismatch fires when a charge settles for an amount
	// different from the invoice's amount_due at settle time (a Checkout
	// session paid after a credit note changed the due — the accepted ≤1h
	// drift residual). Detection-only: books show the delta, operator
	// reconciles. ADR-068.
	EventPaymentAmountMismatch = "payment.amount_mismatch"
	// EventPaymentCurrencyMismatch fires when a charge settles in a currency
	// different from the invoice header's — Stripe's independently-reported
	// charge currency vs Hardcount's label, compared case-folded on the webhook
	// settle path (the one entry point carrying Stripe's value). Hardcount
	// creates the PaymentIntent from the header, so this firing means the
	// label moved (or an out-of-band charge) — detection-only. ADR-100.
	EventPaymentCurrencyMismatch = "payment.currency_mismatch"
	// EventPaymentReceivedOnVoidedInvoice fires when a payment lands on a
	// voided (or otherwise non-payable) invoice — the void's session-expire
	// leg failed and the customer paid inside the residual. Distinct cause
	// from duplicate_charge: the money is owed BACK, not double-collected.
	EventPaymentReceivedOnVoidedInvoice = "payment.received_on_voided_invoice"
	// EventPaymentDisputeOpened fires once per Stripe dispute, when Hardcount
	// first records it against an invoice (charge.dispute.*, #979). Queued in
	// the transaction that records it. Hardcount changes nothing else: the invoice
	// stays paid and no money moves.
	EventPaymentDisputeOpened = "payment.dispute_opened"
	// EventPaymentDisputeClosed fires when a recorded dispute reaches a closed
	// status it did not have: won, lost, warning_closed (an inquiry that never
	// became a chargeback) or prevented. A lost dispute that Stripe later
	// marks won fires it again. Queued in the transaction that records it.
	EventPaymentDisputeClosed = "payment.dispute_closed"
	// EventPaymentRefundedInStripe fires once per refund made in Stripe
	// outside Hardcount on one of the invoice's payments (ADR-134), when
	// Hardcount first sees it: recorded as a credit note, or flagged. Queued in
	// the transaction that records it. A later change to the refund (it
	// fails, say) sends nothing more; a recorded note's refund status follows
	// it as for any refund.
	EventPaymentRefundedInStripe            = "payment.refunded_in_stripe"
	EventSubscriptionCreated                = "subscription.created"
	EventSubscriptionActivated              = "subscription.activated"
	EventSubscriptionCanceled               = "subscription.canceled"
	EventSubscriptionItemAdded              = "subscription.item.added"
	EventSubscriptionItemUpdated            = "subscription.item.updated"
	EventSubscriptionItemRemoved            = "subscription.item.removed"
	EventSubscriptionPendingChangeScheduled = "subscription.pending_change.scheduled"
	EventSubscriptionPendingChangeApplied   = "subscription.pending_change.applied"
	EventSubscriptionPendingChangeCanceled  = "subscription.pending_change.canceled"
	EventSubscriptionCancelScheduled        = "subscription.cancel_scheduled"
	EventSubscriptionCancelCleared          = "subscription.cancel_cleared"
	EventSubscriptionCollectionPaused       = "subscription.collection_paused"
	EventSubscriptionCollectionResumed      = "subscription.collection_resumed"
	EventSubscriptionTrialEnded             = "subscription.trial_ended"
	EventSubscriptionTrialExtended          = "subscription.trial_extended"
	EventSubscriptionThresholdCrossed       = "subscription.threshold_crossed"
	EventCustomerEmailBounced               = "customer.email_bounced"
	// EventCustomerEmailComplained fires when the email provider's
	// SpamComplaint webhook reports the recipient marked a message as
	// spam (ADR-098). Per-cause, deliberately not folded into
	// email_bounced: a complaint is a person opting out, not a dead
	// mailbox, and it is the more severe (provider-irreversible) state.
	EventCustomerEmailComplained = "customer.email_complained"
	EventDunningStarted          = "dunning.started"
	EventDunningEscalated        = "dunning.escalated"
	EventDunningResolved         = "dunning.resolved"
	// Credit balance-VALUE crossing events (ADR-078). Computed on
	// SUM(amount_cents) before/after inside each ledger-writing tx and
	// enqueued on the same tx (transactional outbox) — well-ordered per
	// customer because every balance writer holds the per-customer
	// advisory lock. Payloads carry customer_id + balance_cents (+
	// threshold_cents on balance_low) so consumers with heterogeneous
	// commit sizes can layer per-customer logic without server config.
	//
	// The three balance events are per currency (ADR-129): each one is about
	// the balance in the payload's `currency`, which the crossing is computed
	// on. Payload: customer_id, currency, balance_cents (+ threshold_cents on
	// low).
	//
	// EventCreditBalanceLow fires when the balance crosses from >= to
	// < the tenant's configured threshold (tenant_settings.
	// credit_balance_low_threshold_cents, applied in each currency's own
	// units; unset = never fires).
	EventCreditBalanceLow = "credit.balance_low"
	// EventCreditBalanceDepleted fires on the >0 → <=0 crossing. Known
	// lag: an expired-but-unswept block keeps SUM positive until the
	// expiry sweep retires it — the sweep's expiry entry produces the
	// crossing (minutes-scale, matches the expiry discipline).
	EventCreditBalanceDepleted = "credit.balance_depleted"
	// EventCreditBalanceRecovered fires on the <=0 → >0 crossing. Kept
	// not for parity (single-peer, Orb) but as the state-machine
	// complement of depleted — without it a consumer's depleted state
	// could never clear.
	EventCreditBalanceRecovered = "credit.balance_recovered"
	// EventCreditCommitRetired fires when a relief credit note retires
	// commit credits (ADR-080): payload carries customer_id, grant_id,
	// currency, funding_invoice_id, credit_note_id, retired_cents,
	// refunded_gross_cents, remaining_after_cents. Enqueued IN the CN
	// coordinator tx — exactly-once with the retire.
	EventCreditCommitRetired = "credit.commit_retired"
)

// KnownWebhookEventTypes is the canonical emit catalog — every event type
// the engine actually dispatches, and nothing else. Endpoint create/update
// validates subscriptions against it: pre-2026-07-05 any string was
// accepted and matchesEvent silently never fired, so a Stripe-trained
// subscriber to "invoice.payment_failed" (real name: payment.failed) got
// silence forever — Hardcount's own shipped recipes carried two such phantom
// names. Keep in lockstep with the constants above, the dashboard's
// EVENT_GROUPS picker (web-v2/src/pages/Webhooks.tsx) and docs/webhooks.md;
// webhook_catalog_test.go fails when they differ.
var KnownWebhookEventTypes = map[string]bool{
	EventInvoiceFinalized:                   true,
	EventInvoicePaid:                        true,
	EventInvoiceVoided:                      true,
	EventInvoiceMarkedUncollectible:         true,
	EventInvoicePaymentRecorded:             true,
	EventPaymentSucceeded:                   true,
	EventPaymentFailed:                      true,
	EventPaymentDuplicateCharge:             true,
	EventPaymentAmountMismatch:              true,
	EventPaymentCurrencyMismatch:            true,
	EventPaymentReceivedOnVoidedInvoice:     true,
	EventPaymentDisputeOpened:               true,
	EventPaymentDisputeClosed:               true,
	EventPaymentRefundedInStripe:            true,
	EventPaymentMethodAttached:              true,
	EventPaymentMethodUpdated:               true,
	EventSubscriptionCreated:                true,
	EventSubscriptionActivated:              true,
	EventSubscriptionCanceled:               true,
	EventSubscriptionItemAdded:              true,
	EventSubscriptionItemUpdated:            true,
	EventSubscriptionItemRemoved:            true,
	EventSubscriptionPendingChangeScheduled: true,
	EventSubscriptionPendingChangeApplied:   true,
	EventSubscriptionPendingChangeCanceled:  true,
	EventSubscriptionCancelScheduled:        true,
	EventSubscriptionCancelCleared:          true,
	EventSubscriptionCollectionPaused:       true,
	EventSubscriptionCollectionResumed:      true,
	EventSubscriptionTrialEnded:             true,
	EventSubscriptionTrialExtended:          true,
	EventSubscriptionThresholdCrossed:       true,
	EventCustomerEmailBounced:               true,
	EventCustomerEmailComplained:            true,
	EventDunningStarted:                     true,
	EventDunningEscalated:                   true,
	EventDunningResolved:                    true,
	EventCreditBalanceLow:                   true,
	EventCreditBalanceDepleted:              true,
	EventCreditBalanceRecovered:             true,
	EventCreditCommitRetired:                true,
}
