package testclock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// BillingRunner is the narrow hook the service uses to drive a billing
// catchup after a clock advance. In production billing.Engine satisfies
// it (via RunCycleForClock); tests can stub with a spy that records calls.
//
// Post-ADR-028 disjoint flows: the catchup uses RunCycleForClock,
// which scopes to subs pinned to ONE clock. The wall-clock cron's
// generic RunCycle is a different code path and never touches
// clock-pinned subs. This separation removes the SKIP-LOCKED race
// and the drip-bill artifact that the dual-flow design produced.
//
// The per-sub period loop in billSubscription handles multi-year
// catchup internally — one call per sub processes all due periods.
type BillingRunner interface {
	RunCycleForClock(ctx context.Context, tenantID, clockID string, batchSize int) (int, []error)

	// RetryPendingChargesForClock retries auto-charge attempts on
	// clock-pinned invoices flagged auto_charge_pending=true. Called
	// during catchup AFTER period generation, so customers who attached
	// a payment method since the last advance get charged as part of
	// THIS advance, not on a future wall-clock tick. ADR-029 Phase 1.
	RetryPendingChargesForClock(ctx context.Context, tenantID, clockID string, limit int) (int, []error)

	// ScanThresholdsForClock fires early-invoice (Stripe-parity billing-
	// thresholds) invoices for clock-pinned subs whose running cycle
	// subtotal has crossed a configured threshold. Runs during catchup
	// after period generation; without it, the wall-clock cron would
	// (incorrectly) fire threshold invoices on clock-pinned subs.
	// ADR-029 Phase 3.
	ScanThresholdsForClock(ctx context.Context, tenantID, clockID string, batchSize int) (int, []error)
}

// CustomerReader is the narrow hook the service uses to list customers
// pinned to a clock. The customer domain owns the customers table —
// including the encrypt-at-rest wrapper around display_name / email —
// so testclock reads through this interface instead of joining the
// table directly. That way every caller of the "attached customers"
// surface gets the same decrypted view that every other read path
// (customer detail page, customer list, etc.) does.
//
// Per-domain rule (docs/architecture.md, "Design rules"): zero cross-domain
// imports between peer packages; coordination via narrow interfaces only.
type CustomerReader interface {
	ListByTestClockID(ctx context.Context, tenantID, clockID string, limit, offset int) ([]domain.Customer, int, error)
}

// SubscriptionLister returns the subscriptions pinned to a clock in the same
// shape as every other subscription read (name, billing time, items and the
// rest). Implemented by the subscription store; the clock's own narrow scan
// left most fields empty.
type SubscriptionLister interface {
	ListByTestClock(ctx context.Context, tenantID, clockID string, limit, offset int) ([]domain.Subscription, int, error)
}

// TrialExpirer is the narrow hook the catchup orchestrator uses to
// flip trialing subs whose `trial_end_at` has elapsed in sim time to
// active — at `trial_end_at`, not at the later cycle close.
// Implemented by *subscription.Service via ProcessExpiredTrialsForClock.
// Runs as Phase 0.5 (before cycle billing) so by the time Phase 1
// reads the sub list, the status field reflects the actual lifecycle
// state. Without this phase, status stays 'trialing' for the gap
// between trial_end_at and the first chargeable cycle close (up to
// ~30 days for calendar billing).
type TrialExpirer interface {
	ProcessExpiredTrialsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) (int, []error)
}

// PauseResumer is the narrow hook the catchup orchestrator uses to
// auto-resume subs whose pause_collection_resumes_at has elapsed.
// Implemented by *subscription.Service via
// ProcessExpiredPauseCollectionsForClock. Runs as Phase 1.2, after cycle
// billing: a period that ended before resumes_at closes as a draft
// first, then the pause clears in the same advance. Without this phase a
// sub whose next_billing_at is still in the future stays paused past
// resumes_at.
type PauseResumer interface {
	ProcessExpiredPauseCollectionsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) (int, []error)
}

