package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/activity"
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
		auth.WithTenantID(context.Background(), tenantID), subID))

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
		auth.WithTenantID(context.Background(), tenantID), subID))

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
		auth.WithTenantID(context.Background(), tenantID), subID))

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

// TestActivityTimeline_EarliestFirst: rows read earliest first, and rows with
// an equal created_at keep write order, although the audit query returns them
// newest first.
func TestActivityTimeline_EarliestFirst(t *testing.T) {
	tenantID := "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_a")

	wall := time.Date(2026, 7, 18, 19, 6, 37, 0, time.UTC)
	// The audit query returns newest first; build it that way.
	entries := []domain.AuditEntry{
		{Action: domain.AuditActionUpdate, CreatedAt: wall.Add(840 * time.Millisecond),
			Metadata: map[string]any{"action": "collection_resumed"}},
		{Action: domain.AuditActionUpdate, CreatedAt: wall.Add(804 * time.Millisecond),
			Metadata: map[string]any{"action": "collection_paused"}},
		{Action: "subscription.activated", CreatedAt: wall.Add(-10 * time.Second).Add(100 * time.Millisecond)},
		{Action: "subscription.created", CreatedAt: wall.Add(-10 * time.Second)},
		// Written in one transaction: an equal created_at.
		{Action: "subscription.item_removed", CreatedAt: wall.Add(-20 * time.Second)},
		{Action: "subscription.item_added", CreatedAt: wall.Add(-20 * time.Second)},
	}
	h := NewHandler(NewService(store, nil))
	h.SetAuditLogger(&auditRecorderMock{entries: entries, total: len(entries)})

	rr := httptest.NewRecorder()
	h.activityTimeline(rr, timelineRequest(auth.WithTenantID(context.Background(), tenantID), subID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Events []struct {
			EventType   string `json:"event_type"`
			Description string `json:"description"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var got []string
	for _, e := range resp.Events {
		got = append(got, e.EventType+" "+e.Description)
	}
	want := []string{
		"subscription.item_added", "subscription.item_removed", "subscription.created",
		"subscription.activated", "update Collection paused", "update Collection resumed",
	}
	if len(got) != len(want) {
		t.Fatalf("events %q, want %d", got, len(want))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Errorf("position %d: %q, want %q (order %q)", i, got[i], w, got)
		}
	}
}

// TestDescribeSubscriptionAction_BillingRetried: the operator's retry-billing
// audit row (ADR-119) reads as a sentence on the subscription timeline. The
// metadata arrives as JSON, so the count is a float64.
func TestDescribeSubscriptionAction_BillingRetried(t *testing.T) {
	l := describeSubscriptionAction(domain.AuditActionUpdate,
		map[string]any{"action": "billing_retried", "attempts_before": float64(8), "was_parked": true}, lineLabels{})
	if l.Title != "Billing retried" || activity.MetaText(l) != "After 8 failed tries" || l.Tone != activity.ToneNeutral {
		t.Fatalf("got %q / %q / %q", l.Title, activity.MetaText(l), l.Tone)
	}
	l = describeSubscriptionAction(domain.AuditActionUpdate,
		map[string]any{"action": "billing_retried", "attempts_before": float64(1)}, lineLabels{})
	if got := activity.MetaText(l); got != "After 1 failed try" {
		t.Fatalf("singular: got %q", got)
	}
}

// TestDescribeSubscriptionAction_NetNegativeUsage: the row an invoice decision
// writes when a meter's usage was below zero after corrections (#1193) names
// each meter by its name and its amount, says what to do, and is a warning.
// A row written before the name was recorded shows the meter's key. The
// metadata arrives as JSON, so the meter list is []any.
func TestDescribeSubscriptionAction_NetNegativeUsage(t *testing.T) {
	l := describeSubscriptionAction("subscription.net_negative_usage", map[string]any{
		"billing_reason": "subscription_cycle",
		"meters": []any{
			map[string]any{"meter_id": "mtr_1", "event_name": "api_calls", "meter_name": "API calls", "quantity": "-50"},
			map[string]any{"meter_id": "mtr_2", "event_name": "tokens", "quantity": "-12.5"},
		},
	}, lineLabels{})
	if l.Title != "Usage below zero billed as zero" || l.Tone != activity.ToneWarning {
		t.Fatalf("got %q / %q, want the net-negative title as a warning", l.Title, l.Tone)
	}
	if got := activity.MetaText(l); got != "API calls -50 · tokens -12.5" {
		t.Errorf("meta %q", got)
	}
	if !strings.Contains(l.Problem, "credit note") {
		t.Errorf("problem %q does not say what to do", l.Problem)
	}
}
