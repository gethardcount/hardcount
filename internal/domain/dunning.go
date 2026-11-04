package domain

import (
	"strings"
	"time"
)

// A dunning run is Hardcount's automatic collection of one unpaid invoice:
// it retries the charge on a schedule. When the retries run out, the run is
// exhausted, which settles two independent questions. ADR-112 gives each its
// own column: what happens to the subscription, and what happens to the
// unpaid invoice.
//
// With one column for each question, a policy can express Stripe's own
// default outcome: cancel the subscription and write off the invoice.
// Stripe, Recurly and Chargebee all settle both questions (ADR-112 quotes
// each).
//
// The old `manual_review` value is (SubActionNone, InvActionNone), which
// matches Chargebee's "remain active" + "not paid".

// DunningSubscriptionAction is what exhaustion does to the subscription
// behind the unpaid invoice. It does nothing when the invoice has no
// subscription (a one-off invoice), and the escalation copy must then not
// claim it did.
type DunningSubscriptionAction string

const (
	// SubActionNone leaves the subscription alone, for the operator to
	// handle. It matches Stripe's "leave past_due" and Chargebee's "remain
	// active".
	SubActionNone DunningSubscriptionAction = "none"
	// SubActionPause calls subscription.PauseCollection
	// (behavior=keep_as_draft). The cycle keeps drafting invoices, but
	// nothing is charged and no dunning starts until an operator resumes.
	// It matches Stripe's pause_collection.
	SubActionPause DunningSubscriptionAction = "pause"
	// SubActionCancel cancels the subscription outright.
	SubActionCancel DunningSubscriptionAction = "cancel"
)

// DunningInvoiceAction is what exhaustion does to the unpaid invoice that
// the run was chasing.
type DunningInvoiceAction string

const (
	// InvActionNone leaves the invoice finalized and due. It is the default,
	// because nobody has decided yet that this is bad debt. The receivable
	// (the money the customer owes) stays open until a person closes or
	// collects it, and the policy form says so in plain words.
	InvActionNone DunningInvoiceAction = "none"
	// InvActionMarkUncollectible writes the invoice off as bad debt. The
	// receivable closes. The invoice stays on the books for audit, and it
	// can still be settled out of band (ADR-110).
	//
	// Auto-void and Chargebee's auto-"Reverse" (mark paid plus an adjustment
	// credit note) are not offered, on purpose (ADR-112). Void asserts that
	// the sale never happened, and it reverses tax (ADR-111). A machine
	// making that assertion about delivered usage creates exactly the bad
	// money event the governing rule refuses to create (see
	// RecoveryWarnsOnOfflinePayment).
	InvActionMarkUncollectible DunningInvoiceAction = "mark_uncollectible"
)

// Valid reports whether the action is a known enum value. UpsertPolicy
// refuses any other value instead of using a default. An unknown action
// that silently became "none" would stop closing the receivables the
// operator asked to close.
func (a DunningSubscriptionAction) Valid() bool {
	return a == SubActionNone || a == SubActionPause || a == SubActionCancel
}

// Valid reports whether the action is a known enum value. See the note on
// DunningSubscriptionAction.Valid.
func (a DunningInvoiceAction) Valid() bool {
	return a == InvActionNone || a == InvActionMarkUncollectible
}

// DunningEscalationOutcome is what a terminal exhaustion actually did to one
// invoice, which is not always what its policy asked for. Each field means
// "the requested end state holds". So a field stays true across a
// re-attempt that skipped a half already in place. It is false when the
// action could not apply here at all: a one-off invoice has no subscription
// to cancel, and an unwired mover cannot act.
//
// The escalation email, the escalated timeline row and the outbound webhook
// all show this outcome, not the policy. A debtor must never read "your
// subscription has been canceled" when nothing was canceled.
type DunningEscalationOutcome struct {
	SubscriptionPaused   bool
	SubscriptionCanceled bool
	InvoiceWrittenOff    bool
}

