package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// clockDoor decides a request's time once, before its handler runs
// (test-clock step 3), from the route's declaration in clockRoutes.
//
// In live mode it does nothing: no record there is on a test clock (a
// database CHECK), so live time is real time by rule, and live traffic pays
// for no lookup. In test mode, or with no mode yet (the unauthenticated
// routes, whose mode comes from what they read), it decides:
//   - real time, for a route that names no record on a clock;
//   - the clock of the customer, subscription or invoice the route's path
//     parameter names, or of a credit note's or dunning run's invoice;
//   - real time for a decidedIn route, whose named function decides again
//     from the record it reads.
//
// A record that does not exist is the handler's to answer, as it always
// has (its own 404, after its own checks): the door decides real time, and
// no write can be dated on a record that is not there. A record whose clock
// cannot be read for any other reason answers the error, so the request
// does not go on at a time the door did not decide.
type clockDoor struct {
	// root is the built router: the door finds a request's route pattern and
	// path parameters with Find before the mounted sub-router runs. Set once
	// every route is registered.
	root     chi.Routes
	resolver clock.Resolver
	// creditNoteInvoice and dunningRunInvoice read the invoice a credit note
	// or a dunning run belongs to.
	creditNoteInvoice func(ctx context.Context, tenantID, id string) (string, error)
	dunningRunInvoice func(ctx context.Context, tenantID, id string) (string, error)
}

// Middleware is the door. It is mounted after each group's timeout, so its
// lookups are bounded, a rate-limited request does not pay for them, and an
// idempotent replay is answered from the cache before any lookup.
func (d *clockDoor) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if live, err := postgres.Livemode(r.Context()); err == nil && live {
			next.ServeHTTP(w, r)
			return
		}
		ctx, err := d.decide(r)
		if errors.Is(err, errs.ErrNotFound) {
			ctx, err = clock.WithRealTime(r.Context()), nil
		}
		if err != nil {
			respond.FromError(w, r, err, "record")
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (d *clockDoor) decide(r *http.Request) (context.Context, error) {
	ctx := r.Context()
	rctx := chi.NewRouteContext()
	pattern := canonicalRoute(d.root.Find(rctx, r.Method, r.URL.Path))
	decl, ok := clockRoutes[routeKey{Method: r.Method, Pattern: pattern}]
	if !ok {
		// No such route: the router answers 404 or 405 itself.
		return clock.WithRealTime(ctx), nil
	}
	tenantID, id := auth.TenantID(ctx), rctx.URLParam(decl.Param)
	switch decl.Source {
	case clockOfCustomer:
		return clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, CustomerID: id})
	case clockOfSubscription:
		return clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, SubscriptionID: id})
	case clockOfInvoice:
		return clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, InvoiceID: id})
	case clockOfCreditNote, clockOfDunningRun:
		invoiceOf := d.creditNoteInvoice
		if decl.Source == clockOfDunningRun {
			invoiceOf = d.dunningRunInvoice
		}
		invoiceID, err := invoiceOf(ctx, tenantID, id)
		if err != nil {
			return ctx, err
		}
		return clock.Decide(ctx, d.resolver, clock.Pin{TenantID: tenantID, InvoiceID: invoiceID})
	default: // real time, and decidedIn routes until their function decides
		return clock.WithRealTime(ctx), nil
	}
}
