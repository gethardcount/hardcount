package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// recordingClient answers the n-th POST with statuses[n] (the last entry
// repeats) and keeps every body it was sent. Sync-delivery tests only.
type recordingClient struct {
	statuses []int
	bodies   []map[string]any
}

func (c *recordingClient) Do(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("recordingClient: body is not JSON: %w", err)
	}
	c.bodies = append(c.bodies, body)
	status := c.statuses[len(c.statuses)-1]
	if n := len(c.bodies) - 1; n < len(c.statuses) {
		status = c.statuses[n]
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

func (c *recordingClient) last(t *testing.T) map[string]any {
	t.Helper()
	if len(c.bodies) == 0 {
		t.Fatal("no POST was sent")
	}
	return c.bodies[len(c.bodies)-1]
}

// replayFixture is the real case behind a replay: an invoice.paid went to
// one endpoint a day ago, and the operator sends it again today. The
// original event is backdated by a day so a replay that sent its own
// created_at could not match the original's by landing in the same second.
func replayFixture(t *testing.T, statuses ...int) (*memStore, *recordingClient, *Service, domain.WebhookEvent) {
	t.Helper()
	store := newMemStore()
	client := &recordingClient{statuses: statuses}
	svc := NewTestService(store, client)
	ctx := postgres.WithLivemode(context.Background(), true)
	if _, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
		URL: "http://localhost:9999/hook", Events: []string{"invoice.paid"},
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := svc.Dispatch(ctx, "t1", "invoice.paid", map[string]any{"invoice_id": "hc_inv_1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	store.events[0].CreatedAt = store.events[0].CreatedAt.Add(-24 * time.Hour)
	return store, client, svc, store.events[0]
}

// assertSentAs checks a POST body carries the root event's id and
// created_at: docs/webhooks.md tells receivers to skip an id they have
// seen, and says the id "stays the same on every retry and replay".
func assertSentAs(t *testing.T, body map[string]any, root domain.WebhookEvent) {
	t.Helper()
	if body["id"] != root.ID {
		t.Errorf("sent id = %v, want the root event's id %s: a receiver that skips seen ids runs this event twice", body["id"], root.ID)
	}
	if want := root.CreatedAt.Format(time.RFC3339); body["created_at"] != want {
		t.Errorf("sent created_at = %v, want the root event's %s", body["created_at"], want)
	}
}

func TestReplay_SendsRootEventIDAndCreatedAt(t *testing.T) {
	_, client, svc, orig := replayFixture(t, 200)
	ctx := postgres.WithLivemode(context.Background(), true)

	res, err := svc.Replay(ctx, "t1", orig.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Control: the replay is still its own row, so the dashboard's
	// timeline keeps a record of each resend.
	if res.EventID == orig.ID || res.ReplayOf != orig.ID {
		t.Fatalf("replay result = %+v, want a new event row with replay_of %s", res, orig.ID)
	}
	assertSentAs(t, client.last(t), orig)

	// Replaying the replay still sends the root event.
	if _, err := svc.Replay(ctx, "t1", res.EventID); err != nil {
		t.Fatalf("replay of a replay: %v", err)
	}
	assertSentAs(t, client.last(t), orig)
}

func TestReplayDelivery_SendsRootEventIDAndCreatedAt(t *testing.T) {
	store, client, svc, orig := replayFixture(t, 200)
	ctx := postgres.WithLivemode(context.Background(), true)
	first := store.deliveries[0]

	res, err := svc.ReplayDelivery(ctx, "t1", first.WebhookEndpointID, first.ID)
	if err != nil {
		t.Fatalf("replay delivery: %v", err)
	}
	assertSentAs(t, client.last(t), orig)

	// Resending the resend still sends the root event.
	if _, err := svc.ReplayDelivery(ctx, "t1", first.WebhookEndpointID, res.DeliveryID); err != nil {
		t.Fatalf("replay of the replay's delivery: %v", err)
	}
	assertSentAs(t, client.last(t), orig)
}

// A replay whose first try fails is retried by the delivery worker, which
// loads the event again by id. The retry must send the root event too.
func TestReplay_RetriedReplaySendsRootEventIDAndCreatedAt(t *testing.T) {
	// 200 for the original send, 500 for the replay's first try, 200 for
	// its retry.
	store, client, svc, orig := replayFixture(t, 200, 500, 200)
	ctx := postgres.WithLivemode(context.Background(), true)

	if _, err := svc.Replay(ctx, "t1", orig.ID); err != nil {
		t.Fatalf("replay: %v", err)
	}
	replayed := store.deliveries[len(store.deliveries)-1]
	if replayed.Status != domain.DeliveryPending {
		t.Fatalf("replay delivery status = %s, want pending after a 500", replayed.Status)
	}
	past := time.Now().Add(-time.Minute)
	store.deliveries[len(store.deliveries)-1].NextRetryAt = &past

	if err := svc.RetryPendingDeliveries(ctx); err != nil {
		t.Fatalf("retry pending: %v", err)
	}
	if len(client.bodies) != 3 {
		t.Fatalf("POSTs sent = %d, want 3 (original, replay, retry)", len(client.bodies))
	}
	if got := store.deliveries[len(store.deliveries)-1].Status; got != domain.DeliverySucceeded {
		t.Fatalf("replay delivery status after retry = %s, want succeeded", got)
	}
	assertSentAs(t, client.last(t), orig)
}

// TestDelivery_RedirectIsAFailure: an endpoint URL that redirects (apex to
// www, an auth middleware's 302, a moved path) did not receive the event.
// Go's client follows a 301 or 302 as a GET with no body, and a 307 or 308
// as a second POST, so following one could mark an event delivered that the
// receiver never processed. Stripe: "We consider redirect responses to
// webhook requests as failures." The test runs the production client.
func TestDelivery_RedirectIsAFailure(t *testing.T) {
	for _, code := range []int{
		http.StatusMovedPermanently, http.StatusFound,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var movedHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/moved" {
					movedHits.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				http.Redirect(w, r, "/moved", code)
			}))
			defer srv.Close()

			store := newMemStore()
			svc := NewTestService(store, nil) // nil = the production HTTP client
			ctx := postgres.WithLivemode(context.Background(), true)
			if _, err := svc.CreateEndpoint(ctx, "t1", CreateEndpointInput{
				URL: srv.URL + "/hook", Events: []string{"invoice.paid"},
			}); err != nil {
				t.Fatalf("create endpoint: %v", err)
			}
			if err := svc.Dispatch(ctx, "t1", "invoice.paid", map[string]any{"invoice_id": "hc_inv_1"}); err != nil {
				t.Fatalf("dispatch: %v", err)
			}

			if len(store.deliveries) != 1 {
				t.Fatalf("deliveries = %d, want 1", len(store.deliveries))
			}
			d := store.deliveries[0]
			if d.Status != domain.DeliveryPending || d.NextRetryAt == nil {
				t.Errorf("status = %s, next_retry_at = %v; want pending with a retry scheduled", d.Status, d.NextRetryAt)
			}
			if d.AttemptCount != 1 {
				t.Errorf("attempt_count = %d, want 1", d.AttemptCount)
			}
			if d.HTTPStatusCode != code || d.ErrorMessage != fmt.Sprintf("HTTP %d", code) {
				t.Errorf("recorded status %d, error %q; want %d, %q", d.HTTPStatusCode, d.ErrorMessage, code, fmt.Sprintf("HTTP %d", code))
			}
			if n := movedHits.Load(); n != 0 {
				t.Errorf("the redirect target got %d requests, want 0: the client followed the redirect", n)
			}
		})
	}
}
