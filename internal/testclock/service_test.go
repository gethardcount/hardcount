package testclock

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// --- Tests ---

// fakeCustomerReader records calls into the testclock service's
// CustomerReader hook. Used to assert that ListAttachedCustomers
// routes through the customer domain (not the testclock store).
type fakeCustomerReader struct {
	calls   int
	gotID   string
	returns []domain.Customer
}

func (f *fakeCustomerReader) ListByTestClockID(_ context.Context, _, clockID string, _, _ int) ([]domain.Customer, int, error) {
	f.calls++
	f.gotID = clockID
	return f.returns, len(f.returns), nil
}

// TestListAttachedCustomers_RoutesThroughCustomerReader pins the
// architectural contract: testclock.Service must NOT read customer
// rows out of its own store. The customer package owns the
// encrypt-at-rest wrapper around display_name / email; this surface
// reads through the customer service so values arrive decrypted.
func TestListAttachedCustomers_RoutesThroughCustomerReader(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady}

	reader := &fakeCustomerReader{returns: []domain.Customer{
		{ID: "cus_1", TenantID: "t1", DisplayName: "Smoke Corp"},
	}}
	clockDeps := testDeps(&countingRunner{})
	clockDeps.Customers = reader
	svc := NewService(store, clockDeps)

	got, _, err := svc.ListAttachedCustomers(context.Background(), "t1", "c1", 50, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("expected reader to be called once; got %d", reader.calls)
	}
	if reader.gotID != "c1" {
		t.Errorf("reader received clockID=%q; want c1", reader.gotID)
	}
	if len(got) != 1 || got[0].DisplayName != "Smoke Corp" {
		t.Errorf("unexpected return: %#v", got)
	}
}

func TestCreate_RequiresFrozenTime(t *testing.T) {
	s := NewService(newMockStore(), testDeps(&countingRunner{}))
	_, err := s.Create(context.Background(), "t1", CreateInput{Name: "jan 1"})
	if err == nil {
		t.Fatal("expected error for zero frozen_time")
	}
	if !errors.Is(err, errs.ErrValidation) {
		t.Errorf("want validation error, got %T: %v", err, err)
	}
}

func TestCreate_RejectsLongName(t *testing.T) {
	s := NewService(newMockStore(), testDeps(&countingRunner{}))
	longName := make([]byte, 201)
	for i := range longName {
		longName[i] = 'x'
	}
	_, err := s.Create(context.Background(), "t1", CreateInput{
		Name:       string(longName),
		FrozenTime: time.Now(),
	})
	if err == nil {
		t.Fatal("expected error for name > 200 chars")
	}
}

func TestCreate_Succeeds(t *testing.T) {
	store := newMockStore()
	s := NewService(store, testDeps(&countingRunner{}))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	clk, err := s.Create(context.Background(), "t1", CreateInput{
		Name:       "fy26",
		FrozenTime: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clk.Name != "fy26" {
		t.Errorf("name: got %q, want fy26", clk.Name)
	}
	if !clk.FrozenTime.Equal(now) {
		t.Errorf("frozen_time: got %v, want %v", clk.FrozenTime, now)
	}
	if clk.Status != domain.TestClockStatusReady {
		t.Errorf("status: got %q, want ready", clk.Status)
	}
}

func TestAdvance_RejectsNonReadyClock(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID:         "c1",
		TenantID:   "t1",
		Status:     domain.TestClockStatusAdvancing,
		FrozenTime: time.Now(),
	}
	s := NewService(store, testDeps(&countingRunner{}))

	_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{
		FrozenTime: time.Now().Add(24 * time.Hour),
	})
	if err == nil {
		t.Fatal("expected error advancing non-ready clock")
	}
}

func TestAdvance_RejectsBackwardTime(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID:         "c1",
		TenantID:   "t1",
		Status:     domain.TestClockStatusReady,
		FrozenTime: now,
	}
	s := NewService(store, testDeps(&countingRunner{}))

	_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{
		FrozenTime: now.Add(-time.Hour),
	})
	if err == nil {
		t.Fatal("advancing backward must be rejected")
	}
}

