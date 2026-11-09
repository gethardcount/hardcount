package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// baseReadFails is the invoice store the API wires for item changes
// (api.prorationInvoiceCreatorAdapter), except that FindBaseInvoiceForPeriod
// returns err when err is set. reads counts the base-invoice reads, so a case
// can show the change reached the read.
type baseReadFails struct {
	*invoice.PostgresStore
	numberer *tenant.SettingsStore
	err      error
	reads    int
}

func (s *baseReadFails) FindBaseInvoiceForPeriod(ctx context.Context, tenantID, subscriptionID string, periodStart time.Time) (domain.Invoice, error) {
	s.reads++
	if s.err != nil {
		return domain.Invoice{}, s.err
	}
	return s.PostgresStore.FindBaseInvoiceForPeriod(ctx, tenantID, subscriptionID, periodStart)
}

func (s *baseReadFails) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return s.CreateWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

func (s *baseReadFails) NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	return s.numberer.NextInvoiceNumberTx(ctx, tx, tenantID)
}

// baseReadRig is an active in-advance subscription with one Basic seat over
// June 2026 and no invoice yet. Every change runs on June 16.
type baseReadRig struct {
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	store    *PostgresStore
	h        *Handler
	invoices *baseReadFails
	plans    map[string]domain.Plan // by code
	subID    string
	itemID   string
}

func newBaseReadRig(t *testing.T, db *postgres.DB, name string, readErr error) *baseReadRig {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_base_read", DisplayName: "Base Read",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plans := map[string]domain.Plan{}
	for _, p := range []domain.Plan{
		{Code: "basic", Name: "Basic", BaseAmountCents: 3000},
		{Code: "pro", Name: "Pro", BaseAmountCents: 6000},
		{Code: "extra", Name: "Extra", BaseAmountCents: 1000},
	} {
		p.Currency = "USD"
		p.BillingInterval = domain.BillingMonthly
		p.BaseBillTiming = domain.BillInAdvance
		p.Status = domain.PlanActive
		plans[p.Code] = createPlanPricing(t, db, ctx, tenantID, p)
	}

	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-base-read", DisplayName: "Base Read", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &proPeriodStart,
		CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd:   &proPeriodEnd,
		NextBillingAt:             &proPeriodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: plans["basic"].ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	byID := make(map[string]domain.Plan, len(plans))
	for _, p := range plans {
		byID[p.ID] = p
	}
	reader := &plansMock{plans: byID}
	invoices := &baseReadFails{
		PostgresStore: invoice.NewPostgresStore(db),
		numberer:      tenant.NewSettingsStore(db),
		err:           readErr,
	}
	svc := svcWithStore(store)
	svc.SetPlanReader(reader)
	svc.SetPlanMeterReader(pricing.NewPostgresStore(db))
	h := NewHandler(svc)
	h.SetProrationDeps(reader, invoices)

	return &baseReadRig{
		ctx: ctx, db: db, tenantID: tenantID, store: store, h: h,
		invoices: invoices, plans: plans,
		subID: created.ID, itemID: created.Items[0].ID,
	}
}

func (r *baseReadRig) reqCtx() context.Context {
	return clock.WithEffectiveNow(auth.WithTenantID(r.ctx, r.tenantID), proNow)
}

func (r *baseReadRig) sub(t *testing.T) domain.Subscription {
	t.Helper()
	sub, err := r.store.Get(r.ctx, r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	return sub
}

// invoiceCount is the number of invoices the subscription has in Postgres.
func (r *baseReadRig) invoiceCount(t *testing.T) int {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, `SELECT count(*) FROM invoices WHERE subscription_id = $1`, r.subID).Scan(&n); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	return n
}

