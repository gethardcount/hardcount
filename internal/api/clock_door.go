package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// clockDoor decides the time a request runs at, once, before its handler:
// the test clock of the record its route names, or real time. A request with
// no mode on its context yet (the routes before authentication, whose mode
// comes from what they read) is decided as in test mode.
//
// In live mode the door does nothing. Test clocks exist only in test mode (a
// CHECK on test_clocks), so no live record is on one: live time is real time,
// and live traffic pays for no lookup.
//
// In test mode the route's entry in clockRoutes says how to decide:
//   - real time, for a route that names no record on a clock;
//   - the clock of the customer, subscription or invoice its path names, or
//     of a credit note's or a dunning run's invoice;
//   - real time for a route whose named function decides again from the
//     record it reads (decidedIn).
//
// A record that does not exist is the handler's to answer, with its own 404
// after its own checks: the door decides real time, and nothing can be dated
// on a record that is not there. A record whose clock cannot be read is
// answered 503 (errs.Unavailable), which the client retries and the
// idempotency layer does not store; the handler does not run.
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
		ctx, record, err := d.decide(r)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			ctx = clock.WithRealTime(r.Context())
		case err != nil:
			respond.FromError(w, r, errs.Unavailable("test_clock_unavailable",
				fmt.Sprintf("could not read the test clock of %s; retry", record), err), "record")
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// decide returns the ctx carrying the request's decision, and the record it
// was decided from, for the error answer.
func (d *clockDoor) decide(r *http.Request) (context.Context, string, error) {
	ctx := r.Context()
	rctx := chi.NewRouteContext()
	pattern := canonicalRoute(d.root.Find(rctx, r.Method, routedPath(r)))
	entry, ok := clockRoutes[routeKey{Method: r.Method, Pattern: pattern}]
	if !ok {
		// No such route: the router answers 404 or 405 itself.
		return clock.WithRealTime(ctx), "", nil
	}
	tenantID, id := auth.TenantID(ctx), rctx.URLParam(entry.Param)
	switch entry.Source {
	case clockOfCustomer:
		ctx, err := clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, CustomerID: id})
		return ctx, "customer " + id, err
	case clockOfSubscription:
		ctx, err := clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, SubscriptionID: id})
		return ctx, "subscription " + id, err
	case clockOfInvoice:
		ctx, err := clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, InvoiceID: id})
		return ctx, "invoice " + id, err
	// A credit note and a dunning run have no clock of their own; their
	// invoice has.
	case clockOfCreditNote:
		return d.decideByInvoiceOf(ctx, d.creditNoteInvoice, tenantID, id, "credit note "+id)
	case clockOfDunningRun:
		return d.decideByInvoiceOf(ctx, d.dunningRunInvoice, tenantID, id, "dunning run "+id)
	default: // real time, and decidedIn routes until their function decides
		return clock.WithRealTime(ctx), "", nil
	}
}

// decideByInvoiceOf decides from the invoice that invoiceOf reads for the
// record id.
func (d *clockDoor) decideByInvoiceOf(ctx context.Context, invoiceOf func(context.Context, string, string) (string, error), tenantID, id, record string) (context.Context, string, error) {
	invoiceID, err := invoiceOf(ctx, tenantID, id)
	if err != nil {
		return ctx, record, err
	}
	ctx, err = clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, InvoiceID: invoiceID})
	return ctx, record, err
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
