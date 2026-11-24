package dunning

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type memStore struct {
	// Multi-policy-per-tenant memStore. Tests can mutate policies map
	// directly or via the upsert/set-default methods. defaultID points
	// at the is_default=true row (exactly one per fixture).
	policies  map[string]domain.DunningPolicy
	defaultID string
	runs      map[string]domain.InvoiceDunningRun
	events    []domain.InvoiceDunningEvent
	hookErrs  []error // errors returned by UpdateRunIfActive `then` hooks (non-vetoing)
	// started records the dunning.started payloads StartRun enqueued (the
	// PostgresStore's in-tx outbox row).
	started []map[string]any
	// webhooks records the dunning.escalated and dunning.resolved events
	// EscalateRun and ResolveRun queue when their write applies, as the
	// PostgresStore queues them in the write's transaction.
	webhooks webhookRecorder
	startErr error // when set, StartRun fails and writes nothing (all-or-none)
	// awaiting is each run's awaiting_payment_intent_id (#1289).
	awaiting map[string]string
	// paymentMoved marks an invoice whose payment left flight before the
	// in-flight write: RecordRetryInFlight then writes nothing, as the real
	// store does when the invoice no longer waits on the PaymentIntent.
	paymentMoved map[string]bool
}

func newMemStore() *memStore {
	policy := domain.DunningPolicy{
		ID: "dpol_1", TenantID: "t1", Name: "Default",
		Enabled: true, IsDefault: true,
		RetryDays:               []int{3, 3, 5},
		FinalSubscriptionAction: domain.SubActionNone,
		FinalInvoiceAction:      domain.InvActionNone,
	}
	return &memStore{
		policies:     map[string]domain.DunningPolicy{"dpol_1": policy},
		defaultID:    "dpol_1",
		runs:         make(map[string]domain.InvoiceDunningRun),
		awaiting:     make(map[string]string),
		paymentMoved: make(map[string]bool),
	}
}

// RecordRetryInFlight mirrors the real store: nothing when the payment left
// flight, else UpdateRunIfActive's guard, then the run, its awaited
// PaymentIntent and its event together.
func (m *memStore) RecordRetryInFlight(_ context.Context, _ string, run domain.InvoiceDunningRun, expectedAttempts int, paymentIntentID string, event domain.InvoiceDunningEvent) (RetryInFlightResult, error) {
	if paymentIntentID == "" {
		return RetryInFlightRunMoved, fmt.Errorf("no PaymentIntent id")
	}
	if err := refuseStrandingWrite(run); err != nil {
		return RetryInFlightRunMoved, err
	}
	if m.paymentMoved[run.InvoiceID] {
		return RetryInFlightPaymentMoved, nil
	}
	existing, ok := m.runs[run.ID]
	if ok && (existing.State != domain.DunningActive || existing.AttemptCount != expectedAttempts) {
		return RetryInFlightRunMoved, nil
	}
	m.runs[run.ID] = run
	m.awaiting[run.ID] = paymentIntentID
	_, _ = m.CreateEvent(context.Background(), "", event)
	return RetryInFlightRecorded, nil
}

func (m *memStore) TakeAwaitedRetryTx(_ context.Context, _ *sql.Tx, invoiceID, paymentIntentID string) (domain.InvoiceDunningRun, domain.DunningPolicy, bool, error) {
	if paymentIntentID == "" {
		return domain.InvoiceDunningRun{}, domain.DunningPolicy{}, false, nil
	}
	for id, r := range m.runs {
		if r.InvoiceID == invoiceID && r.State == domain.DunningActive && m.awaiting[id] == paymentIntentID {
			delete(m.awaiting, id)
			p, ok := m.policies[r.PolicyID]
			if !ok {
				return domain.InvoiceDunningRun{}, domain.DunningPolicy{}, false, errs.ErrNotFound
			}
			return r, p, true, nil
		}
	}
	return domain.InvoiceDunningRun{}, domain.DunningPolicy{}, false, nil
}

func (m *memStore) RecordRetryFailureTx(_ context.Context, _ *sql.Tx, run domain.InvoiceDunningRun, event domain.InvoiceDunningEvent, then func(tx *sql.Tx) error) error {
	if run.NextActionAt == nil {
		return fmt.Errorf("no next_action_at")
	}
	existing, ok := m.runs[run.ID]
	if !ok || existing.State != domain.DunningActive || existing.AttemptCount != run.AttemptCount {
		return fmt.Errorf("run changed under its lock")
	}
	existing.NextActionAt = run.NextActionAt
	m.runs[run.ID] = existing
	_, _ = m.CreateEvent(context.Background(), "", event)
	if then != nil {
		if err := then(nil); err != nil {
			m.hookErrs = append(m.hookErrs, err)
		}
	}
	return nil
}

