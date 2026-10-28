package dunning

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
)

// The Dunning page's recovery timeline on the wire (ADR-128): each event of
// GET /v1/dunning/runs/{id} carries the line the invoice's Activity card
// shows for it, and a lane that could not be read is named, never dropped.

type linerFake struct {
	lines    map[string]activity.Line
	degraded []string
	err      error
	got      domain.InvoiceDunningRun
}

func (f *linerFake) RecoveryRunLines(_ context.Context, _ string, run domain.InvoiceDunningRun, _ []domain.InvoiceDunningEvent) (map[string]activity.Line, []string, error) {
	f.got = run
	return f.lines, f.degraded, f.err
}

type eventsErrStore struct{ *memStore }

func (eventsErrStore) ListEvents(context.Context, string, string) ([]domain.InvoiceDunningEvent, error) {
	return nil, errors.New("db down")
}

func getRunWire(t *testing.T, store Store, liner EventLiner, runID string) map[string]any {
	t.Helper()
	h := NewHandler(NewService(store, nil, nil))
	if liner != nil {
		h.SetEventLiner(liner)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID)
	ctx := auth.WithTenantID(req.Context(), "t1")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.getRun(rr, req.WithContext(ctx))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGetRun_EventsCarryTheirLines(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	store := newMemStore()
	store.runs["hc_dr_1"] = domain.InvoiceDunningRun{ID: "hc_dr_1", InvoiceID: "hc_inv_1", State: domain.DunningActive}
	store.events = []domain.InvoiceDunningEvent{
		{ID: "evt_1", RunID: "hc_dr_1", EventType: domain.DunningEventStarted, CreatedAt: at},
		{ID: "evt_2", RunID: "hc_dr_1", EventType: domain.DunningEventRetryAttempted, AttemptCount: 1, CreatedAt: at.Add(time.Hour)},
	}
	liner := &linerFake{
		lines: map[string]activity.Line{
			"evt_1": activity.Stamp(activity.Line{ID: "dunning:evt_1", Title: "Payment recovery started", Tone: activity.ToneNeutral, Actor: activity.Automatic()}, at, time.Time{}, false),
			"evt_2": activity.Stamp(activity.Line{ID: "dunning:evt_2", Title: "Payment retry 1 failed", Tone: activity.ToneFailure, Actor: activity.Automatic()}, at.Add(time.Hour), time.Time{}, false),
		},
		degraded: []string{"emails"},
	}
	body := getRunWire(t, store, liner, "hc_dr_1")
	if liner.got.ID != "hc_dr_1" || liner.got.InvoiceID != "hc_inv_1" {
		t.Errorf("the liner was given run %+v", liner.got)
	}
	events, _ := body["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("%d events, want 2", len(events))
	}
	for i, title := range []string{"Payment recovery started", "Payment retry 1 failed"} {
		e, _ := events[i].(map[string]any)
		line, _ := e["line"].(map[string]any)
		if line["title"] != title || e["event_type"] == nil || e["id"] == nil {
			t.Errorf("event %d: %v, want the event with line %q", i, e, title)
		}
	}
	if got := body["degraded"]; !reflect.DeepEqual(got, []any{"emails"}) {
		t.Errorf("degraded %v, want [emails]", got)
	}

	// The run's invoice cannot be read: the events still come back, without
	// lines, and the lane is named.
	body = getRunWire(t, store, &linerFake{err: errors.New("invoice gone")}, "hc_dr_1")
	events, _ = body["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("%d events, want 2", len(events))
	}
	if e, _ := events[0].(map[string]any); e["line"] != nil {
		t.Errorf("a line with no invoice behind it: %v", e["line"])
	}
	if got := body["degraded"]; !reflect.DeepEqual(got, []any{"invoice"}) {
		t.Errorf("degraded %v, want [invoice]", got)
	}

	// The events cannot be read: an empty timeline that says so.
	body = getRunWire(t, eventsErrStore{store}, liner, "hc_dr_1")
	if events, _ := body["events"].([]any); len(events) != 0 {
		t.Errorf("events %v, want none", events)
	}
	if got := body["degraded"]; !reflect.DeepEqual(got, []any{"events"}) {
		t.Errorf("degraded %v, want [events]", got)
	}
}