// TestAdvance_RejectsAdvanceOverOneYear asserts Hardcount's own per-call
// advance window cap (ADR-028 amendment). Operators chunk
// longer simulations into successive advances; a single call may
// not shift frozen_time by more than 1 year.
func TestAdvance_RejectsAdvanceOverOneYear(t *testing.T) {
	start := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		newTime  time.Time
		wantErr  bool
		errField string
	}{
		{"exactly one year", start.AddDate(1, 0, 0), false, ""},
		{"one second under one year", start.AddDate(1, 0, 0).Add(-time.Second), false, ""},
		{"one second over one year", start.AddDate(1, 0, 0).Add(time.Second), true, "frozen_time"},
		{"five years forward", start.AddDate(5, 0, 0), true, "frozen_time"},
		{"twenty-five years forward", start.AddDate(25, 0, 0), true, "frozen_time"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			store.clocks["c1"] = domain.TestClock{
				ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
			}
			s := NewService(store, testDeps(&countingRunner{}))
			_, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{FrozenTime: tc.newTime})

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected validation error, got nil")
				}
				field := errs.Field(err)
				if field != tc.errField {
					t.Errorf("error field: got %q, want %q", field, tc.errField)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success, got error: %v", err)
			}
		})
	}
}

// TestAdvance_StepsThroughEachDueTime: an advance moves the clock through
// each time something falls due, in order, and runs the jobs once at each,
// on a context bound to that time; it then ends ready at its target. The
// first pass is at the clock's time, where nothing new is due.
func TestAdvance_StepsThroughEachDueTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	target := start.AddDate(0, 3, 0)
	firstDue, secondDue := start.AddDate(0, 1, 0), start.AddDate(0, 1, 3)

	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start}
	store.due = []time.Time{secondDue, firstDue, target.AddDate(0, 0, 1)} // the last is past the target
	runner := &simCapturingRunner{}
	s := NewService(store, testDeps(runner))

	clk, err := advanceThenCatchUp(context.Background(), s, "t1", "c1", AdvanceInput{FrozenTime: target})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if clk.Status != domain.TestClockStatusReady || !clk.FrozenTime.Equal(target) || clk.AdvanceTarget != nil {
		t.Errorf("clock after the advance: %s at %v, target %v; want ready at %v, no target", clk.Status, clk.FrozenTime, clk.AdvanceTarget, target)
	}
	// Three billing jobs a pass: the period close, the threshold scan and
	// the card charges.
	var steps []time.Time
	for i, sim := range runner.sims {
		if sim.TestClockID != "c1" {
			t.Fatalf("job %d ran on clock %q, want c1", i, sim.TestClockID)
		}
		if i%3 == 0 {
			steps = append(steps, sim.At)
		}
	}
	if want := []time.Time{start, firstDue, secondDue, target}; !slices.EqualFunc(steps, want, time.Time.Equal) {
		t.Errorf("the jobs ran at %v, want %v", steps, want)
	}
}

// TestAdvance_RetryCarriesOnFromTheFailedStep: a job that fails at a step
// stops the advance there, with the counts of the steps before it. Retry
// advance runs that step again, carries on to the target, and the counts add
// up across both runs.
func TestAdvance_RetryCarriesOnFromTheFailedStep(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	due, target := start.AddDate(0, 1, 0), start.AddDate(0, 2, 0)

	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start}
	store.due = []time.Time{due}
	runner := &failsOnceAtRunner{at: due}
	s := NewService(store, testDeps(runner))

	if _, err := advanceThenCatchUp(context.Background(), s, "t1", "c1", AdvanceInput{FrozenTime: target}); err == nil {
		t.Fatal("the advance did not fail at the due time")
	}
	clk, _ := s.Get(context.Background(), "t1", "c1")
	if clk.Status != domain.TestClockStatusInternalFailed || !clk.FrozenTime.Equal(due) || clk.LastAdvanceSummary.InvoicesGenerated != 1 {
		t.Fatalf("after the failure: %s at %v with %d invoices; want internal_failure at %v with the first step's 1", clk.Status, clk.FrozenTime, clk.LastAdvanceSummary.InvoicesGenerated, due)
	}

	if _, err := s.RetryAdvance(context.Background(), "t1", "c1"); err != nil {
		t.Fatalf("retry advance: %v", err)
	}
	if err := runPass(context.Background(), s, "t1", "c1"); err != nil {
		t.Fatalf("the retried catch-up: %v", err)
	}
	clk, _ = s.Get(context.Background(), "t1", "c1")
	if clk.Status != domain.TestClockStatusReady || !clk.FrozenTime.Equal(target) || clk.LastAdvanceSummary.InvoicesGenerated != 3 {
		t.Errorf("after Retry advance: %s at %v with %d invoices; want ready at %v with 3, one a step", clk.Status, clk.FrozenTime, clk.LastAdvanceSummary.InvoicesGenerated, target)
	}
	if !clk.LastAdvanceSummary.AdvancedFrom.Equal(start) || !clk.LastAdvanceSummary.AdvancedTo.Equal(target) {
		t.Errorf("summary from %v to %v, want %v to %v", clk.LastAdvanceSummary.AdvancedFrom, clk.LastAdvanceSummary.AdvancedTo, start, target)
	}
}

