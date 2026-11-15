package hostedinvoice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

var pageFrozen = time.Date(2027, 3, 9, 15, 0, 0, 0, time.UTC)

// pageResolver puts every invoice on clock clk_p at pageFrozen (or, with
// noClock, on no clock: the machine's time, as the engine answers), or fails
// with err, and counts the reads.
type pageResolver struct {
	reads   int
	err     error
	noClock bool
}

func (p *pageResolver) answer() (clock.Sim, error) {
	p.reads++
	switch {
	case p.err != nil:
		return clock.Sim{}, p.err
	case p.noClock:
		return clock.Sim{At: time.Now()}, nil
	}
	return clock.Sim{At: pageFrozen, TestClockID: "clk_p"}, nil
}
func (p *pageResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return p.answer()
}
func (p *pageResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return p.answer()
}
func (p *pageResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return p.answer()
}

// checkoutWith opens the hosted checkout of a finalized test invoice with the
// resolver res (live when live is set) and returns the answer and what the
// checkout saw.
func checkoutWith(t *testing.T, res *pageResolver, live bool) (*httptest.ResponseRecorder, *fakeCheckout) {
	t.Helper()
	fi, fc, fs := newFakeInvoices(), newFakeCustomers(), newFakeSettings()
	fi.live = live
	seedFinalized(t, fi, fc, fs, "hc_pinv_time")
	stripe := &fakeCheckout{}
	h := newTestHandler(fi, fc, fs, stripe)
	h.SetResolver(res)
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hc_pinv_time/checkout", nil))
	return w, stripe
}

// The page decides its time from the invoice its token names: a clock
// invoice's checkout is opened at its clock's time, and one on no clock at
// real time. A not-found under the decision (the invoice, or its clock in a
// race with the clock's deletion) is real time too.
func TestHostedPage_DecidesTheInvoicesTime(t *testing.T) {
	for _, c := range []struct {
		name string
		res  *pageResolver
		want clock.Sim
	}{
		{"a clock invoice: its clock's time", &pageResolver{}, clock.Sim{At: pageFrozen, TestClockID: "clk_p"}},
		{"an invoice on no clock: real time", &pageResolver{noClock: true}, clock.Sim{}},
		{"a not-found under the decision: real time", &pageResolver{err: fmt.Errorf("read test clock clk_p: %w", errs.ErrNotFound)}, clock.Sim{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, stripe := checkoutWith(t, c.res, false)
			if w.Code != http.StatusOK || !stripe.decided || stripe.decision != c.want {
				t.Errorf("code %d, decided %v, %+v; want 200 and %+v", w.Code, stripe.decided, stripe.decision, c.want)
			}
		})
	}
}

// A clock that cannot be read is answered 503 with nothing done: no checkout
// is opened.
func TestHostedPage_AnUnreadableClockOpensNothing(t *testing.T) {
	w, stripe := checkoutWith(t, &pageResolver{err: errors.New("connection reset by peer")}, false)
	if w.Code != http.StatusServiceUnavailable || stripe.lastTenant != "" {
		t.Fatalf("code %d, checkout opened for %q; want 503 and no checkout", w.Code, stripe.lastTenant)
	}
	if strings.Contains(w.Body.String(), "connection reset") {
		t.Errorf("the customer sees the cause: %s", w.Body.String())
	}
}

// In live mode no record is on a test clock: the page reads no clock.
func TestHostedPage_ALiveInvoiceReadsNoClock(t *testing.T) {
	res := &pageResolver{err: errors.New("connection reset by peer")}
	w, _ := checkoutWith(t, res, true)
	if w.Code != http.StatusOK || res.reads != 0 {
		t.Errorf("code %d, clock reads %d; want 200 and none", w.Code, res.reads)
	}
}