// TaxRetrier is the narrow hook the catchup orchestrator uses to drive
// the per-clock tax-retry phase. Implemented by *invoice.Service via
// RetryPendingTaxForClock. ADR-029 Phase 2.
//
// Returns (processed_count, per-row errors) — same shape as every
// other phase callback so the orchestrator can collect failures
// uniformly.
type TaxRetrier interface {
	RetryPendingTaxForClock(ctx context.Context, tenantID, clockID string, batch int) (int, []error)
}

// ClawbackRetrier is the narrow hook the catchup orchestrator uses to issue a
// clock's DEFERRED simulated clawback drafts — a downgrade clawback whose source
// invoice was in-flight (ADR-059 defer) when created and has since settled.
// Implemented by *creditnote.Service via RetryPendingClawbackIssueForClock.
// Takes frozenTime so Issue() stamps the credit note and its ledger / tax
// effects in simulated time. ADR-029 disjoint flows.
type ClawbackRetrier interface {
	RetryPendingClawbackIssueForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, batch int) (int, []error)
}

// CreditExpirer is the narrow hook the catchup orchestrator uses to
// expire grants belonging to clock-pinned customers. ADR-029 Phase 4.
// Takes frozenTime explicitly so the per-grant `expires_at < now`
// comparison happens in simulated time, not wall-clock.
type CreditExpirer interface {
	ExpireCreditsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) (int, []error)
}

// DunningProcessor is the narrow hook the catchup orchestrator uses to
// advance dunning state for clock-pinned invoices. ADR-029 Phase 5.
// Takes frozenTime explicitly so the per-run next_action_at compare
// runs in simulated time.
type DunningProcessor interface {
	ProcessDueRunsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, limit int) (int, []error)
}

// AuditEmitter writes an audit row on the caller's transaction (ADR-090).
// Implemented by *audit.Logger.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// Service provides the test-clock API surface. Advance and Retry advance only
// mark the clock; the testclock_catchup leader runs the catch-up on its tick
// (catchup.go, ADR-140), off the request path.
type Service struct {
	store         Store
	billing       BillingRunner
	customers     CustomerReader
	subs          SubscriptionLister
	trialExpirer  TrialExpirer
	pauseResumer  PauseResumer
	taxRetry      TaxRetrier
	clawback      ClawbackRetrier
	creditExpirer CreditExpirer
	dunning       DunningProcessor
	audit         AuditEmitter
	passDeadline  time.Duration
}

// Deps is everything the service needs. Every field is required: a catch-up
// that skipped a job would leave the clock ready with that job's work undone,
// and an operator's change to a clock is saved only with its audit row.
type Deps struct {
	// Billing closes the clock's due periods, collects their invoices and
	// fires spend thresholds.
	Billing BillingRunner
	// Customers and Subs read the clock's customers and subscriptions for
	// its detail page.
	Customers CustomerReader
	Subs      SubscriptionLister
	// Trials ends the trials due by the clock's time; without it a
	// subscription stays trialing until its next period closes.
	Trials TrialExpirer
	// Pauses resumes the collection pauses due by the clock's time.
	Pauses PauseResumer
	// Tax retries tax on the clock's invoices waiting for it. The scheduled
	// retry skips clock invoices.
	Tax TaxRetrier
	// Relief issues the relief credit notes held for the clock's invoices,
	// before collection and again after it. The real-time job skips them.
	Relief ClawbackRetrier
	// Credits expires the clock's credit grants due by its time.
	Credits CreditExpirer
	// Dunning runs the clock's payment retries due by its time. The
	// real-time dunning tick skips clock invoices.
	Dunning DunningProcessor
	// Audit writes the audit row of each create, advance, retry advance and
	// delete, in the transaction that makes the change.
	Audit AuditEmitter
	// PassDeadline bounds one catch-up pass; a pass that runs past it fails
	// the advance. The router passes CatchupPassDeadline.
	PassDeadline time.Duration
}