// Reason is the stable machine token stamped on the escalated event and the
// outbound webhook. It describes outcomes and does not echo the policy. An
// empty string means the run escalated and nothing changed. That is the
// true reading both of a (none, none) policy and of a one-off invoice under
// a cancel policy.
func (o DunningEscalationOutcome) Reason() string {
	var parts []string
	if o.SubscriptionPaused {
		parts = append(parts, "subscription_paused")
	}
	if o.SubscriptionCanceled {
		parts = append(parts, "subscription_canceled")
	}
	if o.InvoiceWrittenOff {
		parts = append(parts, "invoice_marked_uncollectible")
	}
	return strings.Join(parts, "+")
}

// Any reports whether exhaustion changed anything at all.
func (o DunningEscalationOutcome) Any() bool {
	return o.SubscriptionPaused || o.SubscriptionCanceled || o.InvoiceWrittenOff
}

// DunningRunState is where a dunning run stands: active, resolved, or
// escalated (every retry failed and the final action was taken).
type DunningRunState string

const (
	DunningActive    DunningRunState = "active"
	DunningResolved  DunningRunState = "resolved"
	DunningEscalated DunningRunState = "escalated"
)

// DunningStartCause is why collection needed a dunning run. The caller that
// starts the run records it at write time, because each caller knows the
// cause exactly. Working it out later from invoice state would bring back
// the kind of drift between readers that ADR-101 removed for billing. The
// cause is stamped on the run and its dunning_started event, and the
// invoice timeline shows it, so the failure story survives after the
// attention banner clears.
type DunningStartCause string

const (
	// DunningCausePaymentFailed means a real charge was attempted and
	// failed: an interactive decline, an auto-charge decline, or the
	// failed-invoice backfill sweep.
	DunningCausePaymentFailed DunningStartCause = "payment_failed"
	// DunningCauseNoPaymentMethod means no usable payment method existed, so
	// nothing was ever charged. This is the card-less "reminder cycle"
	// enrollment. A "payment failed" row for it would be false, because no
	// payment happened.
	DunningCauseNoPaymentMethod DunningStartCause = "no_payment_method"
)

// DunningRetryReasonCardRefused is the reason on a retry_attempted event for
// a scheduled retry that charged nothing: the bank had permanently refused
// the customer's default card on this invoice (ErrCardNeedsReplacing,
// #966). The attempt counts, and the timeline shows it as not charged, the
// way it shows a retry with no card (DunningCauseNoPaymentMethod).
const DunningRetryReasonCardRefused = "card_refused"

// DunningRetryReasonNeedsConfirmation is the reason on a retry_attempted
// event for a scheduled retry that charged nothing because the bank asked
// the customer to confirm the last charge of their default card on this
// invoice (ErrCardNeedsConfirmation). The attempt counts, and the timeline
// shows it as not charged.
const DunningRetryReasonNeedsConfirmation = "needs_confirmation"

// DunningRetryReasonBelowMinimum is the reason on a retry_attempted event for
// a retry Stripe refused because the amount is below its minimum charge
// (ErrBelowMinimumCharge). Nothing was charged and the run ends on it.
const DunningRetryReasonBelowMinimum = BelowMinimumCode

// DunningRetryReasonPaymentProcessing is the reason on a retry_attempted
// event for a retry whose payment Stripe is still processing (#1289). The
// attempt counts and the run stays active: the payment's outcome decides
// what happens next. A later failure gets a retry_attempted row of its own,
// carrying the PaymentIntent; this row carries none.
const DunningRetryReasonPaymentProcessing = "payment_processing"

// Valid reports whether the cause is a known enum value. StartDunning
// refuses any other value instead of using a default. An unlabeled run would
// bring back the false hardcoded reason that this type exists to end.
func (c DunningStartCause) Valid() bool {
	return c == DunningCausePaymentFailed || c == DunningCauseNoPaymentMethod
}

// DunningEventType names one event in a dunning run's history.
type DunningEventType string

const (
	DunningEventStarted        DunningEventType = "dunning_started"
	DunningEventRetryAttempted DunningEventType = "retry_attempted"
	DunningEventEscalated      DunningEventType = "escalated"
	DunningEventResolved       DunningEventType = "resolved"
)

// DunningResolution is how a dunning run ended, or why it is held for a
// re-attempt.
type DunningResolution string

