package hostedinvoice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// pageServer is the server's clock in these tests: a fixed time, counting
// the reads it answers itself (a bound instant is not a read).
type pageServer struct{ reads int }

var pageServerAt = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

func (c *pageServer) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	return pageServerAt
}

// checkoutWith opens the hosted checkout of a finalized test invoice (live
// when live is set) and returns the answer, what the checkout saw, and the
// server's clock.
func checkoutWith(t *testing.T, live bool) (*httptest.ResponseRecorder, *fakeCheckout, *pageServer) {
	t.Helper()
	fi, fc, fs := newFakeInvoices(), newFakeCustomers(), newFakeSettings()
	fi.live = live
	seedFinalized(t, fi, fc, fs, "hc_pinv_time")
	stripe := &fakeCheckout{}
	h := newTestHandler(fi, fc, fs, stripe)
	srv := &pageServer{}
	h.SetClock(srv)
	w := httptest.NewRecorder()
	mountRouter(h).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hc_pinv_time/checkout", nil))
	return w, stripe, srv
}

// A public page binds its own instant, one read of the server's clock in
// either mode, so the checkout and its audit row read one instant.
func TestHostedPage_BindsOneInstant(t *testing.T) {
	for _, live := range []bool{false, true} {
		w, stripe, srv := checkoutWith(t, live)
		if w.Code != http.StatusOK || !stripe.bound || !stripe.at.Equal(pageServerAt) || srv.reads != 1 {
			t.Errorf("live %v: code %d, bound %v at %v after %d server reads; want 200 at %v after one",
				live, w.Code, stripe.bound, stripe.at, srv.reads, pageServerAt)
		}
	}
}
