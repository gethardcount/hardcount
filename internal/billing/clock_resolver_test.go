package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// countingInvoices and countingSubs count the reads a resolver makes.
type countingInvoices struct {
	*mockInvoices
	reads int
}

func (c *countingInvoices) GetInvoice(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	c.reads++
	return c.mockInvoices.GetInvoice(ctx, tenantID, id)
}

type countingSubs struct {
	*mockSubs
	reads int
}

func (c *countingSubs) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	c.reads++
	return c.mockSubs.Get(ctx, tenantID, id)
}

type countingCustomers struct {
	stubCustomerReader
	reads int
}

func (c *countingCustomers) Get(ctx context.Context, tenantID, id string) (domain.Customer, error) {
	c.reads++
	return c.stubCustomerReader.Get(ctx, tenantID, id)
}

type countingClocks struct {
	stubClockReader
	reads int
}

func (c *countingClocks) Get(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	c.reads++
	return c.stubClockReader.Get(ctx, tenantID, id)
}

// resolverHarness is an engine whose invoice inv_1 is on subscription sub_1
// of customer cus_1, all on clock tc_1 frozen at frozen; clockErr, when set,
// fails the clock's read.
func resolverHarness(clockErr error) (*Engine, *countingInvoices, *countingSubs, *countingCustomers, *countingClocks, time.Time) {
	wall := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	invoices := &countingInvoices{mockInvoices: &mockInvoices{invoices: []domain.Invoice{
		{ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1"},
		{ID: "inv_dangling", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_gone"},
	}}}
	subs := &countingSubs{mockSubs: &mockSubs{subs: map[string]domain.Subscription{
		"sub_1": {ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", TestClockID: "tc_1"},
	}}}
	customers := &countingCustomers{stubCustomerReader: stubCustomerReader{customers: map[string]domain.Customer{
		"cus_1": {ID: "cus_1", TenantID: "t1", TestClockID: "tc_1"},
	}}}
	clocks := &countingClocks{stubClockReader: stubClockReader{clk: domain.TestClock{ID: "tc_1", FrozenTime: frozen}, err: clockErr}}
	e := NewEngine(subs, nil, nil, invoices, nil, nil, nil, nil, clock.NewFake(wall))
	e.SetCustomerReader(customers)
	e.SetTestClockReader(clocks)
	return e, invoices, subs, customers, clocks, wall
}

// In live mode no record is on a clock (a CHECK keeps clocks in test mode),
// so the three resolvers answer real time and read nothing: a live webhook
// settle, which resolves its invoice, reads no invoice, subscription,
// customer or clock for its time.
func TestResolvers_LiveModeReadsNothing(t *testing.T) {
	e, invoices, subs, customers, clocks, wall := resolverHarness(nil)
	live := postgres.WithLivemode(context.Background(), true)
	for name, resolve := range map[string]func() (clock.Sim, error){
		"invoice":      func() (clock.Sim, error) { return e.SimForInvoice(live, "t1", "inv_1") },
		"subscription": func() (clock.Sim, error) { return e.SimForSubscription(live, "t1", "sub_1") },
		"customer":     func() (clock.Sim, error) { return e.SimForCustomer(live, "t1", "cus_1") },
	} {
		got, err := resolve()
		if err != nil || got.TestClockID != "" || !got.At.Equal(wall) {
			t.Errorf("%s in live mode: %+v, %v; want real time (%v) and no error", name, got, err, wall)
		}
	}
	if n := invoices.reads + subs.reads + customers.reads + clocks.reads; n != 0 {
		t.Errorf("live mode read %d records (invoices %d, subscriptions %d, customers %d, clocks %d), want none",
			n, invoices.reads, subs.reads, customers.reads, clocks.reads)
	}
}

// In test mode a record whose clock cannot be read, or an invoice whose
// subscription is gone, is an error, where main answered real time with no
// error. At is still the machine's time, never zero, and claims no clock.
func TestResolvers_UnreadableRecordIsAnError(t *testing.T) {
	test := postgres.WithLivemode(context.Background(), false)
	clockDown := errors.New("clock read failed")
	for _, c := range []struct {
		name     string
		clockErr error
		resolve  func(*Engine) (clock.Sim, error)
	}{
		// One row per error return in the resolver block.
		{"an invoice that cannot be read", nil, func(e *Engine) (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_missing") }},
		{"a subscription that cannot be read", nil, func(e *Engine) (clock.Sim, error) { return e.SimForSubscription(test, "t1", "sub_missing") }},
		{"a customer that cannot be read", nil, func(e *Engine) (clock.Sim, error) { return e.SimForCustomer(test, "t1", "cus_missing") }},
		{"an invoice whose clock cannot be read", clockDown, func(e *Engine) (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_1") }},
		{"an invoice whose subscription is gone", nil, func(e *Engine) (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_dangling") }},
		{"a subscription whose clock cannot be read", clockDown, func(e *Engine) (clock.Sim, error) { return e.SimForSubscription(test, "t1", "sub_1") }},
		{"a customer whose clock cannot be read", clockDown, func(e *Engine) (clock.Sim, error) { return e.SimForCustomer(test, "t1", "cus_1") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, _, _, _, wall := resolverHarness(c.clockErr)
			got, err := c.resolve(e)
			if err == nil {
				t.Errorf("got %+v and no error, want an error", got)
			}
			if got.TestClockID != "" || !got.At.Equal(wall) {
				t.Errorf("answer alongside the error: %+v, want the machine's time (%v) and no clock", got, wall)
			}
		})
	}
}

// In test mode a record on a clock answers the clock's frozen time and id,
// through each resolver on its own.
func TestResolvers_ARecordOnAClockAnswersTheClock(t *testing.T) {
	test := postgres.WithLivemode(context.Background(), false)
	want := clock.Sim{At: time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC), TestClockID: "tc_1"}
	e, _, _, _, _, _ := resolverHarness(nil)
	for name, resolve := range map[string]func() (clock.Sim, error){
		"invoice":      func() (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_1") },
		"subscription": func() (clock.Sim, error) { return e.SimForSubscription(test, "t1", "sub_1") },
		"customer":     func() (clock.Sim, error) { return e.SimForCustomer(test, "t1", "cus_1") },
	} {
		if got, err := resolve(); err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, want)
		}
	}
}

// A ctx that carries no mode counts as test mode: the resolvers read, and a
// record on a clock answers the clock.
func TestResolvers_NoModeReads(t *testing.T) {
	e, invoices, subs, _, clocks, _ := resolverHarness(nil)
	got, err := e.SimForInvoice(context.Background(), "t1", "inv_1")
	if err != nil || got.TestClockID != "tc_1" {
		t.Errorf("no mode: %+v, %v; want clock tc_1", got, err)
	}
	if invoices.reads != 1 || subs.reads != 1 || clocks.reads != 1 {
		t.Errorf("reads: invoices %d, subscriptions %d, clocks %d; want 1 each", invoices.reads, subs.reads, clocks.reads)
	}
}

// An invoice that owes nothing, settled by the real-time run, is paid at the
// machine's time: real time in live mode is never a zero instant.
func TestCollect_LiveZeroDueInvoiceIsPaidNow(t *testing.T) {
	inv, _, engine := retryHarness(t, nil)
	engine.SetDunningResolver(&recordingDunningResolver{})
	inv.invoices[0].AmountDueCents = 0
	charged, errs := engine.RetryPendingCharges(postgres.WithLivemode(context.Background(), true), 50, nil)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	got := inv.invoices[0]
	want := billingTestClock().Now(context.Background())
	if got.Status != domain.InvoicePaid || got.PaidAt == nil || !got.PaidAt.Equal(want) {
		t.Fatalf("charged %d; status %s, paid_at %v; want paid at %v", charged, got.Status, got.PaidAt, want)
	}
}