const (
	ResolutionPaymentRecovered DunningResolution = "payment_recovered"
	// ResolutionInvoiceVoided means the run closed because its invoice was
	// voided: annulled, with applied credits reversed and collection stopped
	// at Stripe. The operator's "Void invoice" resolution writes it, and so
	// do the invoice-void handler and the engine's terminal floor when it
	// finds an invoice already voided. ResolutionInvoiceNotCollectible names
	// a write-off, so the column always says which of the two happened.
	ResolutionInvoiceVoided    DunningResolution = "invoice_voided"
	ResolutionRetriesExhausted DunningResolution = "retries_exhausted"
	// ResolutionInvoiceNotCollectible is the operator's version of the
	// automatic MarkUncollectible final action. Picking it in the
	// resolve-dunning dialog records the run as resolved, and it also moves
	// the invoice to status=uncollectible (bad debt). The side effects are
	// the same as on the automatic path. It is Stripe's "Mark
	// uncollectible", offered in the dunning resolve flow.
	ResolutionInvoiceNotCollectible DunningResolution = "invoice_not_collectible"
	// ResolutionActionFailed marks a run whose terminal action (pause,
	// cancel or mark-uncollectible) failed at exhaustion, for example on a
	// Stripe blip, a conflicting state or a database error. The run stays
	// state=active, not escalated, with next_action_at set, so the due-run
	// pickers re-attempt the action. Otherwise the run would record a clean
	// "escalated" beside an invoice or subscription that was never closed.
	ResolutionActionFailed DunningResolution = "action_failed"
	// ResolutionAmountBelowMinimum means a charge on the run's invoice got
	// Stripe's "below the minimum charge" refusal (ErrBelowMinimumCharge). No
	// retry can collect that amount by card. So recovery ends here, without
	// running out its retries and its final action, and the invoice stays
	// open for the operator.
	ResolutionAmountBelowMinimum DunningResolution = BelowMinimumCode
)

// DunningPolicy is a named template that drives the retry state machine for
// one or more customers (ADR-036, campaigns model). There is one
// is_default=true policy per (tenant, livemode). A customer without an
// explicit `dunning_policy_id` inherits the default. The shape mirrors the
// named campaigns of Lago and Recurly.
type DunningPolicy struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id,omitempty"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	IsDefault bool   `json:"is_default"`
	// Seeded is true for the policy the account was created with, until
	// the operator edits it or makes it the default. While it is true, the
	// next policy created takes over the default slot (ADR-123).
	Seeded bool `json:"seeded"`
	// RetryDays is the whole schedule: the days to wait before each retry.
	// The first wait counts from the failed payment, or, with no card on
	// file, from the collection that found none (StartDunning's anchor).
	// Each later wait counts from the retry before it, so [3, 4, 7] retries
	// on day 3, 7 and 14. The number of retries is its length. A day is 24
	// hours.
	RetryDays []int `json:"retry_days"`
	// The two terminal decisions (ADR-112). Both are always present. The
	// (none, none) pair is the old `manual_review`.
	FinalSubscriptionAction DunningSubscriptionAction `json:"final_subscription_action"`
	FinalInvoiceAction      DunningInvoiceAction      `json:"final_invoice_action"`
	CreatedAt               time.Time                 `json:"created_at"`
	UpdatedAt               time.Time                 `json:"updated_at"`
}

// Limits on DunningPolicy.RetryDays, checked on every write and by the
// column's CHECK.
const (
	MaxDunningRetries   = 15
	MaxDunningRetryWait = 30 // days
)

// Retries is how many retries the policy makes.
func (p DunningPolicy) Retries() int { return len(p.RetryDays) }

// WaitBeforeRetry is how long retry n (1-based) waits. Retry 1 waits after
// the failed payment (or the collection that found no card), and each other
// retry waits after retry n-1. ok is false when the policy has no retry n.
func (p DunningPolicy) WaitBeforeRetry(n int) (wait time.Duration, ok bool) {
	if n < 1 || n > len(p.RetryDays) {
		return 0, false
	}
	return time.Duration(p.RetryDays[n-1]) * 24 * time.Hour, true
}