func (m *memStore) GetPolicyByID(_ context.Context, _ string, id string) (domain.DunningPolicy, error) {
	p, ok := m.policies[id]
	if !ok {
		return domain.DunningPolicy{}, errs.ErrNotFound
	}
	return p, nil
}

func (m *memStore) GetDefaultPolicy(_ context.Context, _ string) (domain.DunningPolicy, error) {
	if m.defaultID == "" {
		return domain.DunningPolicy{}, errs.ErrNotFound
	}
	return m.policies[m.defaultID], nil
}

func (m *memStore) ListPolicies(_ context.Context, _ string) ([]domain.DunningPolicy, error) {
	out := make([]domain.DunningPolicy, 0, len(m.policies))
	// Default first, then insertion order (map iteration nondeterministic
	// for the non-default rows — tests that need ordering set IDs).
	if m.defaultID != "" {
		out = append(out, m.policies[m.defaultID])
	}
	for id, p := range m.policies {
		if id == m.defaultID {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (m *memStore) DeletePolicy(_ context.Context, _, id string) error {
	if _, ok := m.policies[id]; !ok {
		return errs.ErrNotFound
	}
	if id == m.defaultID {
		return errs.InvalidState("cannot delete default policy")
	}
	delete(m.policies, id)
	return nil
}

func (m *memStore) SetDefaultPolicy(_ context.Context, _, id string) error {
	p, ok := m.policies[id]
	if !ok {
		return errs.ErrNotFound
	}
	if cur, ok := m.policies[m.defaultID]; ok {
		cur.IsDefault = false
		m.policies[m.defaultID] = cur
	}
	p.IsDefault = true
	m.policies[id] = p
	m.defaultID = id
	return nil
}

func (m *memStore) CountCustomersOnPolicy(_ context.Context, _, _ string) (int, error) {
	// memStore has no customer table; tests that want a specific
	// count override this via a wrapper if needed.
	return 0, nil
}

func (m *memStore) UpsertPolicy(_ context.Context, tenantID string, p domain.DunningPolicy) (domain.DunningPolicy, error) {
	if p.ID == "" {
		p.ID = fmt.Sprintf("dpol_%d", len(m.policies)+1)
	}
	p.TenantID = tenantID
	if p.IsDefault {
		// Caller can't set default via Upsert (matches postgres behaviour).
		p.IsDefault = m.policies[p.ID].IsDefault
	}
	m.policies[p.ID] = p
	return p, nil
}

// SeedDefaultPoliciesTx records the policy it was handed (one mode only —
// the in-memory store has no modes) so a unit test can check what the
// service passed down; the per-mode writes are pinned against Postgres.
func (m *memStore) SeedDefaultPoliciesTx(_ context.Context, _ *sql.Tx, tenantID string, p domain.DunningPolicy, onSeeded func(domain.DunningPolicy, bool) error) error {
	p.ID = fmt.Sprintf("dpol_%d", len(m.policies)+1)
	p.TenantID = tenantID
	p.IsDefault = true
	p.Seeded = true
	m.policies[p.ID] = p
	if onSeeded != nil {
		return onSeeded(p, false)
	}
	return nil
}

func (m *memStore) CreateRun(_ context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	run.ID = fmt.Sprintf("drun_%d", len(m.runs)+1)
	run.TenantID = tenantID
	// Honor caller-provided CreatedAt to match PostgresStore.CreateRun
	// (postgres.go — it likewise falls back to now only when CreatedAt is
	// zero). Dunning Service stamps CreatedAt in the
	// effective-now domain so engine-triggered runs on a clock-pinned
	// invoice carry simulated time, not wall-clock.
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	run.UpdatedAt = run.CreatedAt
	m.runs[run.ID] = run
	return run, nil
}

// StartRun mirrors PostgresStore.StartRun: the run, its started event and the
// dunning.started outbox row land together, or nothing does.
func (m *memStore) StartRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	if m.startErr != nil {
		return domain.InvoiceDunningRun{}, m.startErr
	}
	run, _ = m.CreateRun(ctx, tenantID, run)
	_, _ = m.CreateEvent(ctx, tenantID, domain.InvoiceDunningEvent{
		RunID: run.ID, InvoiceID: run.InvoiceID, EventType: domain.DunningEventStarted,
		State: run.State, Reason: run.Reason, CreatedAt: run.CreatedAt,
	})
	m.started = append(m.started, map[string]any{
		"run_id": run.ID, "invoice_id": run.InvoiceID, "customer_id": run.CustomerID,
	})
	return run, nil
}

func (m *memStore) GetRun(_ context.Context, _, id string) (domain.InvoiceDunningRun, error) {
	r, ok := m.runs[id]
	if !ok {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return r, nil
}

func (m *memStore) GetActiveRunByInvoice(_ context.Context, _, invoiceID string) (domain.InvoiceDunningRun, error) {
	for _, r := range m.runs {
		// Mirror the store: exclude only resolved runs. Escalated runs are
		// still returnable so they can be resolved on out-of-band payment.
		if r.InvoiceID == invoiceID && r.State != domain.DunningResolved {
			return r, nil
		}
	}
	return domain.InvoiceDunningRun{}, errs.ErrNotFound
}

func (m *memStore) GetRunByInvoice(_ context.Context, _, invoiceID string) (domain.InvoiceDunningRun, error) {
	for _, r := range m.runs {
		if r.InvoiceID == invoiceID {
			return r, nil
		}
	}
	return domain.InvoiceDunningRun{}, errs.ErrNotFound
}

func (m *memStore) ListRuns(_ context.Context, filter RunListFilter) ([]domain.InvoiceDunningRun, int, error) {
	var result []domain.InvoiceDunningRun
	for _, r := range m.runs {
		if filter.State != "" && string(r.State) != filter.State {
			continue
		}
		result = append(result, r)
	}
	return result, len(result), nil
}

// ResolveRun mirrors the store's CAS: apply the resolved fields only if the row is
// not already resolved, and report whether this call won the transition.
func (m *memStore) ResolveRun(_ context.Context, _ string, run domain.InvoiceDunningRun, fromActiveOnly bool) (bool, error) {
	if existing, ok := m.runs[run.ID]; ok && (existing.State == domain.DunningResolved ||
		(fromActiveOnly && existing.State != domain.DunningActive)) {
		return false, nil
	}
	m.runs[run.ID] = run
	delete(m.awaiting, run.ID)
	m.webhooks.record(domain.EventDunningResolved, domain.DunningRunWebhookData(run, nil))
	return true, nil
}

// UpdateRunIfActive mirrors the guarded update: apply the run's fields only if the row
// is not already resolved, and report whether it applied.
// UpdateRunIfActive mirrors the REAL store's full predicate — state guard AND
// attempt-count CAS — rather than stubbing "true": a fake that always applies
// would keep every stale-processor test green forever (fake-fidelity rule).
func (m *memStore) UpdateRunIfActive(_ context.Context, _ string, run domain.InvoiceDunningRun, expectedAttempts int, then func(tx *sql.Tx) error) (bool, error) {
	return m.writeIfActive(run, expectedAttempts, nil, then)
}

// EscalateRun mirrors the real store: UpdateRunIfActive's guarded write, and
// dunning.escalated queued only when it applies.
func (m *memStore) EscalateRun(_ context.Context, _ string, run domain.InvoiceDunningRun, expectedAttempts int, extra map[string]any, then func(tx *sql.Tx) error) (bool, error) {
	if run.State != domain.DunningEscalated {
		return false, fmt.Errorf("EscalateRun with a run in state %q", run.State)
	}
	return m.writeIfActive(run, expectedAttempts, func() {
		m.webhooks.record(domain.EventDunningEscalated, domain.DunningRunWebhookData(run, extra))
	}, then)
}

// EscalateRunTx mirrors the real store: EscalateRun's guarded write, with the
// escalated timeline row written with it, as the real store inserts it on the
// write-off's transaction.
func (m *memStore) EscalateRunTx(_ context.Context, _ *sql.Tx, _ string, run domain.InvoiceDunningRun, expectedAttempts int, extra map[string]any, event domain.InvoiceDunningEvent, then func(tx *sql.Tx) error) (bool, error) {
	if run.State != domain.DunningEscalated {
		return false, fmt.Errorf("EscalateRunTx with a run in state %q", run.State)
	}
	return m.writeIfActive(run, expectedAttempts, func() {
		m.webhooks.record(domain.EventDunningEscalated, domain.DunningRunWebhookData(run, extra))
		_, _ = m.CreateEvent(context.Background(), "", event) // the in-memory insert cannot fail
	}, then)
}

func (m *memStore) writeIfActive(run domain.InvoiceDunningRun, expectedAttempts int, onWrite func(), then func(tx *sql.Tx) error) (bool, error) {
	if err := refuseStrandingWrite(run); err != nil {
		return false, err
	}
	existing, ok := m.runs[run.ID]
	if ok && existing.State != domain.DunningActive {
		return false, nil
	}
	if ok && existing.AttemptCount != expectedAttempts {
		return false, nil
	}
	m.runs[run.ID] = run
	delete(m.awaiting, run.ID) // every write but the in-flight one clears it
	if onWrite != nil {
		onWrite()
	}
	// Mirror the real store: the hook runs only when the write applied, and
	// its error never vetoes (recorded for tests).
	if then != nil {
		if err := then(nil); err != nil {
			m.hookErrs = append(m.hookErrs, err)
		}
	}
	return true, nil
}

// ClaimExhaustion mirrors the real store's predicate exactly.
func (m *memStore) ClaimExhaustion(_ context.Context, _, runID string, expectedAttempts int, expectedNext, lease time.Time) (bool, error) {
	existing, ok := m.runs[runID]
	if !ok || existing.State != domain.DunningActive || existing.AttemptCount != expectedAttempts ||
		existing.NextActionAt == nil || !existing.NextActionAt.Equal(expectedNext) {
		return false, nil
	}
	existing.NextActionAt = &lease
	m.runs[runID] = existing
	return true, nil
}

func (m *memStore) ListDueRuns(_ context.Context, _ string, before time.Time, limit int) ([]domain.InvoiceDunningRun, error) {
	var result []domain.InvoiceDunningRun
	for _, r := range m.runs {
		if r.NextActionAt != nil && !r.NextActionAt.After(before) && !r.Paused &&
			r.State != domain.DunningResolved && r.State != domain.DunningEscalated {
			result = append(result, r)
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *memStore) CreateEvent(_ context.Context, _ string, event domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error) {
	event.ID = fmt.Sprintf("devt_%d", len(m.events)+1)
	m.events = append(m.events, event)
	return event, nil
}

func (m *memStore) ListEvents(_ context.Context, _, runID string) ([]domain.InvoiceDunningEvent, error) {
	var result []domain.InvoiceDunningEvent
	for _, e := range m.events {
		if e.RunID == runID {
			result = append(result, e)
		}
	}
	return result, nil
}

func (m *memStore) GetStats(_ context.Context, tenantID string) (Stats, error) {
	var s Stats
	for _, r := range m.runs {
		if r.TenantID != tenantID {
			continue
		}
		switch r.State {
		case "active":
			s.ActiveCount++
		case "escalated":
			s.EscalatedCount++
		case "resolved":
			s.ResolvedCount++
		}
		// At-risk: mem mock doesn't carry invoice amount_due,
		// service-layer tests don't assert the sum. Postgres impl
		// is verified via integration test.
	}
	return s, nil
}

type noopRetrier struct{}

func (n *noopRetrier) RetryPayment(_ context.Context, _, _, _ string) error { return nil }

type failingRetrier struct{}

func (f *failingRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("payment declined")
}

func TestStartDunning(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	t.Run("creates run", func(t *testing.T) {
		run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if run.InvoiceID != "inv_1" {
			t.Errorf("invoice_id: got %q, want inv_1", run.InvoiceID)
		}
		if run.State != domain.DunningActive {
			t.Errorf("state: got %q, want scheduled", run.State)
		}
		if run.NextActionAt == nil {
			t.Error("next_action_at should be set from retry schedule")
		}
		if run.PolicyID != "dpol_1" {
			t.Errorf("policy_id: got %q, want dpol_1", run.PolicyID)
		}
	})

	t.Run("idempotent — returns existing", func(t *testing.T) {
		run2, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should return the same run, not create a new one
		if len(store.runs) != 1 {
			t.Errorf("expected 1 run, got %d", len(store.runs))
		}
		if run2.InvoiceID != "inv_1" {
			t.Errorf("should return existing run")
		}
	})

	t.Run("records start event", func(t *testing.T) {
		found := false
		for _, e := range store.events {
			if e.EventType == domain.DunningEventStarted {
				found = true
			}
		}
		if !found {
			t.Error("should record dunning_started event")
		}
	})
}

// TestStartDunning_FirstRetryAnchor: the first retry is grace_period_days
// after the moment the failure is recorded, so an invoice declined days after
// its issue still gets its grace.
func TestStartDunning_FirstRetryAnchor(t *testing.T) {
	grace := 3 * 24 * time.Hour // newMemStore's default policy
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore(), &noopRetrier{}, clock.NewFake(now))
	run, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if want := now.Add(grace); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("next_action_at: got %v, want %v (decline + grace)", run.NextActionAt, want)
	}
	if !run.CreatedAt.Equal(now) {
		t.Errorf("created_at: got %v, want %v", run.CreatedAt, now)
	}
}

func TestStartDunning_DisabledPolicy(t *testing.T) {
	store := newMemStore()
	p := store.policies[store.defaultID]
	p.Enabled = false
	store.policies[store.defaultID] = p
	svc := NewService(store, &noopRetrier{}, nil)

	_, err := svc.StartDunning(context.Background(), "t1", "inv_2", "cus_1", domain.DunningCausePaymentFailed)
	if err == nil {
		t.Fatal("expected error when dunning is disabled")
	}
}

// TestStartDunning_NoPolicyConfigured locks the resilience seam (Finding 2):
// a tenant with NO effective/default policy must yield a DELIBERATE-SKIP
// ErrInvalidState (the same class as disabled) — NOT a raw ErrNotFound — so the
// money-path enrollment sweep swallows it instead of poisoning the catchup.
// Mutation guard: before the fix this returned a fmt.Errorf wrapping ErrNotFound
// (errors.Is(err, ErrNotFound) == true), which the adapter propagated as a
// sweep error.
func TestStartDunning_NoPolicyConfigured(t *testing.T) {
	store := newMemStore()
	store.defaultID = "" // no default → GetDefaultPolicy returns ErrNotFound
	svc := NewService(store, &noopRetrier{}, nil)

	_, err := svc.StartDunning(context.Background(), "t1", "inv_3", "cus_1", domain.DunningCausePaymentFailed)
	if err == nil {
		t.Fatal("expected a deliberate-skip error when no policy is configured")
	}
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Errorf("no-policy must map to ErrInvalidState (deliberate skip); got %v", err)
	}
	if errors.Is(err, errs.ErrNotFound) {
		t.Errorf("no-policy must NOT surface raw ErrNotFound (would poison the sweep); got %v", err)
	}
}

func TestProcessDueRuns(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &failingRetrier{}, nil) // Use failing retrier
	ctx := context.Background()

	// Start a run, then make it due
	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
	past := time.Now().UTC().Add(-1 * time.Hour)
	run.NextActionAt = &past
	store.runs[run.ID] = run

	processed, errs := svc.ProcessDueRuns(ctx, "t1", 20)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if processed != 1 {
		t.Errorf("processed: got %d, want 1", processed)
	}

	// Verify run was updated — retry failed so state stays scheduled
	updated := store.runs[run.ID]
	if updated.AttemptCount != 1 {
		t.Errorf("attempt_count: got %d, want 1", updated.AttemptCount)
	}
	if updated.State != domain.DunningActive {
		t.Errorf("state: got %q, want scheduled", updated.State)
	}
}

// TestResolveByInvoice_ResolvesEscalatedRun guards the fix for escalated runs
// being unresolvable: when a customer pays out-of-band AFTER retries exhaust
// (state=escalated), ResolveByInvoice must still transition the run to resolved.
// Pre-fix GetActiveRunByInvoice excluded 'escalated', so the run got stuck.
func TestResolveByInvoice_ResolvesEscalatedRun(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
	run.State = domain.DunningEscalated // retries exhausted
	run.Resolution = domain.ResolutionRetriesExhausted
	store.runs[run.ID] = run

	if err := svc.ResolveByInvoice(ctx, "t1", "inv_1", domain.ResolutionPaymentRecovered); err != nil {
		t.Fatalf("ResolveByInvoice: %v", err)
	}

	updated := store.runs[run.ID]
	if updated.State != domain.DunningResolved {
		t.Errorf("state: got %q, want resolved — an escalated run must resolve on out-of-band payment", updated.State)
	}
	if updated.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("resolution: got %q, want payment_recovered", updated.Resolution)
	}
}