// NewService panics when a dependency is missing, so a wiring mistake stops
// the server at boot instead of skipping a job on every advance or failing
// every change for want of its audit row.
func NewService(store Store, d Deps) *Service {
	missing := func(name string) { panic("testclock.NewService: " + name + " is required") }
	switch {
	case store == nil:
		missing("store")
	case d.Billing == nil:
		missing("Deps.Billing")
	case d.Customers == nil:
		missing("Deps.Customers")
	case d.Subs == nil:
		missing("Deps.Subs")
	case d.Trials == nil:
		missing("Deps.Trials")
	case d.Pauses == nil:
		missing("Deps.Pauses")
	case d.Tax == nil:
		missing("Deps.Tax")
	case d.Relief == nil:
		missing("Deps.Relief")
	case d.Credits == nil:
		missing("Deps.Credits")
	case d.Dunning == nil:
		missing("Deps.Dunning")
	case d.Audit == nil:
		missing("Deps.Audit")
	case d.PassDeadline <= 0:
		missing("Deps.PassDeadline")
	}
	return &Service{
		store: store, billing: d.Billing, customers: d.Customers, subs: d.Subs,
		trialExpirer: d.Trials, pauseResumer: d.Pauses, taxRetry: d.Tax,
		clawback: d.Relief, creditExpirer: d.Credits, dunning: d.Dunning,
		audit: d.Audit, passDeadline: d.PassDeadline,
	}
}

// auditRow is the audit row of an operator's change to a clock: a test_clock
// row named after the clock, with the metadata meta builds from the row the
// change wrote. It lands on the clock's own time axis, at the clock's
// frozen_time. After a delete tears the simulation down (ADR-086), the
// audit log is all that is left of it, and these rows say what the operator
// did: created, advanced from and to, and deleted.
func (s *Service) auditRow(ctx context.Context, action string, meta func(domain.TestClock) map[string]any) AuditRow {
	return func(tx *sql.Tx, clk domain.TestClock) error {
		return s.audit.LogInTx(clock.WithSim(ctx, clock.Sim{At: clk.FrozenTime, TestClockID: clk.ID}), tx, audit.Entry{
			Action:        action,
			ResourceType:  "test_clock",
			ResourceID:    clk.ID,
			ResourceLabel: clk.Name,
			Metadata:      meta(clk),
		})
	}
}

type CreateInput struct {
	Name       string    `json:"name"`
	FrozenTime time.Time `json:"frozen_time"`
}

func (s *Service) Create(ctx context.Context, tenantID string, input CreateInput) (domain.TestClock, error) {
	name := strings.TrimSpace(input.Name)
	if len(name) > 200 {
		return domain.TestClock{}, errs.Invalid("name", "must be at most 200 characters")
	}
	if input.FrozenTime.IsZero() {
		return domain.TestClock{}, errs.Required("frozen_time")
	}

	return s.store.Create(ctx, tenantID, domain.TestClock{
		Name:       name,
		FrozenTime: input.FrozenTime.UTC(),
	}, s.auditRow(ctx, domain.AuditActionCreate, func(clk domain.TestClock) map[string]any {
		return map[string]any{"frozen_time": clk.FrozenTime}
	}))
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	return s.store.Get(ctx, tenantID, id)
}

func (s *Service) List(ctx context.Context, tenantID string, limit, offset int) ([]domain.TestClock, int, error) {
	return s.store.List(ctx, tenantID, limit, offset)
}

// Delete tears the clock down with its simulated customers (ADR-086). Its
// audit row names the clock and its time from the row the teardown deletes.
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.store.Delete(ctx, tenantID, id, s.auditRow(ctx, domain.AuditActionDelete, func(domain.TestClock) map[string]any {
		return map[string]any{"action": "teardown"}
	}))
}

// ListSubscriptions returns one page of the subscriptions pinned to the
// given clock, and their total. Verifies the clock exists first so a
// missing-clock id surfaces as 404 rather than an empty list (which would
// look like an empty clock).
func (s *Service) ListSubscriptions(ctx context.Context, tenantID, clockID string, limit, offset int) ([]domain.Subscription, int, error) {
	if _, err := s.store.Get(ctx, tenantID, clockID); err != nil {
		return nil, 0, err
	}
	return s.subs.ListByTestClock(ctx, tenantID, clockID, limit, offset)
}

