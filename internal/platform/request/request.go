// Package request holds one HTTP request's record: what the layers of the
// request learn about it, readable by every layer, inner or outer.
//
// A middleware that adds to the context hands its handler a new
// *http.Request, so a layer never sees what the layers inside it added. The
// access log wraps authentication and still has to name who made the
// request, and the metrics wrap the router and still have to name its
// route. So the root makes one record per request (Begin), the layer that
// learns a fact writes it there, and every layer reads it through Of.
//
// A fact about the request that more than one layer reads belongs on the
// record: its route, caller, client address, status and audit mark. What a
// callee needs to do its work stays on the context: the tenant, the mode,
// the instant.
package request

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// Route is the route a request matched: its method and pattern, which key
// the server's route table, and its path parameters. The Pattern is empty
// when the request matched no route.
type Route struct {
	Method  string
	Pattern string
	Params  *chi.Context
}

// Actor is who made a request, as the layer that authenticated it names
// them.
type Actor struct {
	// Type and ID name the caller as the audit log does (auth.Caller.Actor):
	// "user" and a user id, or "api_key" and a key id.
	Type string
	ID   string
	// TenantID is the caller's account.
	TenantID string
}

// Record is one request's record. Every field but the audit mark is written
// on the request's own goroutine. The client address is set at Begin and
// never changes, so a goroutine the handler started may read it; the audit
// mark is atomic because such a goroutine may write an audit row.
type Record struct {
	route    Route
	actor    Actor
	clientIP string
	writer   chimw.WrapResponseWriter
	// audited is the audit mark: the request's audit story is accounted
	// for, by a row that landed or by code that declared it changed
	// nothing (audit.MarkSkip).
	audited atomic.Bool
}

// Route is the route the request matched.
func (rec *Record) Route() Route { return rec.route }

// Actor is who made the request: the zero Actor when nobody signed in.
func (rec *Record) Actor() Actor { return rec.actor }

// ClientIP is the client's address: the one mw.TrustedClientIP resolved,
// which honours X-Forwarded-For only from a configured trusted proxy, or the
// TCP peer when nothing was resolved. It is never empty.
func (rec *Record) ClientIP() string { return rec.clientIP }

// Audited reports whether the request's audit story is accounted for
// (MarkAudited).
func (rec *Record) Audited() bool { return rec.audited.Load() }

// Status is the status the request was answered with, or 0 while nothing
// is written. Every layer reads it here, so no other layer wraps the writer
// to learn it.
func (rec *Record) Status() int { return rec.writer.Status() }

type ctxKey struct{}

// Begin returns the middleware that makes each request's record: its route,
// as route finds it, its client address, and the writer that records its
// status. The address is taken once, here, so Begin must be mounted below
// mw.TrustedClientIP; above it, every reader would get the TCP peer, one
// rate-limit bucket and one throttle key per proxy (TestRequestEdge_ClientIP).
// The writer never buffers: its Write passes through, and chi's typed wrappers
// keep http.Flusher, http.Hijacker and io.ReaderFrom, so a streaming route
// (the CSV exports) still streams instead of collecting its answer in
// memory.
func Begin(route func(*http.Request) Route) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			rec := &Record{route: route(r), clientIP: clientIP(r), writer: ww}
			next.ServeHTTP(ww, r.WithContext(context.WithValue(r.Context(), ctxKey{}, rec)))
		})
	}
}

// Of returns the request's record. Every request through the server has
// one (Begin), so a missing record is a mis-wired server: Of logs it and
// panics, and no reader can take "no record" for a fact.
func Of(ctx context.Context) *Record {
	rec, ok := ctx.Value(ctxKey{}).(*Record)
	if !ok {
		const msg = "request: no record on the context: request.Begin must be mounted above every reader"
		slog.ErrorContext(ctx, msg)
		panic(msg)
	}
	return rec
}

// SetActor names the request's actor on its record.
func SetActor(ctx context.Context, a Actor) { Of(ctx).actor = a }

// MarkAudited marks the request's audit story accounted for. Only the audit
// package may call this (internal/arch holds it). With no record it does
// nothing: a job or the CLI writes audit rows with no request, and nothing
// observes them.
func MarkAudited(ctx context.Context) {
	if rec, ok := ctx.Value(ctxKey{}).(*Record); ok {
		rec.audited.Store(true)
	}
}

// ClientIPIfAny is the request's client address, or "" with no record: a job
// or the CLI writes audit rows with no request, so no client. Only the audit
// package may call this (internal/arch holds it); inside a request use
// Of(ctx).ClientIP().
func ClientIPIfAny(ctx context.Context) string {
	if rec, ok := ctx.Value(ctxKey{}).(*Record); ok {
		return rec.clientIP
	}
	return ""
}

// clientIP is the address chi's ClientIPFrom* middleware resolved for r, or
// the TCP peer when it resolved none, so it is never empty. It never reads a
// forwarding header itself.
func clientIP(r *http.Request) string {
	if ip := chimw.GetClientIP(r.Context()); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