func TestProcessDueRuns_MaxRetriesExhausted(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)

	// Simulate max retries reached
	run.AttemptCount = 3 // equals MaxRetryAttempts
	past := time.Now().UTC().Add(-1 * time.Hour)
	run.NextActionAt = &past
	store.runs[run.ID] = run

	svc.ProcessDueRuns(ctx, "t1", 20)

	updated := store.runs[run.ID]
	if updated.State != domain.DunningEscalated {
		t.Errorf("state: got %q, want escalated (manual_review policy)", updated.State)
	}
	if updated.Resolution != domain.ResolutionRetriesExhausted {
		t.Errorf("resolution: got %q, want retries_exhausted", updated.Resolution)
	}
	if updated.ResolvedAt == nil {
		t.Error("resolved_at should be set")
	}
}

// recordingEmailNotifier captures the per-type email enqueues so
// catchup-walk tests can assert "warnings + escalation actually
// enqueued" instead of just "DB state is escalated."
type recordingEmailNotifier struct {
	warnings     int
	escalations  int
	paymentFails int
	// What the latest warning said (#966): its decline reason, and whether
	// it was the "use a different card" variant.
	lastWarnReason string
	lastWarnAsk    domain.DeclineAsk
}

func (r *recordingEmailNotifier) SendPaymentFailed(context.Context, string, string, []string, string, string, string, string, domain.DeclineAsk) error {
	r.paymentFails++
	return nil
}
func (r *recordingEmailNotifier) SendDunningWarning(_ context.Context, _, _ string, _ []string, _, _ string, _, _ int, _, failureReason, _ string, ask domain.DeclineAsk) error {
	r.warnings++
	r.lastWarnReason, r.lastWarnAsk = failureReason, ask
	return nil
}
func (r *recordingEmailNotifier) SendDunningWarningTx(ctx context.Context, _ *sql.Tx, tenantID, to string, cc []string, customerName, invoiceNumber string, attemptNumber, maxAttempts int, nextRetryDate, failureReason, publicToken string, ask domain.DeclineAsk) error {
	return r.SendDunningWarning(ctx, tenantID, to, cc, customerName, invoiceNumber, attemptNumber, maxAttempts, nextRetryDate, failureReason, publicToken, ask)
}