// failsOnceAtRunner's period close bills one invoice a pass, except the
// first pass at at, which fails.
type failsOnceAtRunner struct {
	at     time.Time
	failed bool
}

func (r *failsOnceAtRunner) RunCycleForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	if sim, _ := clock.SimOf(ctx); sim.At.Equal(r.at) && !r.failed {
		r.failed = true
		return 0, []error{errors.New("the period close failed")}
	}
	return 1, nil
}

func (r *failsOnceAtRunner) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}

func (r *failsOnceAtRunner) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}

// TestAdvance_OnlyMarksTheClock: Advance marks the clock advancing to the new
// time, its target, and returns; the clock keeps its own time. It does not run the catch-up: the testclock_catchup
// leader does, on its tick (ADR-140). Where the advance starts from is the
// store's (TestPostgresStore_AdvanceSummary_RoundTrips).
func TestAdvance_OnlyMarksTheClock(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := start.Add(60 * 24 * time.Hour)

	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	runner := &countingRunner{}
	s := NewService(store, testDeps(runner))

	clk, err := s.Advance(context.Background(), "t1", "c1", AdvanceInput{FrozenTime: newTime})
	if err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	if clk.Status != domain.TestClockStatusAdvancing {
		t.Errorf("status on return: got %q, want advancing (the leader drives it to ready)", clk.Status)
	}
	if !clk.FrozenTime.Equal(start) || clk.AdvanceTarget == nil || !clk.AdvanceTarget.Equal(newTime) {
		t.Errorf("frozen_time %v, target %v; want the clock's own time %v and the target %v", clk.FrozenTime, clk.AdvanceTarget, start, newTime)
	}
	if runner.cycleCalls != 0 {
		t.Errorf("Advance ran the billing cycle %d times, want 0: the catch-up is the leader's", runner.cycleCalls)
	}
}

// TestRetryAdvance_ResumesFromInternalFailure covers ADR-018:
// a clock parked at internal_failure can be retried without
// destroying simulation state. After RetryAdvance the clock is
// back in 'advancing' with the prior failure reason cleared, for the
// testclock_catchup leader to pick up.
func TestRetryAdvance_ResumesFromInternalFailure(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID:                "c1",
		TenantID:          "t1",
		Status:            domain.TestClockStatusInternalFailed,
		FrozenTime:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		LastFailureReason: "stripe tax: provider 503",
	}
	s := NewService(store, testDeps(&countingRunner{}))

	clk, err := s.RetryAdvance(context.Background(), "t1", "c1")
	if err != nil {
		t.Fatalf("retry advance: %v", err)
	}
	if clk.Status != domain.TestClockStatusAdvancing {
		t.Errorf("status on return: got %q, want advancing", clk.Status)
	}
	if clk.LastFailureReason != "" {
		t.Errorf("last_failure_reason: got %q, want empty (cleared on retry)", clk.LastFailureReason)
	}
}

// TestRetryAdvance_RefusesNonFailedClock covers the gate:
// retry from ready or advancing must 409, not silently transition.
func TestRetryAdvance_RefusesNonFailedClock(t *testing.T) {
	for _, status := range []domain.TestClockStatus{
		domain.TestClockStatusReady, domain.TestClockStatusAdvancing,
	} {
		t.Run(string(status), func(t *testing.T) {
			store := newMockStore()
			store.clocks["c1"] = domain.TestClock{
				ID: "c1", TenantID: "t1", Status: status,
				FrozenTime: time.Now(),
			}
			s := NewService(store, testDeps(&countingRunner{}))
			_, err := s.RetryAdvance(context.Background(), "t1", "c1")
			if err == nil {
				t.Fatalf("expected error retrying from %s", status)
			}
		})
	}
}

