package creditnote

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// invoiceAnswer answers every invoice with sim, or fails with err, and
// counts the reads.
type invoiceAnswer struct {
	sim   clock.Sim
	err   error
	reads int
}

func (a *invoiceAnswer) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	a.reads++
	return a.sim, a.err
}
func (a *invoiceAnswer) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (a *invoiceAnswer) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// A credit note's time is decided from the invoice its body names: the
// invoice's clock; a not-found is real time; a clock that cannot be read
// refuses the request; live mode, and a body naming no invoice, read nothing.
func TestDecideForInvoice(t *testing.T) {
	frozen := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name      string
		ctx       context.Context
		invoiceID string
		answer    invoiceAnswer
		wantReads int
		wantClock string
		wantReal  bool
		wantErr   bool
	}{
		{"the invoice's clock", testMode, "inv_1", invoiceAnswer{sim: clock.Sim{At: frozen, TestClockID: "clk_inv"}}, 1, "clk_inv", false, false},
		{"a not-found is real time", testMode, "inv_1", invoiceAnswer{err: fmt.Errorf("read test clock clk_gone: %w", errs.ErrNotFound)}, 1, "", true, false},
		{"an unreadable clock refuses", testMode, "inv_1", invoiceAnswer{err: errors.New("connection reset by peer")}, 1, "", false, true},
		{"live mode reads nothing", postgres.WithLivemode(context.Background(), true), "inv_1", invoiceAnswer{err: errors.New("must not be read")}, 0, "", false, false},
		{"no invoice id reads nothing", testMode, "", invoiceAnswer{err: errors.New("must not be read")}, 0, "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := NewService(nil, nil, nil)
			res := c.answer
			svc.SetResolver(&res)
			ctx, err := svc.DecideForInvoice(c.ctx, "t1", c.invoiceID)
			if c.wantErr != (err != nil) || (c.wantErr && (!errors.Is(err, errs.ErrUnavailable) || !errors.Is(err, clock.ErrUndecided))) {
				t.Fatalf("err %v; want error %v (unavailable over clock.ErrUndecided)", err, c.wantErr)
			}
			if res.reads != c.wantReads {
				t.Errorf("%d reads; want %d", res.reads, c.wantReads)
			}
			if sim, _ := clock.SimOf(ctx); sim.TestClockID != c.wantClock {
				t.Errorf("clock %q; want %q", sim.TestClockID, c.wantClock)
			}
			if d, ok := clock.Decision(ctx); c.wantReal && (!ok || !d.At.IsZero() || d.TestClockID != "") {
				t.Errorf("decision %+v (%v); want real time", d, ok)
			}
		})
	}
}

// POST /v1/credit-notes decides from the body's invoice before anything
// runs, in both shapes: a create (here with auto_issue) and a commit relief.
// A clock that cannot be read answers 503 before the service is reached.
func TestCreateRoute_DecidesBeforeEitherShape(t *testing.T) {
	for name, body := range map[string]string{
		"create":        `{"invoice_id":"inv_1","reason":"Billing error","auto_issue":true,"lines":[{"description":"x","quantity":1,"unit_amount_cents":100}]}`,
		"commit relief": `{"invoice_id":"inv_1","reason":"Retire","commit_relief":{"retire_all":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			svc := NewService(nil, nil, nil) // nothing past the decision is wired
			svc.SetResolver(&invoiceAnswer{err: errors.New("connection reset by peer")})
			req := httptest.NewRequest("POST", "/", strings.NewReader(body))
			req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), "t1"))
			rec := httptest.NewRecorder()
			NewHandler(svc, HandlerDeps{}).create(rec, req)
			if rec.Code != 503 || !strings.Contains(rec.Body.String(), "test_clock_unavailable") {
				t.Errorf("%d %s; want 503 test_clock_unavailable", rec.Code, rec.Body.String())
			}
		})
	}
}