func (r *recordingEmailNotifier) SendDunningEscalation(context.Context, string, string, []string, string, string, domain.DunningEscalationOutcome, string) error {
	r.escalations++
	return nil
}

func (r *recordingEmailNotifier) SendDunningEscalationTx(ctx context.Context, _ *sql.Tx, tenantID, to string, cc []string, customerName, invoiceNumber string, outcome domain.DunningEscalationOutcome, publicToken string) error {
	return r.SendDunningEscalation(ctx, tenantID, to, cc, customerName, invoiceNumber, outcome, publicToken)
}

type stubCustomerEmail struct{}

func (stubCustomerEmail) GetCustomerEmail(context.Context, string, string) (string, string, []string, error) {
	return "test@hardcount.dev", "Test Co", nil, nil
}

// An invoice paid outside dunning resolves its open run.
func TestResolveByInvoice_ResolvesAnActiveRun(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)

	if err := svc.ResolveByInvoice(ctx, "t1", "inv_1", domain.ResolutionPaymentRecovered); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolved := store.runs[run.ID]
	if resolved.State != domain.DunningResolved {
		t.Errorf("state: got %q, want resolved", resolved.State)
	}
	if resolved.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("resolution: got %q, want payment_succeeded", resolved.Resolution)
	}
	if resolved.ResolvedAt == nil {
		t.Error("resolved_at should be set")
	}
}

