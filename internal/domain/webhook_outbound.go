package domain

import (
	"context"
	"time"
)

// WebhookEndpoint is a tenant's URL that receives the outbound webhook
// events it subscribes to, signed with its secret.
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
	// During a rotation with a grace period, SecondarySecret carries the
	// previous signing key, and SecondarySecretExpiresAt is set 72h in the
	// future. The dispatcher signs outbound events with both keys during the
	// window (two v1= entries in Hardcount-Signature, as Stripe does). So
	// partners can roll out a verifier update without breaking production
	// webhooks. After expiry, signing skips both fields. The row keeps them
	// as unused data until the next rotation overwrites them.
	SecondarySecret          string     `json:"-"`
	SecondarySecretLast4     string     `json:"secondary_secret_last4,omitempty"`
	SecondarySecretExpiresAt *time.Time `json:"secondary_secret_expires_at,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

// WebhookEvent is one outbound event: its type and payload, delivered to
// every endpoint that subscribes to the type.
type WebhookEvent struct {
	ID        string         `json:"id"`
	TenantID  string         `json:"tenant_id,omitempty"`
	Livemode  bool           `json:"livemode"`
	EventType string         `json:"event_type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
	// ReplayOfEventID is set when an operator produced this row by clicking
	// "Replay" on an earlier event in the dashboard. It points back to the
	// event the operator was looking at, not to the latest replay clone. So
	// the audit chain has one pivot: the original has many clones, never
	// original → A → B → …. The list-deliveries endpoint joins the original
	// and its clones into one attempt timeline.
	ReplayOfEventID *string `json:"replay_of_event_id,omitempty"`
	// ReplayOfCreatedAt is the created_at of the ReplayOfEventID event. A
	// replay goes to receivers under that event's id and this time, so a
	// receiver that skips seen ids skips it. Loaded only by GetEvent and
	// CreateReplayEvent, the two reads that feed a delivery attempt. Not
	// part of the API.
	ReplayOfCreatedAt time.Time `json:"-"`
}

// DeliveryStatus is where one delivery of an event to an endpoint stands.
type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliverySucceeded DeliveryStatus = "succeeded"
	DeliveryFailed    DeliveryStatus = "failed"
)

