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
// that runs before a request's handler. On a real-time route and a record's
// route it decides the request's time and binds it, so everything the
// request does reads one instant. On a decidedIn route it binds nothing: the
// route names its record in the body or by a token, so the door cannot know
// that record's clock, and the named function reads the record, then decides
// and binds.
//
// It decides in this order:
//   - It finds the request's route in the router (Find: an in-memory lookup,
//     no database read) and the route's entry in clockRoutes.
//   - A route the router does not have, and a decidedIn route: in test mode
//     the door records real time with no instant (clock.WithRealTime, one of
//     the states named on clock.Decision); in live mode it leaves the
//     context as it is. The router answers an unknown route with 404 or 405.
//   - Live mode, on a real-time or a record route: a real instant, one read
//     of the server's clock, and no record is read. Test clocks exist only
//     in test mode (a CHECK on test_clocks), so no live record is on one.
//   - Test mode, on a real-time route (one that names no record on a
//     clock): a real instant, one read of the server's clock.
//   - Test mode, on a record's route (the customer, subscription or invoice
//     its path names, or a credit note's or a dunning run's invoice): that
//     record's test clock and its frozen time, with no read of the server's
//     clock. A record on no clock, a not-found, or an empty id or one
//     Postgres cannot read (a NUL, or bytes that are not UTF-8): a real
//     instant, one read of the server's clock.
//
// A request with no mode on its context yet (the routes before
// authentication, whose mode comes from what they read) is decided as in
// test mode.
//
// A not-found is the record itself, or, in a race with a clock's deletion,
// its clock, after which the record is on no clock: a real instant is right
// in both, and the handler answers a missing record with its own 404 after
// its own checks. A record whose clock cannot be read is answered 503
// (errs.Unavailable), which the client retries and the idempotency layer
// does not store; nothing is bound, and the handler does not run.
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
	return doorHandler{door: d, next: next}
}

// doorHandler is the clock door: it decides the request's time from the
// record its route names and binds it on the context (clockDoor).
// clockDoor.Middleware makes it.
type doorHandler struct {
	door *clockDoor
	next http.Handler
}

func (h doorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	decl, params, routed := h.door.route(r)
	// No mode on the context yet (a route before authentication) is
	// treated as test mode.
	live, err := postgres.Livemode(r.Context())
	live = err == nil && live
	if !routed || decl.Source == clockDecidedIn {
		// The router answers an unknown route itself; a decidedIn
		// route's function decides and binds once it has read its
		// record.
		if !live {
			r = r.WithContext(clock.WithRealTime(r.Context()))
		}
		h.next.ServeHTTP(w, r)
		return
	}
	if live {
		// No live record is on a test clock: a real instant, one read of
		// the server's clock, and no record is read.
		h.next.ServeHTTP(w, r.WithContext(clock.EnsureEffectiveNow(r.Context(), h.door.clock)))
		return
	}
	ctx := clock.WithRealTime(r.Context())
	pin, record, err := h.door.pinFor(r.Context(), decl, params)
	if err == nil && pin != (clock.Pin{}) {
		ctx, err = clock.Decide(r.Context(), h.door.resolver, pin)
	}
	switch {
	case errors.Is(err, errUnknownClockSource):
		// A route table entry the door does not know: a server fault.
		respond.InternalError(w, r)
		return
	case errors.Is(err, errs.ErrNotFound):
		ctx = clock.WithRealTime(r.Context())
	case err != nil:
		// The clock could not be read: refuse, so the work never runs on
		// a time nobody decided.
		respond.FromError(w, r, errs.Unavailable("test_clock_unavailable",
			fmt.Sprintf("The test clock of %s could not be read. Nothing was done; send the request again.", record), err), "record")
		return
	}
	// A record's clock carries its instant; otherwise a real instant,
	// one read of the server's clock.
	h.next.ServeHTTP(w, r.WithContext(clock.EnsureEffectiveNow(ctx, h.door.clock)))
}

// route finds the request's entry in clockRoutes and its path parameters,
// reading nothing: false when the router has no such route.
func (d *clockDoor) route(r *http.Request) (clockDecl, *chi.Context, bool) {
	rctx := chi.NewRouteContext()
	pattern := canonicalRoute(d.root.Find(rctx, r.Method, routedPath(r)))
	decl, ok := clockRoutes[routeKey{Method: r.Method, Pattern: pattern}]
	return decl, rctx, ok
}

// errUnknownClockSource is pinFor's answer for a route table entry whose
// source the door does not know: a fault in the table or the door, answered
// as a server error, never as a clock that could not be read.
var errUnknownClockSource = errors.New("clock door: unknown clock source")

// pinFor works out the record whose clock decides a test-mode request on a
// real-time or a record route: its pin, and its label for the error answer.
// The zero pin means a real instant: the route names no record on a clock,
// or the id is empty or one Postgres cannot read.
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
	case clockRealTime:
		return clock.Pin{}, "", nil
	default:
		return clock.Pin{}, "", fmt.Errorf("%w: %d", errUnknownClockSource, entry.Source)
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
