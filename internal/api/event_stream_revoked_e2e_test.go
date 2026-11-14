package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// The webhook event stream is authenticated when it opens and again before
// each frame it sends: a stream opened with a key ends, with no further
// frame, once the key is revoked.
func TestE2E_EventStream_EndsWhenItsKeyIsRevoked(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "stream-revoke-bootstrap")
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	const email, password = "owner@stream-revoke.test", "stream-revoke-pw-12"
	tenantID := flowStr(flowBootstrap(t, ts, "stream-revoke-bootstrap", email, password), "tenant", "id")
	owner := login(t, ts, email, password)
	created := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner,
		body: map[string]any{"name": "Stream", "key_type": "secret"}}).want(t, 201, "create key").body
	keyID, rawKey := flowStr(created, "key", "id"), flowStr(created, "raw_key")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/webhook_events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+rawKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("open the stream: status %d, want 200", res.StatusCode)
	}
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	frame := func(id string) webhook.StreamFrame {
		return webhook.StreamFrame{EventID: id, EventType: "customer.created", Status: "pending", CreatedAt: time.Now().UTC()}
	}

	// The stream subscribes after its snapshot, so publish until a frame
	// comes through.
	deadline := time.After(10 * time.Second)
	for subscribed := false; !subscribed; {
		srv.WebhookOutSvc.EventBus().Publish(tenantID, frame("evt_before_revoke"))
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the stream ended before its key was revoked")
			}
			subscribed = strings.Contains(l, "evt_before_revoke")
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("no frame came through the stream in 10s")
		}
	}

	flowDo(t, ts, "DELETE", "/v1/api-keys/"+keyID, flowReq{cookie: owner}).want(t, 200, "revoke the stream's key")
	srv.WebhookOutSvc.EventBus().Publish(tenantID, frame("evt_after_revoke"))

	timeout := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return // the stream ended
			}
			if strings.Contains(l, "evt_after_revoke") {
				t.Fatal("the stream sent a frame after its key was revoked")
			}
		case <-timeout:
			t.Fatal("the stream is still open 5s after its key was revoked")
		}
	}
}