// capturingUncollect records calls to MarkUncollectible so the
// cross-flow test below can assert the dunning resolve path actually
// reaches the invoice service. With err set the write-off is refused before
// it reaches then, as the real one is. Otherwise then runs, as inside the
// write-off's transaction, and its error is the write-off's: without that, a
// run would escalate beside a write-off that failed.
type capturingUncollect struct {
	calls []string // invoice IDs
	err   error
}

func (c *capturingUncollect) MarkUncollectible(_ context.Context, _, invoiceID string, then func(*sql.Tx) error) error {
	c.calls = append(c.calls, invoiceID)
	if c.err != nil {
		return c.err
	}
	if then != nil {
		return then(nil)
	}
	return nil
}

func TestUpsertPolicy(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	policy, err := svc.UpsertPolicy(ctx, "t1", domain.DunningPolicy{
		Name:      "Custom",
		Enabled:   true,
		RetryDays: []int{3, 3, 5},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy.Retries() != 3 {
		t.Errorf("retries: got %d, want 3 (the length of retry_days)", policy.Retries())
	}
	// Default flipped from manual_review → pause in migration 0071 so
	// dunning-exhausted subs go into pause_collection.keep_as_draft
	// automatically instead of stacking finalized invoices each cycle.
	// ADR-112 split the axes and kept this default exactly.
	if policy.FinalSubscriptionAction != domain.SubActionPause {
		t.Errorf("default final_subscription_action: got %q, want pause", policy.FinalSubscriptionAction)
	}
	// The invoice half deliberately does NOT default to a write-off, even
	// though Recurly defaults its equivalent on: writing off asserts bad
	// debt, and a machine must not make that assertion unasked (ADR-112).
	if policy.FinalInvoiceAction != domain.InvActionNone {
		t.Errorf("default final_invoice_action: got %q, want none — defaulting to a write-off would assert bad debt on the tenant's behalf", policy.FinalInvoiceAction)
	}

	// No schedule is no policy: there is no default schedule to fall back to.
	if _, err := svc.UpsertPolicy(ctx, "t1", domain.DunningPolicy{Name: "Bad", Enabled: true}); err == nil {
		t.Error("expected an error for a policy with no retry_days")
	}
}

// TestUpsertPolicy_RetryDaysBounds: every entry is a whole number of days
// from 1 to 30 and there are 1 to 15 of them, on every writer. A wait the
// retry path could not schedule must be refused at save (the FLOW TC5 "3d"
// livelock, 2026-07-18, was an entry that saved fine and wedged the run at
// retry time).
func TestUpsertPolicy_RetryDaysBounds(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	fifteen := []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	cases := []struct {
		name    string
		days    []int
		wantErr bool
	}{
		{"none", nil, true},
		{"empty", []int{}, true},
		{"zero wait", []int{3, 0}, true},
		{"negative wait", []int{-3}, true},
		{"over 30 days", []int{3, 31}, true},
		{"sixteen retries", append(append([]int(nil), fifteen...), 1), true},
		{"one day", []int{1}, false},
		{"thirty days", []int{30}, false},
		{"fifteen retries", fifteen, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for writer, upsert := range map[string]func() error{
				"UpsertPolicy": func() error {
					_, err := svc.UpsertPolicy(ctx, "t1", domain.DunningPolicy{Name: "P", Enabled: true, RetryDays: tc.days})
					return err
				},
			} {
				err := upsert()
				if tc.wantErr && (err == nil || errs.Field(err) != "retry_days") {
					t.Errorf("%s %v: got %v, want a retry_days validation error", writer, tc.days, err)
				}
				if !tc.wantErr && err != nil {
					t.Errorf("%s %v: unexpected error: %v", writer, tc.days, err)
				}
			}
		})
	}
}

// TestDunning_StampsTheDecidedTime: every per-invoice timestamp dunning
// writes (next_action_at, last_attempt_at, resolved_at, created_at), and the
// instant a run's start is written on, is the time its caller bound.
func TestDunning_StampsTheDecidedTime(t *testing.T) {
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	bound := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), frozen)

	t.Run("StartDunning stamps frozen", func(t *testing.T) {
		store := &instantStore{memStore: newMemStore()}
		svc := NewService(store, &noopRetrier{}, nil)

		run, err := svc.StartDunning(bound, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !run.CreatedAt.Equal(frozen) {
			t.Errorf("created_at: got %v, want %v (frozen)", run.CreatedAt, frozen)
		}
		// next_action_at = frozen + grace (3 days default).
		if run.NextActionAt == nil {
			t.Fatal("next_action_at should be set")
		}
		wantNext := frozen.Add(72 * time.Hour)
		if !run.NextActionAt.Equal(wantNext) {
			t.Errorf("next_action_at: got %v, want %v (frozen + 3d grace)", *run.NextActionAt, wantNext)
		}
		store.wroteAt(t, "StartRun", frozen)
	})

	t.Run("a late retry is dated at the bound time", func(t *testing.T) {
		// A retry run after its scheduled time (here an hour late) is dated
		// at the bound time, and the next retry counts from it.
		store := newMemStore()
		svc := NewService(store, &failingRetrier{}, nil)
		ctx := bound

		run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
		// Mark run as due so processRun picks it up.
		past := frozen.Add(-1 * time.Hour)
		run.NextActionAt = &past
		store.runs[run.ID] = run

		_, _ = svc.ProcessDueRuns(ctx, "t1", 20)

		updated := store.runs[run.ID]
		if updated.LastAttemptAt == nil {
			t.Fatal("last_attempt_at should be stamped")
		}
		if !updated.LastAttemptAt.Equal(frozen) {
			t.Errorf("last_attempt_at: got %v, want the bound time %v", *updated.LastAttemptAt, frozen)
		}
		// The next retry counts from the attempt.
		if updated.NextActionAt == nil {
			t.Fatal("next_action_at should be re-stamped")
		}
		wantNext := frozen.Add(72 * time.Hour)
		if !updated.NextActionAt.Equal(wantNext) {
			t.Errorf("next_action_at: got %v, want %v (the attempt + 3d retry interval)", *updated.NextActionAt, wantNext)
		}
	})

	t.Run("ResolveByInvoice stamps frozen", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, &noopRetrier{}, nil)
		ctx := bound

		run, _ := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
		if err := svc.ResolveByInvoice(ctx, "t1", "inv_1", domain.ResolutionPaymentRecovered); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resolved := store.runs[run.ID]
		if resolved.ResolvedAt == nil {
			t.Fatal("resolved_at should be set")
		}
		if !resolved.ResolvedAt.Equal(frozen) {
			t.Errorf("resolved_at: got %v, want %v (frozen)", *resolved.ResolvedAt, frozen)
		}
	})
}

