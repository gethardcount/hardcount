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
// instant a request runs on before its handler, from its route's entry in
// routeTable. A real-time route gets one read of the server's clock. A
// record's route gets, in test mode, the time of the test clock its record is
// on, and one read when the record is on none, is not found, or has an id
// Postgres cannot read; in live mode, where no record is on a clock, one read
// and no lookup. A decidedIn route gets no instant (in test mode, real time
// with none): its named function decides after it reads its record. So does a
// request whose route is not in routeTable, which the router answers 404 or
// 405. A request with no mode yet counts as test mode.
// A clock that cannot be read is answered 503, which the client retries and
// Idempotency does not keep, and the handler does not run.
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

// Middleware is the door (docs/architecture.md#the-path-of-a-request says
// why it ends every group).
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
