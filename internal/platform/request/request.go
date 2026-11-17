// Package request holds one HTTP request's record: what the layers of the
// request learn about it, readable by every layer, inner or outer.
//
// A middleware that adds to the context hands its handler a new
// *http.Request, so a layer never sees what the layers inside it added. The
// access log wraps authentication and still has to name who made the
// request, and the metrics wrap the router and still have to name its
// route. So the root makes one record per request (Begin), the layer that
// learns a fact writes it there, and every layer reads it through Of.
package request

import (
	"context"
	"log/slog"
	"net/http"

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

// Record is one request's record. It has no lock: every write and read is
// on the request's own goroutine.
type Record struct {
	route  Route
	actor  Actor
	writer chimw.WrapResponseWriter
}

// Route is the route the request matched.
func (rec *Record) Route() Route { return rec.route }

// Actor is who made the request: the zero Actor when nobody signed in.
func (rec *Record) Actor() Actor { return rec.actor }

// Status is the status the request was answered with, or 0 while nothing
// is written. Every layer reads it here, so no other layer wraps the writer
// to learn it.
func (rec *Record) Status() int { return rec.writer.Status() }

type ctxKey struct{}

// Begin returns the root middleware that makes each request's record: its
// route, as route finds it, and the writer that records its status. That
// writer never buffers: its Write passes through, and chi's typed wrappers
// keep http.Flusher, http.Hijacker and io.ReaderFrom, so a streaming route
// (the CSV exports) still streams instead of collecting its answer in
// memory.
func Begin(route func(*http.Request) Route) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			rec := &Record{route: route(r), writer: ww}
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
		const msg = "request: no record on the context: request.Begin must be the root middleware, above every reader"
		slog.ErrorContext(ctx, msg)
		panic(msg)
	}
	return rec
}

// SetActor names the request's actor on its record.
func SetActor(ctx context.Context, a Actor) { Of(ctx).actor = a }