// failThenSucceedRetrier declines the first N attempts, then succeeds —
// exercises processRun's success branch mid-schedule.
type failThenSucceedRetrier struct{ failures, calls int }

func (f *failThenSucceedRetrier) RetryPayment(_ context.Context, _, _, _ string) error {
	f.calls++
	if f.calls <= f.failures {
		return fmt.Errorf("payment declined")
	}
	return nil
}

// TestProcessDueRuns_RecoveryWritesItsOwnRetryRow: retry 1 fails and retry 2
// succeeds. The succeeding retry writes its own retry_attempted row, so the
// run does not read "resolved after 2 attempts" with one visible, and the run
// resolves at that retry's time.
func TestProcessDueRuns_RecoveryWritesItsOwnRetryRow(t *testing.T) {
	store := newMemStore()
	failedAt := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	wall := clock.NewFake(failedAt)
	svc := NewService(store, &failThenSucceedRetrier{failures: 1}, wall)
	ctx := context.Background()
	if _, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed); err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	// The policy's retry days are 3, 3, 5.
	retry1At := failedAt.Add(3 * 24 * time.Hour)
	retry2At := retry1At.Add(3 * 24 * time.Hour)
	for _, at := range []time.Time{retry1At, retry2At} {
		wall.Set(at)
		if _, errs := svc.ProcessDueRuns(ctx, "t1", 20); len(errs) > 0 {
			t.Fatalf("tick at %v: %v", at, errs)
		}
	}

	var run domain.InvoiceDunningRun
	for _, r := range store.runs {
		run = r
	}
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
		t.Fatalf("run: state=%q resolution=%q, want resolved/payment_recovered", run.State, run.Resolution)
	}
	if run.ResolvedAt == nil || !run.ResolvedAt.Equal(retry2At) {
		t.Errorf("resolved_at: got %v, want the recovering retry's time %v", run.ResolvedAt, retry2At)
	}
	var successRow, resolvedRow *domain.InvoiceDunningEvent
	for i := range store.events {
		e := store.events[i]
		if e.EventType == domain.DunningEventRetryAttempted && e.Reason == "succeeded" {
			successRow = &store.events[i]
		}
		if e.EventType == domain.DunningEventResolved {
			resolvedRow = &store.events[i]
		}
	}
	if successRow == nil {
		t.Fatal("the successful retry wrote no retry_attempted row")
	}
	if !successRow.CreatedAt.Equal(retry2At) {
		t.Errorf("success retry row: got %v, want %v", successRow.CreatedAt, retry2At)
	}
	if resolvedRow == nil || !resolvedRow.CreatedAt.Equal(retry2At) {
		t.Errorf("resolved row: %v, want one at %v", resolvedRow, retry2At)
	}
}