// ListAttachedCustomers returns one page of the customers pinned to this
// clock, and their total — the Stripe-parity "attached customers" surface
// (ADR-027 Tier 3). 404s if the clock doesn't exist. Reads through the
// customer domain's CustomerReader so display_name / email arrive
// decrypted; see CustomerReader doc.
func (s *Service) ListAttachedCustomers(ctx context.Context, tenantID, clockID string, limit, offset int) ([]domain.Customer, int, error) {
	if _, err := s.store.Get(ctx, tenantID, clockID); err != nil {
		return nil, 0, err
	}
	return s.customers.ListByTestClockID(ctx, tenantID, clockID, limit, offset)
}

type AdvanceInput struct {
	FrozenTime time.Time `json:"frozen_time"`
}

// Advance moves the clock forward to FrozenTime and marks it advancing.
// The testclock_catchup leader then runs the catch-up (catchup.go,
// ADR-140): everything that fell due in the skipped time, for every
// subscription and invoice on the clock. A large jump (e.g. 3 months
// forward on a monthly sub) closes multiple cycles — the billing engine
// loops each sub's periods until it is caught up.
//
// Advance returns as soon as the clock is marked advancing; the leader runs
// the catch-up on a later tick, after any passes ahead of it. The dashboard polls /v1/test-clocks/{id} every
// 1.5s while status === 'advancing' to surface the transition. This matches
// Stripe's Test Clocks shape — the HTTP advance call returns in
// milliseconds, the catchup runs in the background.
//
// State machine:
//
//	ready ──Advance── advancing ──catchup ok── ready
//	                       │
//	                       └──catchup errored── internal_failure
//
// While in advancing, other callers get 409 from the MarkAdvancing CAS; while
// in internal_failure, all further advances are blocked until the tenant
// retries or deletes the clock.
func (s *Service) Advance(ctx context.Context, tenantID, id string, input AdvanceInput) (domain.TestClock, error) {
	if input.FrozenTime.IsZero() {
		return domain.TestClock{}, errs.Required("frozen_time")
	}
	newTime := input.FrozenTime.UTC()

	current, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.TestClock{}, err
	}
	if current.Status != domain.TestClockStatusReady {
		return domain.TestClock{}, errs.InvalidState(fmt.Sprintf("clock is %s, must be ready to advance", current.Status))
	}
	if !newTime.After(current.FrozenTime) {
		return domain.TestClock{}, errs.Invalid("frozen_time", "must be strictly after current frozen_time")
	}
	// Advance window cap (ADR-028 amendment): a single Advance call
	// shifts frozen_time by ≤ 1 year. This is Hardcount's own cap; Stripe
	// allows two intervals of the shortest subscription period, or, on a
	// clock with no subscriptions, up to two years from the clock's
	// initial frozen time. Larger ranges are
	// chunked into successive advances by the operator. Reasons:
	//   - Predictable per-click resource use (operator can't trigger
	//     a multi-decade catchup that exceeds the 10-minute pass
	//     deadline, then has to manually retry from internal_failure).
	//   - Iteration discipline: each chunk's invoices/dunning state
	//     is reviewable before the next advance.
	//   - Failure isolation: a bug in year 14 doesn't poison earlier
	//     years' simulated billing.
	// AddDate(1,0,0) handles leap years correctly (Mar 1 → next Feb 29
	// or Mar 1 depending on year). Constant year-arithmetic, not a
	// fixed nanosecond duration.
	maxAllowed := current.FrozenTime.AddDate(1, 0, 0)
	if newTime.After(maxAllowed) {
		return domain.TestClock{}, errs.Invalid("frozen_time",
			"advance cannot exceed 1 year per call — chunk longer ranges into successive advances")
	}

	// The audit row records the window the advance swept, so the events it
	// causes can be traced to it. advanced_from is the time MarkAdvancing
	// replaced, read by the same statement.
	return s.store.MarkAdvancing(ctx, tenantID, id, newTime, s.auditRow(ctx, domain.AuditActionUpdate, func(clk domain.TestClock) map[string]any {
		return map[string]any{
			"action":        "advanced",
			"frozen_time":   clk.FrozenTime,
			"advanced_from": clk.LastAdvanceSummary.AdvancedFrom,
		}
	}))
}