// TestAdvance_FailurePersistsReason covers the worker-side: when
// catchup errors, MarkFailed gets called with the underlying
// error string, which is captured on the row for the dashboard.
func TestAdvance_FailurePersistsReason(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := start.Add(60 * 24 * time.Hour)

	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	sub := domain.Subscription{
		ID: "s1", TenantID: "t1", TestClockID: "c1", Status: domain.SubscriptionActive,
	}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	store.subsOnClock["c1"] = []domain.Subscription{sub}

	runner := &stubRunner{
		store: store, clockID: "c1", subID: "s1",
		err: errors.New("stripe tax: 503 service unavailable"),
	}
	s := NewService(store, testDeps(runner))
	_, err := advanceThenCatchUp(context.Background(), s, "t1", "c1", AdvanceInput{FrozenTime: newTime})
	if err == nil {
		t.Fatal("expected advance to fail")
	}
	got := store.clocks["c1"]
	if got.Status != domain.TestClockStatusInternalFailed {
		t.Errorf("status: got %q, want internal_failure", got.Status)
	}
	if got.LastFailureReason == "" {
		t.Error("last_failure_reason should be populated after a failed catchup")
	}
}

// TestRunAdvancing_RunsEachAdvancingClock: the tick runs one pass on every
// clock its claim list returns, and leaves the others alone.
func TestRunAdvancing_RunsEachAdvancingClock(t *testing.T) {
	at := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: at,
	}
	store.clocks["c2"] = domain.TestClock{
		ID: "c2", TenantID: "t2", Status: domain.TestClockStatusAdvancing, FrozenTime: at,
	}
	store.clocks["c3"] = domain.TestClock{
		ID: "c3", TenantID: "t3", Status: domain.TestClockStatusReady, FrozenTime: at,
	}
	s := NewService(store, testDeps(&countingRunner{}))

	worked, err := s.RunAdvancing(context.Background())
	if err != nil {
		t.Fatalf("RunAdvancing: %v", err)
	}
	if worked != 2 {
		t.Errorf("clocks run: got %d, want 2 (only c1 and c2 were advancing)", worked)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if got := store.clocks[id].Status; got != domain.TestClockStatusReady {
			t.Errorf("%s: status %q, want ready", id, got)
		}
	}
}

func TestAdvance_BillingFailureMarksClockFailed(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start,
	}
	sub := domain.Subscription{
		ID: "s1", TenantID: "t1", TestClockID: "c1", Status: domain.SubscriptionActive,
	}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	store.subsOnClock["c1"] = []domain.Subscription{sub}

	runner := &stubRunner{
		store:   store,
		clockID: "c1",
		subID:   "s1",
		err:     errors.New("boom"),
	}
	s := NewService(store, testDeps(runner))

	_, err := advanceThenCatchUp(context.Background(), s, "t1", "c1", AdvanceInput{
		FrozenTime: start.Add(30 * 24 * time.Hour),
	})
	if err == nil {
		t.Fatal("expected error on billing failure")
	}
	if store.clocks["c1"].Status != domain.TestClockStatusInternalFailed {
		t.Errorf("clock status after failure: got %q, want internal_failure",
			store.clocks["c1"].Status)
	}
}

// --- Mocks ---

type mockStore struct {
	clocks      map[string]domain.TestClock
	subsOnClock map[string][]domain.Subscription
	// due is what NextDue answers from: the times something falls due.
	due []time.Time
}

func newMockStore() *mockStore {
	return &mockStore{
		clocks:      make(map[string]domain.TestClock),
		subsOnClock: make(map[string][]domain.Subscription),
	}
}

func (m *mockStore) Create(_ context.Context, tenantID string, clk domain.TestClock, _ AuditRow) (domain.TestClock, error) {
	clk.ID = "c_" + clk.Name
	clk.TenantID = tenantID
	clk.Status = domain.TestClockStatusReady
	clk.CreatedAt = time.Now()
	clk.UpdatedAt = clk.CreatedAt
	m.clocks[clk.ID] = clk
	return clk, nil
}

func (m *mockStore) Get(_ context.Context, _, id string) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	return c, nil
}

