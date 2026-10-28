package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
)

// capture collects the attributes of the one "request" record requestLogger emits.
type capture struct {
	slog.Handler
	attrs map[string]string
	msg   string
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.msg = r.Message
	r.Attrs(func(a slog.Attr) bool {
		c.attrs[a.Key] = a.Value.String()
		return true
	})
	return nil
}
func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

// serveThroughLogger runs one request through requestLogger. The identity is stamped
// INSIDE the handler, mirroring production: auth middleware runs downstream of the
// logger, so at the moment requestLogger is entered the context is still anonymous.
// A logger that read identity before calling the next handler would log nothing —
// which is exactly the bug that would make this test pass while the field stays
// empty in prod.
func serveThroughLogger(t *testing.T, stamp func(context.Context) context.Context) *capture {
	t.Helper()
	cap := &capture{attrs: map[string]string{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*r = *r.WithContext(stamp(r.Context()))
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/invoices", nil)
	req = req.WithContext(context.WithValue(req.Context(), chimw.RequestIDKey, "hc_req_abc123"))
	h.ServeHTTP(httptest.NewRecorder(), req)
	return cap
}

// TestAccessLog_RecordsWhoNotJustWhat is the regression lock for an access log that
// could not answer any question worth asking.
//
// It logged method, path, status and duration — and nothing else. So after an
// incident it could tell you a 500 happened on POST /v1/invoices, and it could not
// tell you which tenant was affected, which operator did it, or from where. Worse,
// with no request_id on the line, a log entry could not be joined to the audit row
// the same request had just written — even though the audit row has carried
// request_id all along. The two halves of the evidence existed and could not be
// put together.
func TestAccessLog_RecordsWhoNotJustWhat(t *testing.T) {
	cap := serveThroughLogger(t, func(ctx context.Context) context.Context {
		ctx = auth.WithTenantID(ctx, "hc_ten_42")
		ctx = auth.WithUserID(ctx, "hc_usr_7")
		return audit.WithClientIP(ctx, "203.0.113.9")
	})

	if cap.msg != "request" {
		t.Fatalf("log message = %q, want %q", cap.msg, "request")
	}
	for key, want := range map[string]string{
		"tenant_id":  "hc_ten_42",
		"actor_id":   "hc_usr_7",
		"actor_type": "user",
		"request_id": "hc_req_abc123", // the join key to the audit row
		"ip":         "203.0.113.9",
		"status":     "200",
		"method":     "POST",
	} {
		if got := cap.attrs[key]; got != want {
			t.Errorf("access log %s = %q, want %q", key, got, want)
		}
	}
}

// TestAccessLog_AgreesWithTheAuditRowsActor pins the two to ONE resolver.
//
// The access log and the audit row must never disagree about who made a request.
// If the log resolved identity its own way, a request could be one actor in the log
// and a different actor in the evidence — and the log is what an investigator reaches
// for first. Both call audit.ResolveActor; this fails if someone forks that.
func TestAccessLog_AgreesWithTheAuditRowsActor(t *testing.T) {
	// An API key, not a user: the precedence order inside ResolveActor is the part
	// most likely to be reimplemented subtly differently by a second copy.
	stamp := func(ctx context.Context) context.Context {
		return auth.WithKeyID(auth.WithTenantID(ctx, "hc_ten_1"), "hc_key_9")
	}
	cap := serveThroughLogger(t, stamp)

	wantType, wantID := audit.ResolveActor(stamp(context.Background()))
	if cap.attrs["actor_type"] != wantType || cap.attrs["actor_id"] != wantID {
		t.Errorf("access log actor = %s/%s, audit row would record %s/%s — the log and the evidence disagree about who did this",
			cap.attrs["actor_type"], cap.attrs["actor_id"], wantType, wantID)
	}
}

// TestAccessLog_NeverLogsAnActorName keeps emails out of the log sink.
//
// Actor names are emails for user actors. Logs ship to sinks with no erasure story,
// which is the same reason the append-only audit_log stores no addresses (0150
// revoked DELETE, so anything written there is unerasable by construction). The log
// carries the actor ID; the name is resolved at read time, from a table that can
// still honour a deletion request.
func TestAccessLog_NeverLogsAnActorName(t *testing.T) {
	cap := serveThroughLogger(t, func(ctx context.Context) context.Context {
		return auth.WithUserID(auth.WithTenantID(ctx, "hc_ten_1"), "hc_usr_7")
	})
	for k, v := range cap.attrs {
		if strings.Contains(v, "@") {
			t.Errorf("access log attr %s = %q looks like an email address — logs have no erasure path", k, v)
		}
	}
	if _, ok := cap.attrs["actor_name"]; ok {
		t.Error("access log records actor_name; names are emails, and this sink cannot erase them")
	}
}

// TestAccessLog_OmitsIdentityItDoesNotHave: an unauthenticated request has no
// tenant. Logging an empty string reads as "we lost it"; omitting the key reads as
// "there wasn't one", which is the truth.
//
// It DOES have an actor — ResolveActor answers system/system — so actor stays on the
// line. That asymmetry is the point: omit what does not exist, log what does.
func TestAccessLog_OmitsIdentityItDoesNotHave(t *testing.T) {
	cap := serveThroughLogger(t, func(ctx context.Context) context.Context { return ctx })

	if _, ok := cap.attrs["tenant_id"]; ok {
		t.Errorf("anonymous request logged tenant_id = %q, want the key omitted", cap.attrs["tenant_id"])
	}
	// actor_id/actor_type still resolve: "system" is a real answer, not a missing
	// one, and it is exactly what the audit row would record for the same request.
	// (An `if actorID != ""` guard here would be dead code — ResolveActor never
	// returns empty — and a branch that cannot run is a gate that cannot hold.)
	if cap.attrs["actor_id"] != "system" {
		t.Errorf("anonymous actor_id = %q, want system", cap.attrs["actor_id"])
	}
	if cap.attrs["actor_type"] != "system" {
		t.Errorf("actor_type = %q, want system", cap.attrs["actor_type"])
	}
	if cap.attrs["request_id"] == "" {
		t.Error("request_id missing — every line must be joinable, authenticated or not")
	}
}