// RetryAdvance resumes a clock parked in status='internal_failure'
// from a prior catchup error. Stripe-parity recovery — the catchup
// loop is idempotent (only processes subs with next_billing_at <=
// frozen_time), so resuming from where the previous attempt
// stopped is safe. Frozen_time stays at its current value; the
// operator's earlier Advance input is preserved by virtue of
// MarkAdvancing already having stamped frozen_time before the
// failure. ADR-018.
//
// Like Advance, it only marks the clock advancing again; the
// testclock_catchup leader runs the catch-up on its next tick.
//
// Refuses to retry from any state other than internal_failure
// with a 409. A clock currently in 'advancing' is already being
// caught up; a clock in 'ready' has no failure to retry.
func (s *Service) RetryAdvance(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	current, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.TestClock{}, err
	}
	if current.Status != domain.TestClockStatusInternalFailed {
		return domain.TestClock{}, errs.InvalidState(fmt.Sprintf(
			"retry only valid on clocks in internal_failure (current: %s)", current.Status))
	}

	return s.store.RetryFromFailed(ctx, tenantID, id, s.auditRow(ctx, domain.AuditActionUpdate, func(clk domain.TestClock) map[string]any {
		return map[string]any{"action": "retry_advance", "frozen_time": clk.FrozenTime}
	}))
}

