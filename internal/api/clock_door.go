package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// clockDoor is "the door": one middleware, the last of every route group,
// that decides the time a request runs at before its handler runs, and binds
// it, so everything the request does reads one instant. That time is the
// test clock of the record the request's route names, or one read of the
// server's clock.
//
// The route's entry in clockRoutes says how to decide:
//   - real time, for a route that names no record on a clock: one read of
//     the server's clock;
//   - the clock of the customer, subscription or invoice its path names, or
//     of a credit note's or a dunning run's invoice, and its frozen time; a
//     record on no clock gets one read of the server's clock;
//   - a decidedIn entry: nothing here (real time with no instant in test
//     mode, as before); the named function decides and binds once it has
//     read its record.
//
// In live mode the door reads no record: test clocks exist only in test mode
// (a CHECK on test_clocks), so no live record is on one. A live request on a
// real-time or a record route gets one read of the server's clock; a
// decidedIn route gets nothing. A request with no mode on its context yet
// (the routes before authentication, whose mode comes from what they read)
// is decided as in test mode.
//
// A not-found is the record itself, or, in a race with a clock's deletion,
// its clock, after which the record is on no clock: real time is right in
// both, and the handler answers a missing record with its own 404 after its
// own checks. So does an empty id, or one Postgres cannot read (a NUL, or
// bytes that are not UTF-8): the door decides real time without a lookup. A
// record whose clock cannot be read is answered 503 (errs.Unavailable),
// which the client retries and the idempotency layer does not store; the
// handler does not run.
type clockDoor struct {
	// root is the built router: the door finds a request's route and its
	// path parameters with Find before the mounted sub-router runs. Set once
	// every route is registered.
	root     chi.Routes
	resolver clock.Resolver
	// clock is the server's clock, the router's: the door reads real time
	// from it once per request.
	clock clock.Clock
	// creditNoteInvoice and dunningRunInvoice read the invoice a credit note
	// or a dunning run belongs to.
	creditNoteInvoice func(ctx context.Context, tenantID, id string) (string, error)
	dunningRunInvoice func(ctx context.Context, tenantID, id string) (string, error)
}

// Middleware is the door. router.go says where it sits in each group, and
// why.
func (d *clockDoor) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decl, params, routed := d.route(r)
		live, err := postgres.Livemode(r.Context())
		live = err == nil && live
		switch {
		case !routed || decl.Source == clockDecidedIn:
			// No such route (the router answers 404 or 405 itself), or the
			// named function decides and binds once it has read its record.
			if live {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(clock.WithRealTime(r.Context())))
			return
		case live:
			// No live record is on a test clock: one read of the server's
			// clock, and no lookup.
			next.ServeHTTP(w, r.WithContext(clock.EnsureEffectiveNow(r.Context(), d.clock)))
			return
		}
		ctx := clock.WithRealTime(r.Context())
		pin, record, err := d.pinFor(r.Context(), decl, params)
		if err == nil && pin != (clock.Pin{}) {
			ctx, err = clock.Decide(r.Context(), d.resolver, pin)
		}
		switch {
		case errors.Is(err, errs.ErrNotFound):
			ctx = clock.WithRealTime(r.Context())
		case err != nil:
			respond.FromError(w, r, errs.Unavailable("test_clock_unavailable",
				fmt.Sprintf("The test clock of %s could not be read. Nothing was done; send the request again.", record), err), "record")
			return
		}
		// The record's clock carries its instant; real time is one read of
		// the server's clock.
		next.ServeHTTP(w, r.WithContext(clock.EnsureEffectiveNow(ctx, d.clock)))
	})
}

// route finds the request's entry in clockRoutes and its path parameters,
// reading nothing: false when the router has no such route.
func (d *clockDoor) route(r *http.Request) (clockDecl, *chi.Context, bool) {
	rctx := chi.NewRouteContext()
	pattern := canonicalRoute(d.root.Find(rctx, r.Method, routedPath(r)))
	decl, ok := clockRoutes[routeKey{Method: r.Method, Pattern: pattern}]
	return decl, rctx, ok
}

// pinFor works out the record whose clock decides a test-mode request on a
// real-time or a record route: its pin, and its label for the error answer.
// The zero pin means real time: the route names no record on a clock, or the
// id is empty or one Postgres cannot read.
func (d *clockDoor) pinFor(ctx context.Context, entry clockDecl, params *chi.Context) (clock.Pin, string, error) {
	tenantID, id := auth.TenantID(ctx), params.URLParam(entry.Param)
	if entry.Param != "" && !readableID(id) {
		return clock.Pin{}, "", nil
	}
	switch entry.Source {
	case clockOfCustomer:
		return clock.Pin{TenantID: tenantID, CustomerID: id}, "customer " + id, nil
	case clockOfSubscription:
		return clock.Pin{TenantID: tenantID, SubscriptionID: id}, "subscription " + id, nil
	case clockOfInvoice:
		return clock.Pin{TenantID: tenantID, InvoiceID: id}, "invoice " + id, nil
	// A credit note and a dunning run have no clock of their own; their
	// invoice has.
	case clockOfCreditNote:
		invoiceID, err := d.creditNoteInvoice(ctx, tenantID, id)
		return clock.Pin{TenantID: tenantID, InvoiceID: invoiceID}, "credit note " + id, err
	case clockOfDunningRun:
		invoiceID, err := d.dunningRunInvoice(ctx, tenantID, id)
		return clock.Pin{TenantID: tenantID, InvoiceID: invoiceID}, "dunning run " + id, err
	default: // real time
		return clock.Pin{}, "", nil
	}
}

// readableID reports whether id can name a record: not empty, and text
// Postgres can read (UTF-8 with no NUL). chi matches an empty parameter in
// the middle of a path (/v1/subscriptions//cancel), and an escaped NUL or
// stray byte reaches the parameter decoded. A read with such an id fails
// with an error that is not a not-found, which would be a 503 a client
// retries for ever; the handler answers it instead, as it would without the
// door.
func readableID(id string) bool {
	return id != "" && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

// routedPath is the path chi routes the request on: the escaped path when
// the URL has one, so a path parameter holding an escaped slash finds the
// same route, with the same parameter, as the handler.
func routedPath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	return r.URL.Path
}
