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
// of customer cus_1, all on clock tc_1 frozen at frozen, and whose invoice
// inv_free (on sub_free) and one-off invoice inv_oneoff are of customer
// cus_free, on no clock; one-off invoice inv_oneoff_1 is of cus_1. clockErr, when set, fails the clock's read. The
// engine's clock steps and counts the reads it answers itself.
func resolverHarness(clockErr error) (*Engine, *countingInvoices, *countingSubs, *countingCustomers, *countingClocks, *steppingClock) {
	wall := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	frozen := time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC)
	invoices := &countingInvoices{mockInvoices: &mockInvoices{invoices: []domain.Invoice{
		{ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1"},
		{ID: "inv_dangling", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_gone"},
		{ID: "inv_free", TenantID: "t1", CustomerID: "cus_free", SubscriptionID: "sub_free"},
		{ID: "inv_oneoff", TenantID: "t1", CustomerID: "cus_free"},
		{ID: "inv_oneoff_1", TenantID: "t1", CustomerID: "cus_1"},
	}}}
	subs := &countingSubs{mockSubs: &mockSubs{subs: map[string]domain.Subscription{
		"sub_1":    {ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", TestClockID: "tc_1"},
		"sub_free": {ID: "sub_free", TenantID: "t1", CustomerID: "cus_free"},
	}}}
	customers := &countingCustomers{stubCustomerReader: stubCustomerReader{customers: map[string]domain.Customer{
		"cus_1":    {ID: "cus_1", TenantID: "t1", TestClockID: "tc_1"},
		"cus_free": {ID: "cus_free", TenantID: "t1"},
	}}}
	clocks := &countingClocks{stubClockReader: stubClockReader{clk: domain.TestClock{ID: "tc_1", FrozenTime: frozen}, err: clockErr}}
	server := &steppingClock{at: wall}
	e := NewEngine(subs, nil, nil, invoices, nil, nil, nil, nil, server)
	e.SetCustomerReader(customers)
	e.SetTestClockReader(clocks)
	return e, invoices, subs, customers, clocks, server
}

// In live mode no record is on a clock (a CHECK keeps clocks in test mode),
// so the three resolvers answer no clock and read nothing: a live webhook
// settle, which resolves its invoice, reads no invoice, subscription,
// customer or clock for its time, and not the engine's clock.
func TestResolvers_LiveModeReadsNothing(t *testing.T) {
	e, invoices, subs, customers, clocks, server := resolverHarness(nil)
	live := postgres.WithLivemode(context.Background(), true)
	for name, resolve := range map[string]func() (clock.Sim, error){
		"invoice":      func() (clock.Sim, error) { return e.SimForInvoice(live, "t1", "inv_1") },
		"subscription": func() (clock.Sim, error) { return e.SimForSubscription(live, "t1", "sub_1") },
		"customer":     func() (clock.Sim, error) { return e.SimForCustomer(live, "t1", "cus_1") },
	} {
		if got, err := resolve(); err != nil || got != (clock.Sim{}) {
			t.Errorf("%s in live mode: %+v, %v; want no clock (a zero Sim) and no error", name, got, err)
		}
	}
	if n := invoices.reads + subs.reads + customers.reads + clocks.reads + server.reads; n != 0 {
		t.Errorf("live mode read %d times (invoices %d, subscriptions %d, customers %d, clocks %d, the engine's clock %d), want none",
			n, invoices.reads, subs.reads, customers.reads, clocks.reads, server.reads)
	}
}

// In test mode a record whose clock cannot be read, or an invoice whose
// subscription is gone, is an error, returned with no clock (a zero Sim) and
// no read of the engine's clock: a caller that goes on anyway reads its own.
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
			e, _, _, _, _, server := resolverHarness(c.clockErr)
			got, err := c.resolve(e)
			if err == nil || got != (clock.Sim{}) || server.reads != 0 {
				t.Errorf("%+v, %v, %d reads of the engine's clock; want an error, no clock (a zero Sim) and no read", got, err, server.reads)
			}
		})
	}
}

// In test mode each resolver answers its record's clock: a record on a clock
// answers the clock's frozen time and id; a record on no clock answers no
// clock, a zero Sim, without reading the engine's clock.
func TestResolvers_AnswerTheRecordsClock(t *testing.T) {
	test := postgres.WithLivemode(context.Background(), false)
	onClock := clock.Sim{At: time.Date(2024, 2, 1, 12, 0, 0, 0, time.UTC), TestClockID: "tc_1"}
	e, _, _, _, _, server := resolverHarness(nil)
	for name, c := range map[string]struct {
		resolve func() (clock.Sim, error)
		want    clock.Sim
	}{
		"an invoice on a clock":         {func() (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_1") }, onClock},
		"a subscription on a clock":     {func() (clock.Sim, error) { return e.SimForSubscription(test, "t1", "sub_1") }, onClock},
		"a customer on a clock":         {func() (clock.Sim, error) { return e.SimForCustomer(test, "t1", "cus_1") }, onClock},
		"a one-off invoice on a clock":  {func() (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_oneoff_1") }, onClock},
		"an invoice on no clock":        {func() (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_free") }, clock.Sim{}},
		"a one-off invoice on no clock": {func() (clock.Sim, error) { return e.SimForInvoice(test, "t1", "inv_oneoff") }, clock.Sim{}},
		"a subscription on no clock":    {func() (clock.Sim, error) { return e.SimForSubscription(test, "t1", "sub_free") }, clock.Sim{}},
		"a customer on no clock":        {func() (clock.Sim, error) { return e.SimForCustomer(test, "t1", "cus_free") }, clock.Sim{}},
		// Narrow unit tests build the engine with no customer reader.
		"a customer, by an engine that reads no customers": {func() (clock.Sim, error) {
			return NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, server).SimForCustomer(test, "t1", "cus_1")
		}, clock.Sim{}},
	} {
		if got, err := c.resolve(); err != nil || got != c.want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, c.want)
		}
	}
	if server.reads != 0 {
		t.Errorf("the engine's clock was read %d times; want none", server.reads)
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