// runClock is one catch-up pass on one advancing clock, at its frozen_time.
// It runs the jobs in order until one fails (runPhases), then completes or
// fails the advance, writing the summary in the same fenced statement.
//
// The pass first moves the clock to the back of the claim list, so a clock
// whose pass keeps being cut off does not hold up the others. It runs under
// Deps.PassDeadline; a pass that overruns it fails the advance with a
// reason that names the clock's time. When ctx itself ends first (a
// shutdown, or the testclock_catchup lease lost: the heartbeat cancels a led
// tick's ctx within leader.AbandonAfter), the pass stops and the clock stays
// advancing for the next leader, which runs the pass again; the phases are
// safe to repeat. A fenced write that changes no row (ErrCatchupFenceLost,
// ErrClockNoLongerAdvancing) ends the run with no error: neither is a
// failure of the advance.
func (s *Service) runClock(ctx context.Context, clk domain.TestClock) (err error) {
	tenantID, clockID := clk.TenantID, clk.ID
	sum := domain.AdvanceSummary{AdvancedTo: clk.FrozenTime}
	if prev := clk.LastAdvanceSummary; prev != nil {
		sum.AdvancedFrom = prev.AdvancedFrom
	}
	// fail ends the advance in internal_failure, with the summary so far, on
	// a context detached from the pass (its deadline may be what ended it)
	// that keeps ctx's values: the livemode pin and the lease token.
	fail := func(reason string) {
		sum.HadErrors = true
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, ferr := s.store.MarkFailed(failCtx, tenantID, clockID, reason, sum); ferr != nil {
			if fencedWriteMissed(ferr) {
				logFencedMiss(ctx, tenantID, clockID, ferr)
				return
			}
			slog.Error("test clock: could not mark the advance failed",
				"clock_id", clockID, "reason", reason, "mark_err", ferr)
		}
	}

	// A panic in a phase must not leave the clock advancing for the next
	// leader to panic on again. The reason stays generic: a panic string
	// carries Go internals the dashboard banner must not show (ADR-026).
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		slog.Error("test-clock catchup panicked",
			"clock_id", clockID, "tenant_id", tenantID, "panic", r, "stack", string(debug.Stack()))
		err = fmt.Errorf("catchup panicked: %v", r)
		if ctx.Err() != nil {
			// The tick ended, which may be what panicked: the next leader
			// runs the pass again.
			logStopped(ctx, tenantID, clockID)
			return
		}
		fail(errs.SanitizeForOperator(fmt.Errorf("internal error during catchup"), clockID))
	}()

	if err := s.store.TouchAdvancing(ctx, tenantID, clockID); err != nil {
		if fencedWriteMissed(err) {
			logFencedMiss(ctx, tenantID, clockID, err)
			return nil
		}
		return fmt.Errorf("start catch-up pass: %w", err)
	}

	passCtx, cancel := context.WithTimeout(ctx, s.passDeadline)
	defer cancel()
	// The pass's ctx carries the clock and its frozen_time (clock.WithSim):
	// every clock.Now(ctx) below reads the simulated time, and every audit
	// row a phase writes lands on the sim axis (ADR-090 §5). Those rows are
	// the only record of the advance left after the clock is deleted
	// (ADR-086).
	passCtx = clock.WithSim(passCtx, clock.Sim{At: clk.FrozenTime, TestClockID: clockID})
	failedJob, runErrs := s.runPhases(passCtx, tenantID, clockID, clk.FrozenTime, &sum)

	if ctx.Err() != nil {
		logStopped(ctx, tenantID, clockID)
		return nil
	}
	// The banner gets an operator-safe reason (ADR-026); the log keeps every
	// error in full. When the pass ran out of time, the reason is the
	// deadline alone: the job that met the ended ctx is not the one that took
	// the time.
	var reason, logged []string
	if errors.Is(passCtx.Err(), context.DeadlineExceeded) {
		msg := fmt.Sprintf("the catch-up pass at %s did not finish within %s",
			clk.FrozenTime.UTC().Format(time.RFC3339), s.passDeadline)
		reason, logged = []string{msg}, []string{msg}
	}
	if len(runErrs) > 0 {
		if reason == nil {
			reason = []string{failedJob + ": " + errs.SummarizeForOperator(runErrs, clockID)}
		}
		logged = append(logged, fmt.Sprintf("%s: %v", failedJob, errors.Join(runErrs...)))
	}
	if reason != nil {
		fail(reason[0])
		return fmt.Errorf("catch-up pass failed: %s", strings.Join(logged, "; "))
	}
	if _, err := s.store.CompleteAdvance(ctx, tenantID, clockID, sum); err != nil {
		if fencedWriteMissed(err) {
			logFencedMiss(ctx, tenantID, clockID, err)
			return nil
		}
		return fmt.Errorf("complete advance: %w", err)
	}
	return nil
}

// fencedWriteMissed reports whether err is a fenced catch-up write that
// changed no row: the lease lost, or the clock no longer advancing.
func fencedWriteMissed(err error) bool {
	return errors.Is(err, ErrCatchupFenceLost) || errors.Is(err, ErrClockNoLongerAdvancing)
}

// logFencedMiss records a run that stopped on a fenced miss. INFO, not an
// error: the current leader runs the clock (lease lost), or there is nothing
// left to run (the clock completed, failed or was deleted under the pass).
func logFencedMiss(ctx context.Context, tenantID, clockID string, err error) {
	slog.InfoContext(ctx, "test clock: catch-up stopped without changing the clock",
		"clock_id", clockID, "tenant_id", tenantID, "reason", err)
}

// logStopped records a pass whose tick ended before it finished.
func logStopped(ctx context.Context, tenantID, clockID string) {
	slog.InfoContext(ctx, "test clock: catch-up pass stopped before it finished; the clock stays advancing and the next leader runs the pass again",
		"clock_id", clockID, "tenant_id", tenantID, "cause", context.Cause(ctx))
}