// TestStartDunning_CauseStamped locks the write-time cause (the fix for
// the hardcoded-payment_failed lie): the caller's declared cause lands
// on BOTH the run and the dunning_started event, and an unknown cause
// is refused before any row is written.
func TestStartDunning_CauseStamped(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)
	ctx := context.Background()

	run, err := svc.StartDunning(ctx, "t1", "inv_cause", "cus_1", domain.DunningCauseNoPaymentMethod)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if run.Reason != string(domain.DunningCauseNoPaymentMethod) {
		t.Errorf("run reason = %q, want no_payment_method", run.Reason)
	}
	evs, _ := store.ListEvents(ctx, "t1", run.ID)
	found := false
	for _, e := range evs {
		if e.EventType == domain.DunningEventStarted {
			found = true
			if e.Reason != string(domain.DunningCauseNoPaymentMethod) {
				t.Errorf("started-event reason = %q, want no_payment_method", e.Reason)
			}
		}
	}
	if !found {
		t.Fatal("no dunning_started event written")
	}

	if _, err := svc.StartDunning(ctx, "t1", "inv_cause2", "cus_1", domain.DunningStartCause("wat")); err == nil {
		t.Fatal("unknown cause must be refused")
	}
}

// TestStartDunning_DisabledPolicy_SkipsSameClassAsNoPolicy pins the sibling of
// TestStartDunning_NoPolicyConfigured. Both end in "this invoice will never
// dun", so both must be the same DELIBERATE-SKIP class the enrollment sweeps
// swallow — never a raw error that poisons a catchup.
//
// Why this exists (FLOW TC5 walk, 2026-08-04): POST /v1/dunning/policies
// decodes straight into the domain struct, so omitting `enabled` creates a
// DISABLED policy — and the first policy a tenant creates is auto-promoted to
// default. An API caller therefore ends up with a policy that is
// simultaneously the tenant default and inert: no error at create, no dunning
// run, and — before the fix this test guards — no log line either, while the
// no-policy path next door logged a WARN. The dashboard never hits it (its
// form sends enabled:true) and the recipe installer sets Enabled explicitly,
// so this is the API/SDK-caller trap specifically.
func TestStartDunning_DisabledPolicy_SkipsSameClassAsNoPolicy(t *testing.T) {
	store := newMemStore()
	p := store.policies["dpol_1"]
	p.Enabled = false // the shape POST /v1/dunning/policies produces without `enabled`
	store.policies["dpol_1"] = p
	svc := NewService(store, &noopRetrier{}, nil)

	_, err := svc.StartDunning(context.Background(), "t1", "inv_disabled", "cus_1", domain.DunningCausePaymentFailed)
	if err == nil {
		t.Fatal("expected a deliberate-skip error when the effective policy is disabled")
	}
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Errorf("disabled policy must map to ErrInvalidState (deliberate skip, swallowed by the sweeps); got %v", err)
	}
	if errors.Is(err, errs.ErrNotFound) {
		t.Errorf("disabled policy must NOT surface ErrNotFound — that is the no-policy signal; got %v", err)
	}
	if _, runErr := store.GetRunByInvoice(context.Background(), "t1", "inv_disabled"); runErr == nil {
		t.Error("a disabled policy must not create a dunning run")
	}
}
