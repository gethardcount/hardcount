package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// auditRecorderMock fakes the handler's audit seam for timeline tests —
// records the QueryFilter it was handed so tests can pin the resource scope.
type auditRecorderMock struct {
	entries   []domain.AuditEntry
	total     int
	queryErr  error
	gotFilter audit.QueryFilter
}

func (m *auditRecorderMock) Log(_ context.Context, _, _, _, _, _ string, _ map[string]any) error {
	return nil
}

func (m *auditRecorderMock) Query(_ context.Context, _ string, f audit.QueryFilter) ([]domain.AuditEntry, int, error) {
	m.gotFilter = f
	if m.queryErr != nil {
		return nil, 0, m.queryErr
	}
	return m.entries, m.total, nil
}

func timelineRequest(ctx context.Context, subID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/subscriptions/%s/timeline", subID), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", subID)
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

// The audit log is the timeline's ONLY event source: a query failure must
// surface as a 5xx, never a 200 with an empty feed indistinguishable from
// "this subscription has no history" — the empty-200 shape sent CS reps
// away thinking nothing had happened (audit e2e 2026-07-13).
func TestActivityTimeline_AuditQueryErrorSurfacesAs500(t *testing.T) {
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_a")

	h := NewHandler(NewService(store, nil))
	h.SetAuditLogger(&auditRecorderMock{queryErr: errors.New("audit db down")})

	rr := httptest.NewRecorder()
	h.activityTimeline(rr, timelineRequest(
		context.WithValue(context.Background(), auth.TestTenantIDKey(), tenantID), subID))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500 (empty-200 masks audit failures). body=%s", rr.Code, rr.Body.String())
	}
}

