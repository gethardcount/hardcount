// Package request carries what the inner layers of one HTTP request learn
// about it out to the layers that wrap them.
//
// A middleware that adds to the context hands its handler a new
// *http.Request, so a layer never sees what the layers inside it added. The
// access log wraps authentication and still has to name who made the
// request. So the outer layer puts one record on the context (WithRecord),
// the layer that learns a fact writes it there, and the outer layer reads it
// once the handler has returned.
//
// Use it only to tell a fact to a layer that wraps you. A fact for the
// layers inside goes on the context as usual (auth.WithTenantID).
package request

import "context"

// Actor is who made a request, as the layer that authenticated it names
// them.
type Actor struct {
	// ActorType and ActorID are the audit log's names for the caller
	// (auth.Caller.Actor): "user" with a user id, or "api_key" with a key id.
	ActorType string
	ActorID   string
	// TenantID is the account the caller signed in to.
	TenantID string
}

// record is one request's record. It has no lock: every write and read is on
// the request's own goroutine.
type record struct {
	actor Actor
}

type ctxKey struct{}

// WithRecord returns ctx holding a new, empty record for one request.
func WithRecord(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, &record{})
}

// SetActor names the request's actor on its record. On a context with no
// record it does nothing: no layer is waiting to read it.
func SetActor(ctx context.Context, a Actor) {
	if rec, ok := ctx.Value(ctxKey{}).(*record); ok {
		rec.actor = a
	}
}

// ActorOf returns the actor named on the request's record. It is the zero
// Actor when no layer named one: nobody signed in.
func ActorOf(ctx context.Context) Actor {
	if rec, ok := ctx.Value(ctxKey{}).(*record); ok {
		return rec.actor
	}
	return Actor{}
}
