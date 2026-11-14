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
// that decides the time a request runs at before its handler runs. That time
// is the test clock of the record the request's route names, or real time.
//
// In live mode the door does nothing. Test clocks exist only in test mode (a
// CHECK on test_clocks), so no live record is on one: live time is real time,
// and live traffic pays for no lookup. A request with no mode on its context
// yet (the routes before authentication, whose mode comes from what they
// read) is decided as in test mode.
//
// In test mode the route's entry in clockRoutes says how to decide:
//   - real time, for a route that names no record on a clock;
//   - the clock of the customer, subscription or invoice its path names, or
//     of a credit note's or a dunning run's invoice; a record on no clock
//     gets real time;
//   - a decidedIn entry: real time here; the named function decides once it
//     has read its record.
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
	// creditNoteInvoice and dunningRunInvoice read the invoice a credit note
	// or a dunning run belongs to.
	creditNoteInvoice func(ctx context.Context, tenantID, id string) (string, error)
	dunningRunInvoice func(ctx context.Context, tenantID, id string) (string, error)
}

// Middleware is the door. router.go says where it sits in each group, and
// why.
func (d *clockDoor) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if live, err := postgres.Livemode(r.Context()); err == nil && live {
			next.ServeHTTP(w, r)
			return
		}
		ctx := clock.WithRealTime(r.Context())
		pin, record, err := d.pinFor(r)
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
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// pinFor works out the record whose clock decides the request: its pin, and
// its label for the error answer. The zero pin means real time: the route
// names no record on a clock, its entry is decidedIn, the router has no such
// route, or the id is empty or one Postgres cannot read.
func (d *clockDoor) pinFor(r *http.Request) (clock.Pin, string, error) {
	ctx := r.Context()
	rctx := chi.NewRouteContext()
	pattern := canonicalRoute(d.root.Find(rctx, r.Method, routedPath(r)))
	entry, ok := clockRoutes[routeKey{Method: r.Method, Pattern: pattern}]
	if !ok {
		// No such route: the router answers 404 or 405 itself.
		return clock.Pin{}, "", nil
	}
	tenantID, id := auth.TenantID(ctx), rctx.URLParam(entry.Param)
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
	default: // real time, and decidedIn entries
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