// WebhookDelivery is one event's delivery to one endpoint: its attempts,
// the receiver's last answer and when it retries next.
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
	// EndpointURL and EndpointDescription are filled only by the
	// delivery-listing JOIN, to name the receiver on the operator timeline.
	// A delivery must keep a human-readable receiver even after its endpoint
	// is deleted. Deletes are soft (migration 0168), so the join still finds
	// the endpoint. Writers never set these.
	EndpointURL         string `json:"endpoint_url,omitempty"`
	EndpointDescription string `json:"endpoint_description,omitempty"`
	// EventType and EventReplayOfID are filled only by the endpoint-scoped
	// delivery-listing JOIN (the endpoint drill-down). That view lists
	// deliveries across events, so each row must say which business fact it
	// carried without fetching the event per row. Writers never set these.
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
	// EventInvoiceMarkedUncollectible matches Stripe's event of the same name
	// (https://docs.stripe.com/api/events/types#event_types-invoice.marked_uncollectible).
	// It fires when an invoice moves to status=uncollectible: through
	// dunning's mark_uncollectible final action, through the operator's
	// ResolveRun(invoice_not_collectible), or through the direct API call.
	// Subscribers should treat it as a bad-debt write-off: stop expecting
	// payment, and leave the invoice out of receivables and revenue
	// rollups.
	EventInvoiceMarkedUncollectible = "invoice.marked_uncollectible"
	// EventInvoicePaymentRecorded fires when an operator records an
	// out-of-band payment (cheque, wire, cash). It is distinct from
	// invoice.paid (collected by the engine through a PaymentIntent), so
	// analytics can tell the two apart. Stripe makes the same distinction
	// with the paid_out_of_band flag on the invoice.
	EventInvoicePaymentRecorded = "invoice.payment_recorded"
	EventPaymentSucceeded       = "payment.succeeded"
	EventPaymentFailed          = "payment.failed"
	// EventPaymentMethodAttached fires when a payment method lands on a
	// customer through a completed Checkout setup (setup_intent.succeeded).
	// It is the webhook-side signal that charge-on-attach now proceeds for
	// any queued invoices. Payload: customer_id, stripe_payment_method_id.
	EventPaymentMethodAttached = "payment_method.attached"
	// EventPaymentMethodUpdated fires when a customer's payment-method setup
	// completes or refreshes with card metadata (the hosted update-payment
	// or portal flow). Payload: customer_id, stripe_customer_id, card_brand,
	// card_last4.
	EventPaymentMethodUpdated = "payment_method.updated"
	// EventPaymentDuplicateCharge fires when a second, different
	// PaymentIntent succeeds against an already-paid invoice (two devices,
	// or an old Checkout session that is still live). Money was captured
	// twice, and the extra exists only in Stripe until the operator refunds
	// it. It is its own event for this cause, and nothing refunds it
	// automatically (ADR-068).
	EventPaymentDuplicateCharge = "payment.duplicate_charge"
	// EventPaymentAmountMismatch fires when a charge settles for an amount
	// other than the invoice's amount_due at settle time. An example is a
	// Checkout session paid after a credit note changed the amount due: the
	// accepted residual, a drift window of at most 1h. It only reports: the
	// books show the difference, and the operator reconciles it (ADR-068).
	EventPaymentAmountMismatch = "payment.amount_mismatch"
	// EventPaymentCurrencyMismatch fires when a charge settles in a currency
	// other than the invoice header's. It compares the charge currency
	// Stripe reports with Hardcount's label, case-folded, on the webhook
	// settle path, the one entry point that carries Stripe's value.
	// Hardcount creates the PaymentIntent from the header, so this event
	// means the label moved, or the charge was made out of band. It only
	// reports (ADR-100).
	EventPaymentCurrencyMismatch = "payment.currency_mismatch"
	// EventPaymentReceivedOnVoidedInvoice fires when a payment lands on a
	// voided (or otherwise non-payable) invoice. The void's step that expires
	// the Checkout session failed, and the customer paid in the window that
	// was left. It is a different cause from duplicate_charge: the money is
	// owed back, not collected twice.
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
	// SpamComplaint webhook reports that the recipient marked a message as
	// spam (ADR-098). It is its own event on purpose, not folded into
	// email_bounced. A complaint is a person opting out, not a dead mailbox,
	// and it is the more severe state, which the provider cannot undo.
	EventCustomerEmailComplained = "customer.email_complained"
	EventDunningStarted          = "dunning.started"
	EventDunningEscalated        = "dunning.escalated"
	EventDunningResolved         = "dunning.resolved"
	// The credit balance events fire when the balance value crosses a line
	// (ADR-078). Each ledger-writing transaction computes SUM(amount_cents)
	// before and after, and enqueues the event in the same transaction
	// (transactional outbox). They are well ordered per customer, because
	// every balance writer holds the per-customer advisory lock. Payloads
	// carry customer_id and balance_cents (plus threshold_cents on
	// balance_low). So consumers with commits of different sizes can add
	// per-customer logic without server config.
	//
	// The three balance events are per currency (ADR-129). Each one is about
	// the balance in the payload's `currency`, which the crossing is
	// computed on. Payload: customer_id, currency, balance_cents (plus
	// threshold_cents on low).
	//
	// EventCreditBalanceLow fires when the balance crosses from >= to < the
	// tenant's configured threshold
	// (tenant_settings.credit_balance_low_threshold_cents). The threshold
	// applies in each currency's own units, and when it is unset the event
	// never fires.
	EventCreditBalanceLow = "credit.balance_low"
	// EventCreditBalanceDepleted fires on the >0 → <=0 crossing. It has a
	// known lag: an expired block that is not swept yet keeps SUM positive
	// until the expiry sweep retires it. The sweep's expiry entry produces
	// the crossing, within minutes, which matches how expiry works.
	EventCreditBalanceDepleted = "credit.balance_depleted"
	// EventCreditBalanceRecovered fires on the <=0 → >0 crossing. It is not
	// kept for parity, since only one peer (Orb) has it. It is the
	// state-machine complement of depleted: without it, a consumer's
	// depleted state could never clear.
	EventCreditBalanceRecovered = "credit.balance_recovered"
	// EventCreditCommitRetired fires when a relief credit note retires
	// commit credits (ADR-080). The payload carries customer_id, grant_id,
	// currency, funding_invoice_id, credit_note_id, retired_cents,
	// refunded_gross_cents and remaining_after_cents. It is enqueued in the
	// credit-note coordinator's transaction, so it is sent exactly once with
	// the retire.
	EventCreditCommitRetired = "credit.commit_retired"
)

// KnownWebhookEventTypes is the emit catalog: every event type the engine
// dispatches, and nothing else. Endpoint create and update check
// subscriptions against it. Without the check, a subscription to a name
// Hardcount never sends (such as "invoice.payment_failed", which is
// payment.failed here) would match nothing and stay silent forever. Keep it
// in step with the constants above, the dashboard's EVENT_GROUPS picker
// (web-v2/src/pages/Webhooks.tsx) and docs/api/webhook-events.md.
// webhook_catalog_test.go and webhook_events_doc_test.go fail when they
// differ.
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
