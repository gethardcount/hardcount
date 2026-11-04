package domain

import "time"

// TestClockStatus tracks what a test clock is doing. It mirrors Stripe's API.
// A catch-up is the run that does everything that fell due in the time an
// advance skipped.
//
//   - ready: idle, so it can be advanced.
//   - advancing: a catch-up run is in flight. Advance and RetryAdvance are
//     refused (409) until it completes. frozen_time is already the new time.
//   - internal_failure: the last catch-up failed partway, so the clock is at
//     its new frozen_time with the catch-up unfinished. Advance is refused
//     (409). POST /retry-advance resumes the catch-up (ADR-018), or the
//     tenant deletes the clock.
//
// No status blocks attaching a customer. A customer created on the clock
// joins it at its frozen_time in any status: ResolveSim (testclock store)
// checks only that the clock exists. While the clock is advancing or failed,
// frozen_time is already the new time, so the customer joins as one created
// just after the advance would.
type TestClockStatus string

const (
	TestClockStatusReady          TestClockStatus = "ready"
	TestClockStatusAdvancing      TestClockStatus = "advancing"
	TestClockStatusInternalFailed TestClockStatus = "internal_failure"
)

// TestClock is a frozen-time simulator scoped to a single tenant in test mode.
// Subscriptions attached to a clock read the clock's FrozenTime instead of
// the wall clock when the billing engine evaluates cycle boundaries, trial
// ends and dunning retries. So an integration test can walk through a
// multi-month lifecycle in seconds.
//
// Livemode is always false, and a CHECK constraint in the database enforces
// it.
type TestClock struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id,omitempty"`
	Name       string          `json:"name"`
	FrozenTime time.Time       `json:"frozen_time"`
	Status     TestClockStatus `json:"status"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	// LastFailureReason is the error captured when an advance moved the
	// clock to status='internal_failure'. The next successful advance clears
	// it (ADR-018).
	LastFailureReason string `json:"last_failure_reason,omitempty"`
	// LastAdvanceSummary records what the most recent advance produced: the
	// per-phase counts of the billing catch-up that ran in simulated time.
	// The dashboard shows it as a "Last advance results" card once the clock
	// polls back to status='ready'. So the operator sees what an advance did
	// without searching the Invoices, Audit and Dunning pages. It is nil
	// until the clock has been advanced with billing wired. Each advance
	// overwrites it, whether the advance succeeded or partly failed.
	LastAdvanceSummary *AdvanceSummary `json:"last_advance_summary,omitempty"`
}

// AdvanceSummary is the compact, operator-facing record of one test-clock
// advance: the simulated span it covered, and how many of each billing
// artifact the catch-up produced. The counts come from the catch-up phases
// themselves, since each already returns a processed count. So they are
// exact, and not derived from a time-window query. It is stored as JSONB on
// the clock row.
type AdvanceSummary struct {
	// AdvancedFrom and AdvancedTo are the simulated frozen-time span this
	// advance covered (AdvancedTo is the new frozen_time). On a retry of a
	// failed advance the two can be equal, because the original "from" was
	// lost when the failed advance overwrote frozen_time. The dashboard then
	// shows only the destination.
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
	// HadErrors is true when at least one phase failed. The advance then
	// ended in internal_failure, and this summary shows only the work that
	// completed before the error.
	HadErrors bool `json:"had_errors"`
}