func (m *mockStore) List(_ context.Context, _ string, _, _ int) ([]domain.TestClock, int, error) {
	out := make([]domain.TestClock, 0, len(m.clocks))
	for _, c := range m.clocks {
		out = append(out, c)
	}
	return out, len(out), nil
}

func (m *mockStore) Delete(_ context.Context, _, id string, _ AuditRow) error {
	if _, ok := m.clocks[id]; !ok {
		return errs.ErrNotFound
	}
	delete(m.clocks, id)
	return nil
}

func (m *mockStore) MarkAdvancing(_ context.Context, _, id string, newFrozenTime time.Time, _ AuditRow) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	if c.Status != domain.TestClockStatusReady {
		return domain.TestClock{}, errs.InvalidState("not ready")
	}
	c.Status = domain.TestClockStatusAdvancing
	c.AdvanceTarget = &newFrozenTime
	c.LastAdvanceSummary = &domain.AdvanceSummary{AdvancedFrom: c.FrozenTime}
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) TouchAdvancing(_ context.Context, _, id string) error {
	c, ok := m.clocks[id]
	if !ok || c.Status != domain.TestClockStatusAdvancing {
		return ErrClockNoLongerAdvancing
	}
	return nil
}

func (m *mockStore) MoveTo(_ context.Context, _, id string, to time.Time, sum domain.AdvanceSummary) error {
	c, ok := m.clocks[id]
	if !ok || c.Status != domain.TestClockStatusAdvancing {
		return ErrClockNoLongerAdvancing
	}
	c.FrozenTime = to
	c.LastAdvanceSummary = &sum
	m.clocks[id] = c
	return nil
}

func (m *mockStore) NextDue(_ context.Context, _, _ string, after, until time.Time) (time.Time, error) {
	next := until
	for _, d := range m.due {
		if d.After(after) && d.Before(next) {
			next = d
		}
	}
	return next, nil
}

func (m *mockStore) CompleteAdvance(_ context.Context, _, id string, sum domain.AdvanceSummary) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	if c.Status != domain.TestClockStatusAdvancing {
		return domain.TestClock{}, errs.InvalidState("not advancing")
	}
	c.Status = domain.TestClockStatusReady
	c.LastFailureReason = ""
	c.AdvanceTarget = nil
	c.LastAdvanceSummary = &sum
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) MarkFailed(_ context.Context, _, id, reason string, sum domain.AdvanceSummary) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	c.Status = domain.TestClockStatusInternalFailed
	c.LastFailureReason = reason
	c.LastAdvanceSummary = &sum
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) RetryFromFailed(_ context.Context, _, id string, _ AuditRow) (domain.TestClock, error) {
	c, ok := m.clocks[id]
	if !ok {
		return domain.TestClock{}, errs.ErrNotFound
	}
	if c.Status != domain.TestClockStatusInternalFailed {
		return domain.TestClock{}, errs.InvalidState("not internal_failure")
	}
	c.Status = domain.TestClockStatusAdvancing
	c.LastFailureReason = ""
	m.clocks[id] = c
	return c, nil
}

func (m *mockStore) ListAdvancingForCatchup(_ context.Context, limit int) ([]domain.TestClock, error) {
	var out []domain.TestClock
	for _, c := range m.clocks {
		if c.Status == domain.TestClockStatusAdvancing && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

// stubRunner mimics the post-ADR-028 billing engine: one RunCycle call
// catches the sub up across all due periods. Advances next_billing_at
// by cycleDur in a loop until it overshoots frozen_time. Returns the
// number of "invoices" generated. Track call count for tests that
// assert RunCycle is invoked exactly once per pass.
// countingRunner is a deterministic BillingRunner: RunCycleForClock returns a
// fixed invoice count (or a fixed error), and the other phases are no-ops. Used
// to assert the advance summary captures the phase counts exactly.
type countingRunner struct {
	invoices   int
	err        error
	cycleCalls int
}

func (r *countingRunner) RunCycleForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	r.cycleCalls++
	if r.err != nil {
		return 0, []error{r.err}
	}
	return r.invoices, nil
}
func (r *countingRunner) ScanThresholdsForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}
func (r *countingRunner) RetryPendingChargesForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}