// Pins two contracts at once (audit e2e 2026-07-13, Q13):
//  1. SCOPE — the timeline is an object-scoped activity view under
//     PermSubscriptionRead; its audit query must always be pinned to this
//     one subscription (ResourceType+ResourceID). Widening it would turn a
//     weaker permission into a general audit-log reader.
//  2. TRUNCATION — the store clamps at 100 rows; when the sub has more, the
//     response must say so explicitly instead of silently starting the
//     timeline mid-history.
func TestActivityTimeline_ScopePinnedAndTruncationFlagged(t *testing.T) {
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_a")

	entries := make([]domain.AuditEntry, 100)
	for i := range entries {
		entries[i] = domain.AuditEntry{
			Action:    "subscription.updated",
			CreatedAt: time.Now().UTC().Add(-time.Duration(i) * time.Minute),
		}
	}
	rec := &auditRecorderMock{entries: entries, total: 150}

	h := NewHandler(NewService(store, nil))
	h.SetAuditLogger(rec)

	rr := httptest.NewRecorder()
	h.activityTimeline(rr, timelineRequest(
		context.WithValue(context.Background(), auth.TestTenantIDKey(), tenantID), subID))

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if rec.gotFilter.ResourceType != "subscription" || rec.gotFilter.ResourceID != subID {
		t.Errorf("audit query must stay pinned to this subscription; got filter %+v", rec.gotFilter)
	}
	if rec.gotFilter.Limit != 100 {
		t.Errorf("timeline must request the store's 100-row clamp explicitly; got Limit=%d", rec.gotFilter.Limit)
	}

	var resp struct {
		Events    []map[string]any `json:"events"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Truncated {
		t.Error("truncated: got false, want true (total 150 > 100 fetched)")
	}
	if len(resp.Events) != 100 {
		t.Errorf("events: got %d, want 100", len(resp.Events))
	}
}

func TestActivityTimeline_NoTruncationOnFullHistory(t *testing.T) {
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_a")

	rec := &auditRecorderMock{
		entries: []domain.AuditEntry{{Action: "subscription.created", CreatedAt: time.Now().UTC()}},
		total:   1,
	}
	h := NewHandler(NewService(store, nil))
	h.SetAuditLogger(rec)

	rr := httptest.NewRecorder()
	h.activityTimeline(rr, timelineRequest(
		context.WithValue(context.Background(), auth.TestTenantIDKey(), tenantID), subID))

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Truncated {
		t.Error("truncated: got true, want false (full history fetched)")
	}
}

// TestActivityTimeline_SortsByDisplayedSimAxis is the 2026-07-19 inversion
// regression, replicating the live shape exactly: a pause PUT and the
// catchup auto-resume wrote 36ms apart (same wall SECOND), with simulated
// instants 8 days apart. The old sort keyed on the second-truncated wall
// Timestamp string — the pair tied, the tie kept the audit query's DESC
// orientation, and "auto-resumed (sim Nov 10)" rendered ABOVE "paused
// (sim Nov 2)" on a surface that displays sim-primary (#513). The sort
// axis must be the DISPLAYED axis: sim when present, wall otherwise;
// equal display instants tie-break on full-precision wall write order.
func TestActivityTimeline_SortsByDisplayedSimAxis(t *testing.T) {
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_a")

	wall := time.Date(2026, 7, 18, 19, 6, 37, 0, time.UTC)
	simMeta := func(sim string) map[string]any {
		return map[string]any{"sim_effective_at": sim, "test_clock_id": "clk_1"}
	}
	// Audit query returns DESC (newest wall first) — build it that way.
	entries := []domain.AuditEntry{
		// The axis-disagreement case (ADR-097 remediation shape): the
		// NEWEST wall write carries the EARLIEST sim instant — a stranded
		// cancel healed long after its contracted date. Wall-axis sorting
		// dumps it at the bottom; the display (sim) axis puts it FIRST.
		{Action: "subscription.canceled", CreatedAt: wall.Add(50 * time.Second),
			Metadata: map[string]any{"action": "cancel_fired", "sim_effective_at": "2028-08-15T00:00:00Z", "test_clock_id": "clk_1"}},
		{Action: "subscription.updated", CreatedAt: wall.Add(840 * time.Millisecond),
			Metadata: map[string]any{"action": "collection_resumed", "sim_effective_at": "2028-11-10T00:00:00Z", "test_clock_id": "clk_1"}},
		{Action: "subscription.updated", CreatedAt: wall.Add(804 * time.Millisecond),
			Metadata: map[string]any{"action": "collection_paused", "sim_effective_at": "2028-11-02T00:00:00Z", "test_clock_id": "clk_1"}},
		// Same SIM instant pair (create+activate at the same frozen time,
		// 100ms apart on the wall): the causal tie-break must order by
		// full-precision wall write time, not the DESC input order.
		{Action: "subscription.activated", CreatedAt: wall.Add(-10 * time.Second).Add(100 * time.Millisecond),
			Metadata: simMeta("2028-10-20T00:00:00Z")},
		{Action: "subscription.created", CreatedAt: wall.Add(-10 * time.Second),
			Metadata: simMeta("2028-10-20T00:00:00Z")},
		// Exactly-equal pair (same tx batch: identical created_at AND sim).
		// DESC input carries them reverse-causally; only the pre-sort
		// Reverse restores true insertion order for residual ties.
		{Action: "subscription.item_removed", CreatedAt: wall.Add(-20 * time.Second),
			Metadata: simMeta("2028-10-19T00:00:00Z")},
		{Action: "subscription.item_added", CreatedAt: wall.Add(-20 * time.Second),
			Metadata: simMeta("2028-10-19T00:00:00Z")},
	}
	rec := &auditRecorderMock{entries: entries, total: len(entries)}

	h := NewHandler(NewService(store, nil))
	h.SetAuditLogger(rec)

	rr := httptest.NewRecorder()
	h.activityTimeline(rr, timelineRequest(
		context.WithValue(context.Background(), auth.TestTenantIDKey(), tenantID), subID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Events []struct {
			EventType      string `json:"event_type"`
			SimEffectiveAt string `json:"sim_effective_at"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Events) != 7 {
		t.Fatalf("events: got %d, want 7", len(resp.Events))
	}
	if resp.Events[1].EventType != "subscription.item_added" || resp.Events[2].EventType != "subscription.item_removed" {
		t.Errorf("exactly-equal (sim, created_at) pair must keep true insertion order (added before removed): got %s then %s", resp.Events[1].EventType, resp.Events[2].EventType)
	}
	wantSims := []string{
		"2028-08-15T00:00:00Z", // remediated cancel: latest wall write, earliest sim
		"2028-10-19T00:00:00Z", // item added (equal-tie pair, causal order)
		"2028-10-19T00:00:00Z", // item removed
		"2028-10-20T00:00:00Z", // created (tie-break: earlier wall write)
		"2028-10-20T00:00:00Z", // activated
		"2028-11-02T00:00:00Z", // paused — MUST precede…
		"2028-11-10T00:00:00Z", // …auto-resumed (the inversion)
	}
	for i, want := range wantSims {
		if resp.Events[i].SimEffectiveAt != want {
			t.Fatalf("position %d: got sim %s (%s), want %s — display order does not follow the sim axis", i, resp.Events[i].SimEffectiveAt, resp.Events[i].EventType, want)
		}
	}
	if resp.Events[3].EventType != "subscription.created" || resp.Events[4].EventType != "subscription.activated" {
		t.Errorf("same-sim tie must order by wall write time: got %s then %s", resp.Events[3].EventType, resp.Events[4].EventType)
	}
}

// TestDescribeSubscriptionAction_BillingRetried: the operator's retry-billing
// audit row (ADR-119) reads as a sentence on the subscription timeline. The
// metadata arrives as JSON, so the count is a float64.
func TestDescribeSubscriptionAction_BillingRetried(t *testing.T) {
	desc, detail, _, status := describeSubscriptionAction(domain.AuditActionUpdate,
		map[string]any{"action": "billing_retried", "attempts_before": float64(8), "was_parked": true}, nil)
	if desc != "Billing retried" || detail != "After 8 failed attempts" || status != "info" {
		t.Fatalf("got %q / %q / %q", desc, detail, status)
	}
	_, detail, _, _ = describeSubscriptionAction(domain.AuditActionUpdate,
		map[string]any{"action": "billing_retried", "attempts_before": float64(1)}, nil)
	if detail != "After 1 failed attempt" {
		t.Fatalf("singular: got %q", detail)
	}
}
