package webhook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// wireRecorder answers the n-th POST with statuses[n] (the last entry
// repeats) and keeps every body it was sent.
type wireRecorder struct {
	statuses []int
	bodies   []map[string]any
}

func (c *wireRecorder) Do(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("wireRecorder: body is not JSON: %w", err)
	}
	c.bodies = append(c.bodies, body)
	status := c.statuses[len(c.statuses)-1]
	if n := len(c.bodies) - 1; n < len(c.statuses) {
		status = c.statuses[n]
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

// TestReplay_SendsRootEvent_RealStore runs the real case on the real store:
// an invoice.paid went out a day ago, the endpoint failed, and the operator
// replays it. Every send of the replay (Replay, its retry by the delivery
// worker, a replay of the replay, and a one-endpoint ReplayDelivery) must
// carry the root event's id and created_at, which docs/webhooks.md tells
// receivers to dedupe on. The replay rows themselves keep their own id and
// time, so the dashboard's timeline is unchanged.
func TestReplay_SendsRootEvent_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx, cancel := context.WithTimeout(
		leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleWebhookDelivery),
		20*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Replay Wire Id")
	store := webhook.NewPostgresStore(db)
	// 200 original, 500 replay, 200 its retry, then 200 for every later send.
	client := &wireRecorder{statuses: []int{200, 500, 200}}
	svc := webhook.NewTestService(store, client)

	ep, err := store.CreateEndpoint(ctx, tenantID, domain.WebhookEndpoint{
		URL: "https://example.test/hook", Events: []string{"invoice.paid"}, Active: true, Secret: "whsec_test",
	})
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := svc.Dispatch(ctx, tenantID, "invoice.paid", map[string]any{"invoice_id": "vlx_inv_1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(client.bodies) != 1 {
		t.Fatalf("POSTs after dispatch = %d, want 1", len(client.bodies))
	}
	origID, _ := client.bodies[0]["id"].(string)

	// The original went out a day ago.
	if _, err := admin.ExecContext(ctx,
		`UPDATE webhook_events SET created_at = created_at - INTERVAL '1 day' WHERE id = $1`, origID); err != nil {
		t.Fatalf("backdate original event: %v", err)
	}
	orig, err := store.GetEvent(ctx, tenantID, origID)
	if err != nil {
		t.Fatalf("get original event: %v", err)
	}
	wantCreated := orig.CreatedAt.Format(time.RFC3339)
	assertSent := func(step string) {
		t.Helper()
		body := client.bodies[len(client.bodies)-1]
		if body["id"] != origID {
			t.Errorf("%s: sent id = %v, want the root event's id %s", step, body["id"], origID)
		}
		if body["created_at"] != wantCreated {
			t.Errorf("%s: sent created_at = %v, want the root event's %s", step, body["created_at"], wantCreated)
		}
	}

	// 1. Replay: the endpoint answers 500, so the replay stays pending.
	res, err := svc.Replay(ctx, tenantID, origID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	assertSent("replay")

	// Control: the replay row keeps its own id and today's time, so the
	// dashboard lists it as a new resend.
	clone, err := store.GetEvent(ctx, tenantID, res.EventID)
	if err != nil {
		t.Fatalf("get replay event: %v", err)
	}
	if clone.ID == origID || !clone.CreatedAt.After(orig.CreatedAt.Add(23*time.Hour)) {
		t.Errorf("replay row = %s at %s, want its own id and a time a day after the original's %s", clone.ID, clone.CreatedAt, orig.CreatedAt)
	}

	// 2. The delivery worker retries the failed replay; it loads the
	// event again by id.
	deliveries, err := store.ListDeliveries(ctx, tenantID, origID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	var replayDeliveryID string
	for _, d := range deliveries {
		if d.WebhookEventID == res.EventID {
			replayDeliveryID = d.ID
		}
	}
	if replayDeliveryID == "" {
		t.Fatalf("no delivery for the replay event %s", res.EventID)
	}
	setDuePast(t, db, replayDeliveryID)
	if err := svc.RetryPendingDeliveries(ctx); err != nil {
		t.Fatalf("retry pending: %v", err)
	}
	if len(client.bodies) != 3 {
		t.Fatalf("POSTs after retry = %d, want 3 (original, replay, retry)", len(client.bodies))
	}
	assertSent("retried replay")
	retried, err := store.GetDelivery(ctx, tenantID, replayDeliveryID)
	if err != nil {
		t.Fatalf("get replay delivery: %v", err)
	}
	if retried.Status != domain.DeliverySucceeded {
		t.Errorf("replay delivery status after retry = %s, want succeeded", retried.Status)
	}

	// 3. Replay of the replay.
	if _, err := svc.Replay(ctx, tenantID, res.EventID); err != nil {
		t.Fatalf("replay of a replay: %v", err)
	}
	assertSent("replay of a replay")

	// 4. Resend to one endpoint, from the replay's delivery.
	if _, err := svc.ReplayDelivery(ctx, tenantID, ep.ID, replayDeliveryID); err != nil {
		t.Fatalf("replay delivery: %v", err)
	}
	assertSent("one-endpoint resend")
}