// TestRunCatchup_RecordsAdvanceSummary verifies the post-advance summary
// captures the (exact) per-phase counts and the simulated span, on both the
// success transition and the partial-failure transition.
func TestRunCatchup_RecordsAdvanceSummary(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	t.Run("success persists exact counts and span", func(t *testing.T) {
		store := newMockStore()
		store.clocks["c1"] = domain.TestClock{
			ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: to,
			LastAdvanceSummary: &domain.AdvanceSummary{AdvancedFrom: from}, // as MarkAdvancing leaves it
		}
		s := NewService(store, testDeps(&countingRunner{invoices: 3}))

		if err := runPass(context.Background(), s, "t1", "c1"); err != nil {
			t.Fatalf("pass: %v", err)
		}
		got := store.clocks["c1"]
		if got.Status != domain.TestClockStatusReady {
			t.Fatalf("status: got %q, want ready", got.Status)
		}
		sum := got.LastAdvanceSummary
		if sum == nil {
			t.Fatal("LastAdvanceSummary was not persisted")
		}
		if !sum.AdvancedFrom.Equal(from) || !sum.AdvancedTo.Equal(to) {
			t.Errorf("span: got %s → %s, want %s → %s", sum.AdvancedFrom, sum.AdvancedTo, from, to)
		}
		if sum.InvoicesGenerated != 3 {
			t.Errorf("InvoicesGenerated: got %d, want 3", sum.InvoicesGenerated)
		}
	})

	t.Run("partial failure still persists the summary", func(t *testing.T) {
		store := newMockStore()
		store.clocks["c1"] = domain.TestClock{
			ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: to,
			LastAdvanceSummary: &domain.AdvanceSummary{AdvancedFrom: from},
		}
		s := NewService(store, testDeps(&countingRunner{err: errors.New("phase boom")}))

		if err := runPass(context.Background(), s, "t1", "c1"); err == nil {
			t.Fatal("expected the pass to surface the phase error")
		}
		got := store.clocks["c1"]
		if got.Status != domain.TestClockStatusInternalFailed {
			t.Fatalf("status: got %q, want internal_failure", got.Status)
		}
		if got.LastAdvanceSummary == nil {
			t.Fatal("summary must persist even on a partial-failure advance")
		}
	})
}

type stubRunner struct {
	store    *mockStore
	clockID  string
	subID    string
	cycleDur time.Duration
	calls    int
	err      error
}

// RunCycleForClock implements the new disjoint-flow contract
// RetryPendingChargesForClock — ADR-029 Phase 1 stub. The narrow tests
// in this file don't exercise auto-charge retry (no payment-method
// fixture wired); a no-op satisfies the interface contract while
// existing assertions on RunCycleForClock keep proving period-loop
// semantics. Tests that exercise the orchestration sequence end-to-end
// live in the integration-test suite where a real engine is wired.
func (r *stubRunner) RetryPendingChargesForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}

// ScanThresholdsForClock — ADR-029 Phase 3 stub. Threshold-scan
// behavior is exercised by the engine's own threshold_scan tests; the
// catchup-orchestrator tests just need a no-op to satisfy the
// interface, mirroring the Phase 1 charge stub above.
func (r *stubRunner) ScanThresholdsForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}

// (ADR-028). The stub mimics the post-refactor engine: one call
// catches the sub up across all due periods of the targeted clock.
func (r *stubRunner) RunCycleForClock(_ context.Context, _, clockID string, _ int) (int, []error) {
	r.calls++
	if r.err != nil {
		return 0, []error{r.err}
	}
	clk, ok := r.store.clocks[clockID]
	if !ok {
		return 0, nil
	}
	subs := r.store.subsOnClock[clockID]
	invoices := 0
	for i := range subs {
		if subs[i].ID != r.subID || subs[i].NextBillingAt == nil {
			continue
		}
		// Loop until caught up (post-ADR-028 engine semantics).
		// Cap iterations at 10000 to mirror maxPeriodsPerSubPerCall.
		for n := 0; n < 10000 && !subs[i].NextBillingAt.After(clk.FrozenTime); n++ {
			next := subs[i].NextBillingAt.Add(r.cycleDur)
			subs[i].NextBillingAt = &next
			invoices++
		}
	}
	r.store.subsOnClock[clockID] = subs
	return invoices, nil
}

// simCapturingRunner records the ctx catchup hands each billing phase, so the
// test can assert what is bound on it.
type simCapturingRunner struct {
	sims []clock.Sim
	ok   []bool
}

