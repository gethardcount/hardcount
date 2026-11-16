package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// invoiceAnswers answers each invoice from its own entry, fails for an
// invoice with none (an unreadable clock), and counts the reads.
type invoiceAnswers struct {
	sims  map[string]clock.Sim
	errs  map[string]error
	reads int
}

func (a *invoiceAnswers) SimForInvoice(_ context.Context, _, invoiceID string) (clock.Sim, error) {
	a.reads++
	if sim, ok := a.sims[invoiceID]; ok {
		return sim, nil
	}
	if err, ok := a.errs[invoiceID]; ok {
		return clock.Sim{}, err
	}
	return clock.Sim{}, errors.New("connection reset by peer")
}
func (a *invoiceAnswers) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (a *invoiceAnswers) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// clockSeeingRetrier records the clock each retry ran on.
type clockSeeingRetrier struct {
	*stubTaxRetrier
	clocks map[string]string
}

func (r *clockSeeingRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string,
	then func(ctx context.Context, tx *sql.Tx, taxed domain.Invoice) error,
) (domain.Invoice, error) {
	sim, _ := clock.SimOf(ctx)
	r.clocks[invoiceID] = sim.TestClockID
	return r.stubTaxRetrier.RetryTaxForInvoice(ctx, tenantID, invoiceID, then)
}

func stuckDraft(id, customerID, code string) domain.Invoice {
	return domain.Invoice{
		ID: id, TenantID: "t1", CustomerID: customerID, Status: domain.InvoiceDraft,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: code},
		BillingReason: "subscription_cycle", Currency: "USD", CreatedAt: time.Now().UTC(),
	}
}

// The connect fan-out decides each invoice's time before its whole retry:
// invoices on two clocks each run, finalize and settle on their own clock; one on
// none, or whose clock is not found, at real time; one whose clock cannot be
// read is left as it was, with the others retried.
func TestRetryProviderConfigErrors_DecidesEachInvoice(t *testing.T) {
	frozenA := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	frozenE := time.Date(2027, 8, 20, 6, 0, 0, 0, time.UTC)
	store := newMemStore()
	for _, inv := range []domain.Invoice{
		stuckDraft("inv_a", "cus_a", "provider_not_configured"),
		stuckDraft("inv_b", "cus_b", "provider_not_configured"),
		stuckDraft("inv_c", "cus_c", "provider_auth"),
		stuckDraft("inv_d", "cus_d", "provider_not_configured"),
		stuckDraft("inv_e", "cus_e", "provider_not_configured"),
	} {
		store.invoices[inv.ID] = inv
	}
	retrier := &clockSeeingRetrier{stubTaxRetrier: &stubTaxRetrier{store: store}, clocks: map[string]string{}}
	svc := NewService(store, nil, newMemNumberer())
	svc.SetTaxRetrier(retrier)
	svc.SetResolver(&invoiceAnswers{sims: map[string]clock.Sim{
		"inv_a": {At: frozenA, TestClockID: "clk_a"},
		"inv_b": {At: time.Now().UTC()}, // on no clock
		"inv_e": {At: frozenE, TestClockID: "clk_e"},
	}, errs: map[string]error{
		"inv_d": fmt.Errorf("read test clock clk_gone: %w", errs.ErrNotFound),
	}})

	processed, retryErrs := svc.RetryProviderConfigErrors(postgres.WithLivemode(context.Background(), false), "t1", false)
	if processed != 4 || len(retryErrs) != 1 || !strings.Contains(retryErrs[0].Error(), "inv_c") || !strings.Contains(retryErrs[0].Error(), "could not be read") {
		t.Fatalf("processed %d, errors %v; want 4 retried and inv_c's clock refused", processed, retryErrs)
	}
	if got := retrier.clocks; len(got) != 4 || got["inv_a"] != "clk_a" || got["inv_e"] != "clk_e" || got["inv_b"] != "" || got["inv_d"] != "" {
		t.Errorf("clocks the retries ran on: %v; want inv_a on clk_a, inv_e on clk_e, inv_b and inv_d on none, inv_c not retried", got)
	}
	// A retry that resolves the tax finalizes the draft, and these $0 drafts
	// are then settled by the finalize's steps: paid_at is the instant those
	// ran at, each invoice's own clock.
	for id, at := range map[string]time.Time{"inv_a": frozenA, "inv_e": frozenE} {
		if inv := store.invoices[id]; inv.Status != domain.InvoicePaid || inv.PaidAt == nil || !inv.PaidAt.Equal(at) {
			t.Errorf("%s: status %s, paid_at %v; want finalized and settled at its clock's %v", id, inv.Status, inv.PaidAt, at)
		}
	}
	if store.invoices["inv_c"].TaxStatus != domain.InvoiceTaxPending {
		t.Errorf("inv_c tax status %q; want it left pending", store.invoices["inv_c"].TaxStatus)
	}
}

