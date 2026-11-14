package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// The webhook event stream re-checks its credential every interval. These
// tests serve the stream as router.go mounts it (eventStreamRoute) with a
// 20ms interval in place of webhook.StreamHeartbeatInterval, beside the full
// server, which issues and revokes the credentials.
const testStreamInterval = 20 * time.Millisecond

type streamRig struct {
	srv      *Server
	api      *httptest.Server // the full server
	stream   *httptest.Server // the stream alone, with the short interval
	tenantID string
	owner    *http.Cookie
}

// newStreamRig builds the rig. The stream authenticates on authDB, which can
// be a second pool to the same database so a test can close it.
func newStreamRig(t *testing.T, db, authDB *postgres.DB) *streamRig {
	t.Helper()
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "stream-credential-bootstrap")
	srv := NewServer(db, clock.Real(), nil)
	api := httptest.NewServer(srv)
	t.Cleanup(api.Close)

	const email, password = "owner@stream-credential.test", "stream-credential-pw-12"
	tenantID := flowStr(flowBootstrap(t, api, "stream-credential-bootstrap", email, password), "tenant", "id")

	r := chi.NewRouter()
	noRateLimit := func(next http.Handler) http.Handler { return next }
	r.Route("/v1/webhook_events/stream", eventStreamRoute(webhook.NewHandler(srv.WebhookOutSvc),
		session.NewService(session.NewPostgresStore(authDB)), auth.NewService(auth.NewPostgresStore(authDB)),
		noRateLimit, testStreamInterval))
	stream := httptest.NewServer(r)
	t.Cleanup(stream.Close)

	return &streamRig{srv: srv, api: api, stream: stream, tenantID: tenantID, owner: login(t, api, email, password)}
}

func (rig *streamRig) createKey(t *testing.T) (id, raw string) {
	t.Helper()
	created := flowDo(t, rig.api, "POST", "/v1/api-keys", flowReq{cookie: rig.owner,
		body: map[string]any{"name": "Stream", "key_type": "secret"}}).want(t, 201, "create key").body
	return flowStr(created, "key", "id"), flowStr(created, "raw_key")
}

// open opens the stream with the credential setAuth adds and returns its
// lines; the channel closes when the stream ends.
func (rig *streamRig) open(t *testing.T, setAuth func(*http.Request)) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", rig.stream.URL+"/v1/webhook_events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	setAuth(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("open the stream: status %d, want 200", res.StatusCode)
	}
	lines := make(chan string, 4096)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func (rig *streamRig) publish(id string) {
	rig.srv.WebhookOutSvc.EventBus().Publish(rig.tenantID,
		webhook.StreamFrame{EventID: id, EventType: "customer.created", Status: "pending", CreatedAt: time.Now().UTC()})
}

// waitForFrame returns once a frame for id arrives. publishing republishes it
// until then: the stream subscribes only after its snapshot, so a frame
// published before that is not delivered.
func (rig *streamRig) waitForFrame(t *testing.T, lines <-chan string, id string, publishing bool) {
	t.Helper()
	rig.publish(id)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, open := <-lines:
			if !open {
				t.Fatalf("the stream ended before the frame for %s arrived", id)
			}
			if strings.Contains(l, id) {
				return
			}
		case <-time.After(100 * time.Millisecond):
			if publishing {
				rig.publish(id)
			}
		case <-deadline:
			t.Fatalf("no frame for %s in 10s", id)
		}
	}
}

func waitClosed(t *testing.T, lines <-chan string, why string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-lines:
			if !open {
				return
			}
		case <-deadline:
			t.Fatalf("the stream is still open 5s after %s (the check runs every %s)", why, testStreamInterval)
		}
	}
}

func TestE2E_EventStream_ClosesWhenItsKeyIsRevoked(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rig := newStreamRig(t, db, db)
	keyID, rawKey := rig.createKey(t)
	lines := rig.open(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+rawKey) })
	rig.waitForFrame(t, lines, "evt_before_revoke", true)

	flowDo(t, rig.api, "DELETE", "/v1/api-keys/"+keyID, flowReq{cookie: rig.owner}).want(t, 200, "revoke the stream's key")
	waitClosed(t, lines, "its key was revoked")
}

func TestE2E_EventStream_ClosesWhenItsSessionEnds(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rig := newStreamRig(t, db, db)
	lines := rig.open(t, func(r *http.Request) { r.AddCookie(rig.owner) })
	rig.waitForFrame(t, lines, "evt_before_logout", true)

	flowDo(t, rig.api, "POST", "/v1/auth/logout", flowReq{cookie: rig.owner}).want(t, 204, "log out the stream's session")
	waitClosed(t, lines, "its session ended")
}

// A check that reaches no verdict does not end the stream. The stream's
// credential store goes down after it opens (its pool is closed); the next
// checks fail, and the stream still delivers.
func TestE2E_EventStream_StaysOpenWhenTheCheckReachesNoVerdict(t *testing.T) {
	db := testutil.SetupTestDB(t)
	authDB := testutil.OpenTracedDB(t, nil)
	rig := newStreamRig(t, db, authDB)
	_, rawKey := rig.createKey(t)
	lines := rig.open(t, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+rawKey) })
	rig.waitForFrame(t, lines, "evt_before_outage", true)

	_ = authDB.Pool.Close()
	// A heartbeat follows each check. The first after the close may come from
	// a check that began before it, so wait for two.
	for heartbeats := 0; heartbeats < 2; {
		select {
		case l, open := <-lines:
			if !open {
				t.Fatal("the stream ended when its credential store went down")
			}
			if l == ": heartbeat" {
				heartbeats++
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no heartbeat in 10s")
		}
	}
	rig.waitForFrame(t, lines, "evt_during_outage", false)
}