// runPhases runs the catch-up jobs in dependency order on ctx, which
// carries the clock and frozen (clock.WithSim). ADR-028 disjoint flows:
// each job scopes to THIS clock's entities, and the wall-clock cron never
// sees them. ADR-029: every time-aware engine concern that touches
// clock-pinned entities runs here, in lockstep with simulated time. An
// error on one row does not stop the job's other rows. The pass stops at
// the first job that returned an error and returns its name, as
// docs/concepts/test-clocks.md lists the jobs, with the errors: no later job
// acts on state the failed one did not reach, so a failed relief credit note
// stops the card charge that would take the full amount. Retry advance runs
// the pass again from the top; every job is safe to repeat. Counts
// accumulate into sum, the failed job's included.
func (s *Service) runPhases(ctx context.Context, tenantID, clockID string, frozen time.Time, sum *domain.AdvanceSummary) (string, []error) {
	// Phase 0.5 (Bug #8): flip trialing subs to active at
	// trial_end_at when sim time has elapsed past it, BEFORE
	// Phase 1 reads the sub list for cycle billing. Without
	// this, a sub whose trial ended weeks ago in sim time but
	// whose next_billing_at (cycle close) is still future keeps
	// reading as 'trialing' on the dashboard — confusing for
	// operators, semantically wrong vs Stripe / Lago where
	// status flips at trial_end_at.
	//
	// Also fires BillOnCreate per activated sub so in_advance
	// items' first paid period is covered at the activation
	// instant (Bug #6 carry-through).
	trialCount, trialErrs := s.trialExpirer.ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen)
	sum.TrialsActivated = trialCount
	if len(trialErrs) > 0 {
		return "trial ends", trialErrs
	}

	// Phase 1 (ADR-028): generate any newly-due periods. Multi-period
	// catchup happens inside billSubscription's per-sub loop.
	periodCount, periodErrs := s.billing.RunCycleForClock(ctx, tenantID, clockID, 100)
	sum.InvoicesGenerated = periodCount
	if len(periodErrs) > 0 {
		return "billing period closes", periodErrs
	}

	// Phase 1.2: auto-resume any sub whose pause_collection_resumes_at
	// has elapsed in simulated time. It runs AFTER Phase 1: a period
	// that ended before resumes_at was paused at its end and closes as
	// a draft, and the close reads the pause off the row. The store
	// query also skips a sub that still owes such a close (a failed
	// Phase 1), so a later advance resumes it once that period is
	// billed. Before Phase 1.5 so the threshold scan sees a resumed sub.
	pauseCount, pauseErrs := s.pauseResumer.ProcessExpiredPauseCollectionsForClock(ctx, tenantID, clockID, frozen)
	sum.PausesResumed = pauseCount
	if len(pauseErrs) > 0 {
		return "pause ends", pauseErrs
	}

	// Phase 1.5 (ADR-029 Phase 3): threshold scan for early-invoice
	// invoices. Runs after period generation (so the latest
	// running cycle is the one being evaluated) but before tax
	// retry / charge so any threshold-fired invoices get the
	// same downstream treatment as period-generated ones.
	thresholdCount, thresholdErrs := s.billing.ScanThresholdsForClock(ctx, tenantID, clockID, 100)
	sum.ThresholdsFired = thresholdCount
	if len(thresholdErrs) > 0 {
		return "spend thresholds", thresholdErrs
	}

	// Phase 2 (ADR-029): tax retry on clock-pinned invoices left
	// at tax_status=pending with a retryable code. Runs BEFORE
	// Phase 3 (charge) so a successful tax retry unblocks finalize
	// in the same Advance — without this ordering, an invoice that
	// gets unstuck would have to wait for the next advance to fire
	// its charge attempt.
	taxCount, taxErrs := s.taxRetry.RetryPendingTaxForClock(ctx, tenantID, clockID, 100)
	sum.TaxRetried = taxCount
	if len(taxErrs) > 0 {
		return "tax retries", taxErrs
	}

	// Phase 2.9: issue this clock's pending clawback drafts BEFORE the
	// charge phase, as the wall-clock billing run issues relief drafts
	// (clawback_issue) before it collects queued invoices. A cancel or
	// plan change whose relief note failed to issue in its own call left
	// its invoice uncollected (subscription's finishReliefThenCollect);
	// charged first, the card paid the whole of it and the relief landed
	// on the balance after. A draft whose source is still in flight is
	// not listed here; Phase 3.7 issues it once Phase 3 settles it.
	clawbackCount, clawbackErrs := s.clawback.RetryPendingClawbackIssueForClock(ctx, tenantID, clockID, frozen, 100)
	sum.ClawbacksIssued += clawbackCount
	if len(clawbackErrs) > 0 {
		return "relief credit notes", clawbackErrs
	}

	// Phase 3 (ADR-029): collect auto_charge_pending invoices for this
	// clock's subs. A card-less invoice gets its setup-link email and
	// no-payment dunning here too (the collector starts it), so a
	// simulated subscription with no payment method reaches a terminal
	// on a later Advance once simulated time passes the grace window. Customer may have attached a
	// PM via the customer portal between advances — without this
	// hook, neither cron nor catchup would charge until an operator
	// clicked Advance. This call closes that loop.
	chargeCount, chargeErrs := s.billing.RetryPendingChargesForClock(ctx, tenantID, clockID, 100)
	sum.ChargesRetried = chargeCount
	if len(chargeErrs) > 0 {
		return "card charges", chargeErrs
	}

	// Phase 3.7 (ADR-029): issue this clock's DEFERRED simulated clawback
	// drafts. Runs AFTER the charge phase so a source invoice that just
	// settled this advance is terminal by now and its deferred clawback
	// (ADR-059) can re-drive; the wall-clock reconciler skips simulated
	// rows, so this is their only issuer. frozen_time is bound so Issue()
	// stamps the note + its ledger / tax effects in simulated time.
	clawbackCount, clawbackErrs = s.clawback.RetryPendingClawbackIssueForClock(ctx, tenantID, clockID, frozen, 100)
	sum.ClawbacksIssued += clawbackCount
	if len(clawbackErrs) > 0 {
		return "relief credit notes", clawbackErrs
	}

	// Phase 5 (ADR-029): dunning advance for clock-pinned invoices.
	// Runs after the charge/credit-settle phases. A Phase 1/1.5/3
	// credit-cover settle now resolves the invoice's dunning run in place —
	// the engine calls resolveDunningRecovered on a credit-cover settle, and
	// processRun carries a paid-pre-check backstop — so a just-settled
	// invoice's run is resolved, never re-advanced (a credit-cover settle
	// previously left it active, the bug this fixes). next_action_at vs
	// simulated time.
	dunningCount, dunningErrs := s.dunning.ProcessDueRunsForClock(ctx, tenantID, clockID, frozen, 50)
	sum.DunningAdvanced = dunningCount
	if len(dunningErrs) > 0 {
		return "payment retries", dunningErrs
	}

	// Phase 4 (ADR-029): credit expiry for grants belonging to
	// customers pinned to this clock. expires_at compared against
	// simulated time, not wall-clock.
	//
	// It runs last, after Phase 5 (ADR-029 amendment 2026-10-20). Every
	// credit draw judges expiry at its own instant: drainPositiveBlocks
	// takes only blocks with expires_at at or after the draw, and a dunning
	// retry draws at its scheduled instant, at or before frozen.
	// Expiry retires every grant with expires_at before frozen, and a
	// retired grant is never drawn again (ADR-071). Run first, it took a
	// grant that was still live at an earlier retry's instant, and that
	// retry charged the card in full. Run last, no draw can take an
	// expired grant either: the same filter refuses it. A draw that
	// tests expiry at an earlier instant than its own breaks this; the
	// control case of TestCatchup_CreditExpiryRunsAfterDunningRetries_RealStore
	// then fails.
	creditCount, creditErrs := s.creditExpirer.ExpireCreditsForClock(ctx, tenantID, clockID, frozen)
	sum.CreditsExpired = creditCount
	if len(creditErrs) > 0 {
		return "credit expiry", creditErrs
	}
	return "", nil
}