// The connect fan-out is detached from the connect request and outlives it,
// so it starts on no instant (the connect route is decidedIn: the door binds
// none) and each invoice's retry reads the server's clock by itself: two
// invoices on no clock are settled at two instants, in both modes, and one on
// a clock at its clock's time.
func TestRetryProviderConfigErrors_EachInvoiceReadsItsOwnTime(t *testing.T) {
	frozen := time.Date(2027, 8, 20, 6, 0, 0, 0, time.UTC)
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, c := range []struct {
		name string
		ctx  context.Context
		live bool
	}{
		{"test mode, as the door leaves a decidedIn route", clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), false},
		{"live", postgres.WithLivemode(context.Background(), true), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newMemStore()
			for _, inv := range []domain.Invoice{
				stuckDraft("inv_1", "cus_1", "provider_not_configured"),
				stuckDraft("inv_2", "cus_2", "provider_not_configured"),
				stuckDraft("inv_c", "cus_c", "provider_not_configured"),
			} {
				store.invoices[inv.ID] = inv
			}
			server := &steppingClock{at: past}
			svc := NewService(store, server, newMemNumberer())
			svc.SetTaxRetrier(&stubTaxRetrier{store: store})
			svc.SetResolver(&invoiceAnswers{sims: map[string]clock.Sim{
				"inv_1": {At: time.Now().UTC()}, // on no clock
				"inv_2": {At: time.Now().UTC()},
				"inv_c": {At: frozen, TestClockID: "clk_c"},
			}})
			if processed, retryErrs := svc.RetryProviderConfigErrors(c.ctx, "t1", false); processed != 3 || len(retryErrs) != 0 {
				t.Fatalf("processed %d, errors %v; want all three retried", processed, retryErrs)
			}
			a, b := store.invoices["inv_1"].PaidAt, store.invoices["inv_2"].PaidAt
			if a == nil || b == nil || a.Equal(*b) || a.Before(past) || b.Before(past) {
				t.Errorf("paid at %v and %v; want two instants, each read from the server's clock", a, b)
			}
			// Live mode decides nothing: no live record is on a clock, so
			// the clock invoice is retried at real time too.
			if pc := store.invoices["inv_c"].PaidAt; pc == nil || (!c.live && !pc.Equal(frozen)) {
				t.Errorf("the clock invoice paid at %v; want its clock's %v in test mode", pc, frozen)
			}
		})
	}
}

// The billing-profile fan-out is one customer's invoices, already decided by
// its route: it reads no invoice clock of its own, so a lookup that would
// fail does not stop its retries, and they run on the route's clock.
func TestRetryCustomerDataErrors_KeepsItsRoutesDecision(t *testing.T) {
	store := newMemStore()
	for _, inv := range []domain.Invoice{
		stuckDraft("inv_1", "cus_1", "customer_data_invalid"),
		stuckDraft("inv_2", "cus_1", "customer_data_invalid"),
	} {
		store.invoices[inv.ID] = inv
	}
	retrier := &clockSeeingRetrier{stubTaxRetrier: &stubTaxRetrier{store: store}, clocks: map[string]string{}}
	svc := NewService(store, nil, newMemNumberer())
	svc.SetTaxRetrier(retrier)
	svc.SetResolver(&invoiceAnswers{}) // every invoice lookup fails
	ctx := clock.WithSim(postgres.WithLivemode(context.Background(), false),
		clock.Sim{At: time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC), TestClockID: "clk_route"})

	processed, retryErrs := svc.RetryCustomerDataErrors(ctx, "t1", "cus_1")
	if processed != 2 || len(retryErrs) != 0 {
		t.Fatalf("processed %d, errors %v; want both retried", processed, retryErrs)
	}
	if got := retrier.clocks; got["inv_1"] != "clk_route" || got["inv_2"] != "clk_route" {
		t.Errorf("clocks the retries ran on: %v; want the route's clk_route for both", got)
	}
}

// pinAnswers answers a subscription or a customer with sim, or fails with
// err, and records the pins read.
type pinAnswers struct {
	sim  clock.Sim
	err  error
	read []string
}

