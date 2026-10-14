package domain

import "time"

// TestClockStatus tracks what a test clock is doing. Mirrors Stripe's API.
//
//   - ready: idle, safe to advance or delete.
//   - advancing: a catchup run is in flight; further advances are rejected
//     until it completes.
//   - internal_failure: the last advance errored partway through; the clock
//     is frozen at an intermediate state and the tenant must inspect
//     (advances and subscription attachment stay blocked).
type TestClockStatus string

const (
	TestClockStatusReady          TestClockStatus = "ready"
	TestClockStatusAdvancing      TestClockStatus = "advancing"
	TestClockStatusInternalFailed TestClockStatus = "internal_failure"
)

// TestClock is a frozen-time simulator scoped to a single tenant in test mode.
// Subscriptions attached to a clock read the clock's FrozenTime instead of
// wall-clock when the billing engine evaluates cycle boundaries, trial ends,
// and dunning retries — this lets integration tests walk through a multi-month
// lifecycle in seconds.
//
// Livemode is always false and is enforced by a CHECK constraint in the DB.
type TestClock struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id,omitempty"`
	Name       string          `json:"name"`
	FrozenTime time.Time       `json:"frozen_time"`
	Status     TestClockStatus `json:"status"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	// LastFailureReason is the error captured when an advance
	// transitioned the clock to status='internal_failure'. Cleared
	// on the next successful advance. ADR-018.
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	// LastAdvanceSummary records what the most recent advance produced —
	// the per-phase counts the billing catchup ran in simulated time. The
	// dashboard renders it as a "Last advance results" card once the clock
	// polls back to status='ready', so the operator sees what an advance
	// did without hunting across the Invoices / Audit / Dunning pages. Nil
	// until the clock has been advanced with billing wired. Overwritten on
	// each advance (success or partial failure).
	LastAdvanceSummary *AdvanceSummary `json:"last_advance_summary,omitempty"`
}

// AdvanceSummary is the compact, operator-facing record of one test-clock
// advance: the simulated span it covered and how many of each billing
// artifact the catchup produced. The counts come from the catchup phases
// themselves (each already returns a processed count), so they are exact —
// not derived from a time-window query. Stored as JSONB on the clock row.
type AdvanceSummary struct {
	// AdvancedFrom / AdvancedTo are the simulated frozen-time span this
	// advance covered (AdvancedTo is the new frozen_time). On a retry of a
	// failed advance the two can be equal (the original "from" was lost when
	// the failed advance overwrote frozen_time); the dashboard shows only the
	// destination in that case.
	AdvancedFrom time.Time `json:"advanced_from"`
	AdvancedTo   time.Time `json:"advanced_to"`
	// InvoicesGenerated is the number of cycle/proration invoices the period
	// generation phase created this advance (Phase 1).
	InvoicesGenerated int `json:"invoices_generated"`
	// TrialsActivated is the number of subscriptions whose trial ended in the
	// simulated span and flipped trialing → active (Phase 0.5).
	TrialsActivated int `json:"trials_activated"`
	// PausesResumed is the number of paused subscriptions whose scheduled
	// resume time elapsed and auto-resumed (Phase 1.2).
	PausesResumed int `json:"pauses_resumed"`
	// ThresholdsFired is the number of billing-threshold invoices
	// issued this advance (Phase 1.5).
	ThresholdsFired int `json:"thresholds_fired"`
	// TaxRetried is the number of tax-pending invoices whose tax retry ran
	// this advance (Phase 2).
	TaxRetried int `json:"tax_retried"`
	// ChargesRetried is the number of auto-charge-pending invoices the charge
	// retry phase attempted this advance (Phase 3).
	ChargesRetried int `json:"charges_retried"`
	// ClawbacksIssued is the number of deferred simulated clawback credit-note
	// drafts issued this advance (Phase 3.7, ADR-029).
	ClawbacksIssued int `json:"clawbacks_issued"`
	// CreditsExpired is the number of credit grants that expired in the
	// simulated span (Phase 4).
	CreditsExpired int `json:"credits_expired"`
	// DunningAdvanced is the number of dunning runs that advanced a step this
	// advance (Phase 5).
	DunningAdvanced int `json:"dunning_advanced"`
	// HadErrors is true when at least one phase errored — the advance landed
	// in internal_failure and this summary reflects only the work that
	// completed before the error.
	HadErrors bool `json:"had_errors"`
}
