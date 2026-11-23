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
	"github.com/gethardcount/hardcount/internal/platform/request"
)

// clockDoor is the door: the last step of every group, which binds the
// instant a request runs on before its handler runs. docs/architecture.md,
// "The path of a request", says why it ends every group. The instant is the
// time on the context that clock.Now and every Clock's Now(ctx) return, so
// all the work of one request reads one time. By the request's entry in
// routeTable, the door puts on the context:
//
//   - real time: one read of the server's clock, in either mode.
//   - a record's clock: in test mode, the clock the record is on and its
//     instant, or one read when the record is on none, is not found, or has
//     an id Postgres cannot read; in live mode, one read and no lookup.
//   - decided in a named function: no instant. In test mode it marks real
//     time decided (clock.WithRealTime) until that function decides; in live
//     mode it puts nothing.
//   - no entry, which the router answers 404 or 405: as decided in.
//
// A request with no mode yet counts as test mode. A record's clock that
// cannot be read answers 503, which the client retries and Idempotency does
// not keep, and the handler does not run.
//
// The door also marks a request that changes data (clock.MarkChange): a
// signed-in request, not GET or HEAD, on a route that decides a record's
// clock or decides in a named function. The decision then refuses it while
// the record's clock advances (409 clock_advancing): here on a record route,
// in the named function otherwise. The decision drops the mark, so work the
// request calls, or starts after it answers, is never refused.
type clockDoor struct {
	resolver clock.Resolver
	// clock is the server's clock: the door reads real time from it at most
	// once per request.
	clock clock.Clock
	// creditNoteInvoice and dunningRunInvoice read the invoice a credit note
	// or a dunning run belongs to.
	creditNoteInvoice func(ctx context.Context, tenantID, id string) (string, error)
	dunningRunInvoice func(ctx context.Context, tenantID, id string) (string, error)
}

func (d *clockDoor) Middleware(next http.Handler) http.Handler {
	return doorHandler{door: d, next: next}
}

// doorHandler runs the clock door in front of next; clockDoor has the rule.
// clockDoor.Middleware makes it.
type doorHandler struct {
	door *clockDoor
	next http.Handler
}

func (h doorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	entry, routed := routeTable[routeKeyOf(r.Context())]
	decl, params := entry.clock, request.Of(r.Context()).Route().Params
	live, err := postgres.Livemode(r.Context())
	live = err == nil && live
	if routed && !entry.outsideAuth && decl.Source != clockRealTime && r.Method != http.MethodGet && r.Method != http.MethodHead {
		r = r.WithContext(clock.MarkChange(r.Context()))
	}
	// No entry means no route serves the request (mustListEveryRoute).
	if !routed || decl.Source == clockDecidedIn {
		if !live {
			r = r.WithContext(clock.WithRealTime(r.Context()))
		}
		h.next.ServeHTTP(w, r)
		return
	}
	if live {
		// Test clocks exist only in test mode (a CHECK on test_clocks).
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
		respond.InternalError(w, r)
		return
	case clock.IsAdvancingRefusal(err):
		respond.FromError(w, r, err, "record")
		return
	case errors.Is(err, errs.ErrNotFound):
		// The record, or in a race with its clock's deletion the clock: the
		// handler answers a missing record with its own 404.
		ctx = clock.WithRealTime(r.Context())
	case err != nil:
		// The clock could not be read: refuse, so the work never runs on
		// a time nobody decided.
		respond.FromError(w, r, errs.Unavailable("test_clock_unavailable",
			fmt.Sprintf("The test clock of %s could not be read. Nothing was done; send the request again.", record), err), "record")
		return
	}
	h.next.ServeHTTP(w, r.WithContext(clock.EnsureEffectiveNow(ctx, h.door.clock)))
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
// Postgres can read (UTF-8 with no NUL). chi matches an empty parameter
// (/v1/subscriptions//cancel) and decodes an escaped NUL or stray byte into
// it. A read with such an id fails with an error that is not a not-found, a
// 503 a client would retry for ever, so the door leaves it to the handler.
func readableID(id string) bool {
	return id != "" && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}