func (a *pinAnswers) SimForSubscription(_ context.Context, _, id string) (clock.Sim, error) {
	a.read = append(a.read, "sub:"+id)
	return a.sim, a.err
}
func (a *pinAnswers) SimForCustomer(_ context.Context, _, id string) (clock.Sim, error) {
	a.read = append(a.read, "cus:"+id)
	return a.sim, a.err
}
func (a *pinAnswers) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// A new invoice's time is decided from its subscription when the body names
// one, else its customer; a not-found is real time; a clock that cannot be
// read refuses the create; live mode reads nothing.
func TestDecideCreate(t *testing.T) {
	frozen := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name      string
		ctx       context.Context
		input     CreateInput
		answer    pinAnswers
		wantRead  []string
		wantClock string // "" with wantReal: real time decided
		wantReal  bool
		wantErr   bool
	}{
		{"the subscription's clock", testMode, CreateInput{CustomerID: "cus_1", SubscriptionID: "sub_1"},
			pinAnswers{sim: clock.Sim{At: frozen, TestClockID: "clk_sub"}}, []string{"sub:sub_1"}, "clk_sub", false, false},
		{"the customer's clock", testMode, CreateInput{CustomerID: "cus_1"},
			pinAnswers{sim: clock.Sim{At: frozen, TestClockID: "clk_cus"}}, []string{"cus:cus_1"}, "clk_cus", false, false},
		{"a not-found is real time", testMode, CreateInput{CustomerID: "cus_1"},
			pinAnswers{err: fmt.Errorf("read test clock clk_gone: %w", errs.ErrNotFound)}, []string{"cus:cus_1"}, "", true, false},
		{"an unreadable clock refuses", testMode, CreateInput{CustomerID: "cus_1"},
			pinAnswers{err: errors.New("connection reset by peer")}, []string{"cus:cus_1"}, "", false, true},
		{"live mode reads nothing", postgres.WithLivemode(context.Background(), true), CreateInput{CustomerID: "cus_1"},
			pinAnswers{err: errors.New("must not be read")}, nil, "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := NewService(newMemStore(), nil, newMemNumberer())
			res := c.answer
			svc.SetResolver(&res)
			ctx, err := svc.decideCreate(c.ctx, "t1", c.input)
			if c.wantErr != (err != nil) || (c.wantErr && (!errors.Is(err, errs.ErrUnavailable) || !errors.Is(err, clock.ErrUndecided))) {
				t.Fatalf("err %v; want error %v (unavailable over clock.ErrUndecided)", err, c.wantErr)
			}
			if strings.Join(res.read, ",") != strings.Join(c.wantRead, ",") {
				t.Errorf("pins read %v; want %v", res.read, c.wantRead)
			}
			sim, _ := clock.SimOf(ctx)
			if sim.TestClockID != c.wantClock {
				t.Errorf("clock %q; want %q", sim.TestClockID, c.wantClock)
			}
			if d, ok := clock.Decision(ctx); c.wantReal && (!ok || !d.At.IsZero() || d.TestClockID != "") {
				t.Errorf("decision %+v (%v); want real time", d, ok)
			}
		})
	}
}

// answersOnce answers each invoice's first lookup with a clock and fails
// every later one.
type answersOnce struct{ read map[string]int }

func (a *answersOnce) SimForInvoice(_ context.Context, _, invoiceID string) (clock.Sim, error) {
	a.read[invoiceID]++
	if a.read[invoiceID] == 1 {
		return clock.Sim{At: time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC), TestClockID: "clk_" + invoiceID}, nil
	}
	return clock.Sim{}, errors.New("connection reset by peer")
}
func (a *answersOnce) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (a *answersOnce) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// The connect fan-out's decision is taken once, before the invoice's whole
// retry, and stands for all of it: a lookup inside the retry that fails does
// not take the invoice off its clock.
func TestRetryProviderConfigErrors_TheDecisionStandsForTheWholeRetry(t *testing.T) {
	store := newMemStore()
	store.invoices["inv_a"] = stuckDraft("inv_a", "cus_a", "provider_not_configured")
	retrier := &clockSeeingRetrier{stubTaxRetrier: &stubTaxRetrier{store: store}, clocks: map[string]string{}}
	svc := NewService(store, nil, newMemNumberer())
	svc.SetTaxRetrier(retrier)
	svc.SetResolver(&answersOnce{read: map[string]int{}})

	if processed, retryErrs := svc.RetryProviderConfigErrors(postgres.WithLivemode(context.Background(), false), "t1", false); processed != 1 || len(retryErrs) != 0 {
		t.Fatalf("processed %d, errors %v; want inv_a retried", processed, retryErrs)
	}
	if got := retrier.clocks["inv_a"]; got != "clk_inv_a" {
		t.Errorf("the retry ran on clock %q; want the decided clk_inv_a", got)
	}
}