func (r *simCapturingRunner) capture(ctx context.Context) {
	sim, ok := clock.SimOf(ctx)
	r.sims = append(r.sims, sim)
	r.ok = append(r.ok, ok)
}

func (r *simCapturingRunner) RunCycleForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	r.capture(ctx)
	return 0, nil
}

func (r *simCapturingRunner) ScanThresholdsForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	r.capture(ctx)
	return 0, nil
}

func (r *simCapturingRunner) RetryPendingChargesForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	r.capture(ctx)
	return 0, nil
}

// TestRunCatchup_BindsClockOntoCtx is the linchpin of the audit sim axis
// (ADR-090 §5): catchup binds the CLOCK, not just its frozen instant, onto the
// ctx every phase inherits. Every audit row a clock advance produces —
// finalize, cancel, credit expiry, clawback issue, dunning — is emitted under
// this ctx, and the Logger stamps sim_effective_at / test_clock_id from it.
//
// If this binding regresses to a bare WithEffectiveNow, no emitter breaks and
// no test fails except this one: the rows keep landing, silently missing the
// clock, and the ?test_clock_id= filter starts lying by omission — the exact
// class the sim axis exists to kill. That is why it is asserted on the driver,
// not on any one emitter.
func TestRunCatchup_BindsClockOntoCtx(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: frozen,
	}
	runner := &simCapturingRunner{}
	s := NewService(store, testDeps(runner))

	if err := runPass(context.Background(), s, "t1", "c1"); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if len(runner.sims) == 0 {
		t.Fatal("no billing phase ran — the binding assertion would be vacuous")
	}
	for i, sim := range runner.sims {
		if !runner.ok[i] {
			t.Fatalf("phase %d ran on a ctx with NO clock binding — audit rows from it would carry no sim axis", i)
		}
		if sim.TestClockID != "c1" {
			t.Errorf("phase %d: test_clock_id = %q, want c1", i, sim.TestClockID)
		}
		if !sim.At.Equal(frozen) {
			t.Errorf("phase %d: sim instant = %v, want frozen_time %v", i, sim.At, frozen)
		}
	}
}

// orderRecorder records the order in which the catchup calls the cycle
// billing phase and the pause-resume phase.
type orderRecorder struct{ calls []string }

func (r *orderRecorder) RunCycleForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	r.calls = append(r.calls, "cycle")
	return 0, nil
}
func (r *orderRecorder) ScanThresholdsForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	r.calls = append(r.calls, "threshold")
	return 0, nil
}
func (r *orderRecorder) RetryPendingChargesForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	return 0, nil
}
func (r *orderRecorder) ProcessExpiredPauseCollectionsForClock(_ context.Context, _, _ string, _ time.Time) (int, []error) {
	r.calls = append(r.calls, "resume")
	return 0, nil
}

// TestRunCatchup_ResumesPausesAfterCycleBilling: a period that ended before
// resumes_at must close while the pause is still on the row (it closes as a
// draft), so the pause-resume phase runs after cycle billing, and before the
// threshold scan so that scan sees the resumed subscription.
func TestRunCatchup_ResumesPausesAfterCycleBilling(t *testing.T) {
	to := time.Date(2026, 2, 1, 0, 45, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: to}
	rec := &orderRecorder{}
	clockDeps := testDeps(rec)
	clockDeps.Pauses = rec
	s := NewService(store, clockDeps)
	if err := runPass(context.Background(), s, "t1", "c1"); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got, want := strings.Join(rec.calls, ","), "cycle,resume,threshold"; got != want {
		t.Errorf("phase order = %s, want %s", got, want)
	}
}

// advanceThenCatchUp is what a caller sees from an advance once the
// testclock_catchup leader has run it: Advance marks the clock, then one
// catch-up pass runs it. It returns the clock after the pass,
// and the pass's error.
func advanceThenCatchUp(ctx context.Context, s *Service, tenantID, id string, in AdvanceInput) (domain.TestClock, error) {
	if _, err := s.Advance(ctx, tenantID, id, in); err != nil {
		return domain.TestClock{}, err
	}
	if err := runPass(ctx, s, tenantID, id); err != nil {
		return domain.TestClock{}, err
	}
	return s.Get(ctx, tenantID, id)
}