// TestItemChange_BaseInvoiceRead: a change that charges (an added item, a
// quantity increase, a plan upgrade) reads the current period's base invoice
// first. Only "not found" means there is no base invoice yet: the change
// commits and charges nothing, and cycle close bills the period as it then
// stands. Any other read error refuses the change with a retryable 503
// proration_unavailable, and its transaction rolls back: the item is as it
// was, and the subscription has no invoice.
//
// Mutation-verify: in handleItemProration, treat every read error as "not
// found" again. Each "the read fails" case then commits the change with no
// charge, and fails here.
func TestItemChange_BaseInvoiceRead(t *testing.T) {
	db := testutil.SetupTestDB(t)

	two := int64(2)
	changes := []struct {
		name    string
		send    func(r *baseReadRig, rr *httptest.ResponseRecorder)
		okCode  int
		applied func(r *baseReadRig, sub domain.Subscription) bool
	}{
		{
			name: "add an item",
			send: func(r *baseReadRig, rr *httptest.ResponseRecorder) {
				body, _ := json.Marshal(AddItemInput{PlanID: r.plans["extra"].ID, Quantity: 1})
				r.h.addItem(rr, addItemURL(r.reqCtx(), r.subID, body))
			},
			okCode: http.StatusCreated,
			applied: func(_ *baseReadRig, sub domain.Subscription) bool {
				return len(sub.Items) == 2
			},
		},
		{
			name: "raise the quantity",
			send: func(r *baseReadRig, rr *httptest.ResponseRecorder) {
				body, _ := json.Marshal(UpdateItemInput{Quantity: &two, Immediate: true})
				r.h.updateItem(rr, updateItemURL(r.reqCtx(), r.subID, r.itemID, body))
			},
			okCode: http.StatusOK,
			applied: func(_ *baseReadRig, sub domain.Subscription) bool {
				return len(sub.Items) == 1 && sub.Items[0].Quantity == 2
			},
		},
		{
			name: "upgrade the plan",
			send: func(r *baseReadRig, rr *httptest.ResponseRecorder) {
				body, _ := json.Marshal(UpdateItemInput{NewPlanID: r.plans["pro"].ID, Immediate: true})
				r.h.updateItem(rr, updateItemURL(r.reqCtx(), r.subID, r.itemID, body))
			},
			okCode: http.StatusOK,
			applied: func(r *baseReadRig, sub domain.Subscription) bool {
				return len(sub.Items) == 1 && sub.Items[0].PlanID == r.plans["pro"].ID
			},
		},
	}

	for _, c := range changes {
		t.Run(c.name+": the read fails", func(t *testing.T) {
			r := newBaseReadRig(t, db, "Base Read Fails", errors.New("read base invoice: connection reset by peer"))
			rr := httptest.NewRecorder()
			c.send(r, rr)

			if r.invoices.reads != 1 {
				t.Fatalf("base-invoice reads = %d, want 1: the change never reached the read", r.invoices.reads)
			}
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
			}
			if got := errorCode(rr); got != "proration_unavailable" {
				t.Errorf("error code = %q, want proration_unavailable", got)
			}
			if got := rr.Header().Get("Retry-After"); got != "1" {
				t.Errorf("Retry-After = %q, want 1", got)
			}
			if c.applied(r, r.sub(t)) {
				t.Errorf("the change committed after its base-invoice read failed: %+v", r.sub(t).Items)
			}
			if n := r.invoiceCount(t); n != 0 {
				t.Errorf("invoices = %d, want 0", n)
			}
		})

		t.Run(c.name+": no base invoice yet", func(t *testing.T) {
			r := newBaseReadRig(t, db, "Base Read Not Found", nil)
			rr := httptest.NewRecorder()
			c.send(r, rr)

			if r.invoices.reads != 1 {
				t.Fatalf("base-invoice reads = %d, want 1: the change never reached the read", r.invoices.reads)
			}
			if rr.Code != c.okCode {
				t.Fatalf("status = %d, want %d: %s", rr.Code, c.okCode, rr.Body.String())
			}
			if !c.applied(r, r.sub(t)) {
				t.Errorf("the change did not commit: %+v", r.sub(t).Items)
			}
			if n := r.invoiceCount(t); n != 0 {
				t.Errorf("invoices = %d, want 0: with no base invoice yet the change charges nothing", n)
			}
		})
	}
}