// InvoiceDunningRun is one dunning run: the collection of one unpaid invoice
// under one policy, with its attempts, its next action and its state.
type InvoiceDunningRun struct {
	ID            string            `json:"id"`
	TenantID      string            `json:"tenant_id,omitempty"`
	InvoiceID     string            `json:"invoice_id"`
	CustomerID    string            `json:"customer_id,omitempty"`
	PolicyID      string            `json:"policy_id"`
	State         DunningRunState   `json:"state"`
	Reason        string            `json:"reason,omitempty"`
	AttemptCount  int               `json:"attempt_count"`
	LastAttemptAt *time.Time        `json:"last_attempt_at,omitempty"`
	NextActionAt  *time.Time        `json:"next_action_at,omitempty"`
	Paused        bool              `json:"paused"`
	ResolvedAt    *time.Time        `json:"resolved_at,omitempty"`
	Resolution    DunningResolution `json:"resolution,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`

	// The invoice number, amount due and currency are copied in for the
	// /dunning list, which saves one read per row. The LEFT JOIN in ListRuns
	// fills them. They are empty or zero when the joined invoice cannot be
	// read (deleted, or hidden by RLS).
	InvoiceNumber    string `json:"invoice_number,omitempty"`
	InvoiceAmountDue int64  `json:"invoice_amount_due_cents,omitempty"`
	InvoiceCurrency  string `json:"invoice_currency,omitempty"`

	// EffectiveNow is the owning customer's test-clock frozen_time when the
	// run is on a clock-pinned subscription (one whose customer is on a test
	// clock). The frontend uses it as "now" for relative times. Wall-clock
	// time would show "overdue" on every row whose next_action_at is in the
	// clock's time (for example 2024-02 while the wall clock is in 2026). It
	// is nil for wall-clock runs, and the relative-time renderer then uses
	// Date.now(). The server's value is authoritative.
	EffectiveNow *time.Time `json:"effective_now,omitempty"`
	// TestClockID is the owning customer's clock when pinned. The server
	// sets it, so the amber test-clock badge shows on every pinned run,
	// one-off invoices and later pages included.
	TestClockID string `json:"test_clock_id,omitempty"`
	// MaxRetryAttempts is how many retries the bound policy makes (the
	// length of its retry_days). The list reads (ListRuns,
	// ListRunsByInvoices) read it with the run, and it is 0 where it was not
	// read. The policy row is read every tick, so this is the limit the
	// run's next tick applies.
	MaxRetryAttempts int `json:"max_retry_attempts,omitempty"`
}

// RetryScheduled reports whether a charge retry is still to come: the run is
// active, not paused, and has attempts left under its policy. A run with
// every attempt used only runs its final action next. That holds whether
// the action waits for the invoice grace, is re-attempted after it failed,
// or the policy's schedule was shortened after the run started. A run read
// without its policy (MaxRetryAttempts 0) reports false: no retry is
// promised.
func (r InvoiceDunningRun) RetryScheduled() bool {
	return r.State == DunningActive && !r.Paused && r.AttemptCount < r.MaxRetryAttempts
}

// There is no CustomerDunningOverride (ADR-036). A partial per-customer
// override (the retry count, the first wait and the terminal action, with
// the rest of the schedule inherited) has no industry precedent. Stripe,
// Lago, Orb and Recurly all assign whole named templates. A customer gets
// its own schedule through `customers.dunning_policy_id`, which names a
// DunningPolicy row.

// InvoiceDunningEvent is one entry in a dunning run's history.
type InvoiceDunningEvent struct {
	ID           string           `json:"id"`
	RunID        string           `json:"run_id"`
	TenantID     string           `json:"tenant_id,omitempty"`
	InvoiceID    string           `json:"invoice_id"`
	EventType    DunningEventType `json:"event_type"`
	State        DunningRunState  `json:"state"`
	Reason       string           `json:"reason,omitempty"`
	AttemptCount int              `json:"attempt_count"`
	Metadata     map[string]any   `json:"metadata,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	// RecordedAt is the real-world instant of the INSERT (ADR-104
	// Invariant A). It is the row's only wall-clock stamp, because CreatedAt
	// follows the entity's calendar, which is simulated during catch-up (the
	// work Hardcount does after a test clock moves forward, ADR-030). It is
	// nil on rows written before migration 0164.
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
}
