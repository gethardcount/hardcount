package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Two changes to one subscription item that overlap in time (#1270).
//
// The first change is held inside its transaction at the step that waits on
// an outside service in production: the proration's tax call (a charge) or
// its credit grant (a credit). The second change is sent while the first is
// held, and the test waits until it is blocked on a row lock in Postgres.
// Then the first change finishes, and the test checks what the two changes
// billed together and the state they left.
//
// The rule under test: the second change is priced from the state the first
// change left, or it is refused. It is never priced from the state both
// changes started from, which charges or credits the same seats twice.
//
// The subscription store, its transactions and its locks are real Postgres.
// The invoice and credit stores are recorders: what they record is what the
// handler priced, and every case asserts the status codes, so a write that
// a rollback undid would show as a failed request.

// Every plan bills in advance per seat over June 2026 (30 days). Both
// changes land on June 16, with 15 of 30 days left, so a seat's change is
// prorated at exactly half its monthly price.
const (
	raceBasicCents  = 3000
	raceProCents    = 6000
	raceKeeperCents = 1000
)

// holdFirst blocks the first call that reaches it until release is closed.
// Later calls pass straight through.
type holdFirst struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newHoldFirst() *holdFirst {
	return &holdFirst{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *holdFirst) wait() {
	first := false
	g.once.Do(func() { first = true })
	if first {
		close(g.entered)
		<-g.release
	}
}

// raceTax charges no tax. Its first call holds.
type raceTax struct{ gate *holdFirst }

func (f *raceTax) ApplyTaxToLineItems(_ context.Context, _, _, _ string, subtotal, discount int64, _ []domain.InvoiceLineItem) (ProrationTaxResult, error) {
	f.gate.wait()
	return ProrationTaxResult{SubtotalCents: subtotal, DiscountCents: discount}, nil
}

func (f *raceTax) CommitTax(context.Context, string, string, string) error { return nil }

// raceInvoices records proration invoices. The period's base invoice is
// paid, so an increase is charged (an unpaid one refuses it, ADR-050).
type raceInvoices struct {
	source domain.Invoice
	mu     sync.Mutex
	made   []domain.Invoice
	number int
}

func (f *raceInvoices) record(tenantID string, inv domain.Invoice) domain.Invoice {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv.ID = fmt.Sprintf("vlx_inv_race_%d", len(f.made)+1)
	inv.TenantID = tenantID
	f.made = append(f.made, inv)
	return inv
}

func (f *raceInvoices) all() []domain.Invoice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Invoice(nil), f.made...)
}

func (f *raceInvoices) CreateInvoiceWithLineItems(_ context.Context, tenantID string, inv domain.Invoice, _ []domain.InvoiceLineItem) (domain.Invoice, error) {
	return f.record(tenantID, inv), nil
}

func (f *raceInvoices) CreateInvoiceWithLineItemsTx(_ context.Context, _ *sql.Tx, tenantID string, inv domain.Invoice, _ []domain.InvoiceLineItem) (domain.Invoice, error) {
	return f.record(tenantID, inv), nil
}

func (f *raceInvoices) GetByProrationSource(context.Context, string, string, string, domain.ItemChangeType, time.Time) (domain.Invoice, error) {
	return domain.Invoice{}, errs.ErrNotFound
}

func (f *raceInvoices) nextNumber() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.number++
	return fmt.Sprintf("RACE-%04d", f.number)
}

func (f *raceInvoices) NextInvoiceNumber(context.Context, string) (string, error) {
	return f.nextNumber(), nil
}

func (f *raceInvoices) NextInvoiceNumberTx(context.Context, *sql.Tx, string) (string, error) {
	return f.nextNumber(), nil
}

func (f *raceInvoices) FindBaseInvoiceForPeriod(context.Context, string, string, time.Time) (domain.Invoice, error) {
	return f.source, nil
}

func (f *raceInvoices) FindFundingInvoicesForPeriod(context.Context, string, string, time.Time, time.Time) ([]domain.Invoice, error) {
	return []domain.Invoice{f.source}, nil
}

func (f *raceInvoices) SetAutoChargePending(context.Context, string, string, bool) error {
	return nil
}

func (f *raceInvoices) SetAutoChargePendingTx(context.Context, *sql.Tx, string, string, bool) error {
	return nil
}

// raceCredits records proration credits. Its first grant holds.
type raceCredits struct {
	gate *holdFirst
	mu   sync.Mutex
	made []ProrationGrantInput
}

func (f *raceCredits) record(in ProrationGrantInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.made = append(f.made, in)
}

func (f *raceCredits) all() []ProrationGrantInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ProrationGrantInput(nil), f.made...)
}

func (f *raceCredits) GrantProration(_ context.Context, _ string, in ProrationGrantInput) error {
	f.record(in)
	return nil
}

func (f *raceCredits) GrantProrationTx(_ context.Context, _ *sql.Tx, _ string, in ProrationGrantInput) error {
	f.gate.wait()
	f.record(in)
	return nil
}

func (f *raceCredits) GetByProrationSource(context.Context, string, string, string, domain.ItemChangeType, time.Time) (domain.CreditLedgerEntry, error) {
	return domain.CreditLedgerEntry{}, errs.ErrNotFound
}

// raceReq is one item change: a PATCH body, or a DELETE when remove is set.
type raceReq struct {
	body   UpdateItemInput
	remove bool
}

func raceQty(n int64) raceReq {
	return raceReq{body: UpdateItemInput{Quantity: &n, Immediate: true}}
}

func racePlan(planID string) raceReq {
	return raceReq{body: UpdateItemInput{NewPlanID: planID, Immediate: true}}
}

var raceRemove = raceReq{remove: true}

type itemRaceRig struct {
	ctx      context.Context
	tenantID string
	store    *PostgresStore
	h        *Handler
	gate     *holdFirst
	invoices *raceInvoices
	credits  *raceCredits
	plans    map[string]domain.Plan // by code
	subID    string
	itemID   string
}

// newItemRaceRig seeds an active subscription on the basic plan at qty seats
// for June 2026, plus a one-seat keeper item when withKeeper is set (the last
// item of an active subscription cannot be removed).
func newItemRaceRig(t *testing.T, db *postgres.DB, name string, qty int64, withKeeper bool) *itemRaceRig {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_item_race", DisplayName: "Item Race",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	ps := pricing.NewPostgresStore(db)
	plans := map[string]domain.Plan{}
	for _, p := range []domain.Plan{
		{Code: "basic", Name: "Basic", BillingInterval: domain.BillingMonthly, BaseAmountCents: raceBasicCents},
		{Code: "pro", Name: "Pro", BillingInterval: domain.BillingMonthly, BaseAmountCents: raceProCents},
		{Code: "keeper", Name: "Keeper", BillingInterval: domain.BillingMonthly, BaseAmountCents: raceKeeperCents},
		{Code: "basic-yearly", Name: "Basic Yearly", BillingInterval: domain.BillingYearly, BaseAmountCents: raceBasicCents * 10},
	} {
		p.Currency = "USD"
		p.BaseBillTiming = domain.BillInAdvance
		p.Status = domain.PlanActive
		created, err := ps.CreatePlan(ctx, tenantID, p)
		if err != nil {
			t.Fatalf("create plan %s: %v", p.Code, err)
		}
		plans[p.Code] = created
	}

	items := []domain.SubscriptionItem{{PlanID: plans["basic"].ID, Quantity: qty}}
	if withKeeper {
		items = append(items, domain.SubscriptionItem{PlanID: plans["keeper"].ID, Quantity: 1})
	}
	store := NewPostgresStore(db)
	created, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-item-race", DisplayName: "Item Race", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &proPeriodStart,
		CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd:   &proPeriodEnd,
		NextBillingAt:             &proPeriodEnd,
		Items:                     items,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	var itemID string
	for _, it := range created.Items {
		if it.PlanID == plans["basic"].ID {
			itemID = it.ID
		}
	}
	if itemID == "" {
		t.Fatalf("created subscription has no basic item: %+v", created.Items)
	}

	byID := make(map[string]domain.Plan, len(plans))
	for _, p := range plans {
		byID[p.ID] = p
	}
	reader := &plansMock{plans: byID}
	gate := newHoldFirst()
	invoices := &raceInvoices{source: domain.Invoice{
		ID: "vlx_inv_race_base", PaymentStatus: domain.PaymentSucceeded, Status: domain.InvoicePaid,
		SubtotalCents: raceBasicCents * qty, TotalAmountCents: raceBasicCents * qty, CreatedAt: proPeriodStart,
	}}
	credits := &raceCredits{gate: gate}

	svc := svcWithStore(store)
	svc.SetPlanReader(reader)
	h := NewHandler(svc)
	h.SetDB(db)
	h.SetProrationDeps(reader, invoices, credits)
	h.SetProrationTaxApplier(&raceTax{gate: gate})

	return &itemRaceRig{
		ctx: ctx, tenantID: tenantID, store: store, h: h, gate: gate,
		invoices: invoices, credits: credits, plans: plans,
		subID: created.ID, itemID: itemID,
	}
}

func (r *itemRaceRig) send(at time.Time, q raceReq) *httptest.ResponseRecorder {
	ctx := clock.WithEffectiveNow(context.WithValue(r.ctx, auth.TestTenantIDKey(), r.tenantID), at)
	rr := httptest.NewRecorder()
	if q.remove {
		r.h.removeItem(rr, removeItemURL(ctx, r.subID, r.itemID))
		return rr
	}
	body, _ := json.Marshal(q.body)
	r.h.updateItem(rr, updateItemURL(ctx, r.subID, r.itemID, body))
	return rr
}

// race sends first and holds it inside its transaction, sends second, waits
// until second is blocked on a row lock, then lets first finish. first runs
// at proNow and second an hour later, both inside the same day.
func (r *itemRaceRig) race(t *testing.T, first, second raceReq) (firstRR, secondRR *httptest.ResponseRecorder) {
	t.Helper()
	released := false
	defer func() {
		if !released {
			close(r.gate.release) // a failed wait must not leave a transaction open
		}
	}()

	done1 := make(chan *httptest.ResponseRecorder, 1)
	go func() { done1 <- r.send(proNow, first) }()
	select {
	case <-r.gate.entered:
	case rr := <-done1:
		t.Fatalf("first change finished without reaching its tax call or credit grant: %d %s", rr.Code, rr.Body.String())
	case <-time.After(15 * time.Second):
		t.Fatal("first change never reached its tax call or credit grant")
	}

	done2 := make(chan *httptest.ResponseRecorder, 1)
	go func() { done2 <- r.send(proNow.Add(time.Hour), second) }()
	// Both statements a blocked second change can wait in name the table:
	// the item UPDATE, or the subscription row lock.
	testutil.WaitForLockWaiter(t, r.ctx, testutil.AdminPool(t), "subscription")

	released = true
	close(r.gate.release)
	return <-done1, <-done2
}

// billedNet is what the changes charged minus what they credited, before tax.
func (r *itemRaceRig) billedNet() int64 {
	var net int64
	for _, inv := range r.invoices.all() {
		net += inv.SubtotalCents
	}
	for _, c := range r.credits.all() {
		net -= c.AmountCents
	}
	return net
}

func (r *itemRaceRig) item(t *testing.T) (domain.SubscriptionItem, bool) {
	t.Helper()
	sub, err := r.store.Get(r.ctx, r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	for _, it := range sub.Items {
		if it.ID == r.itemID {
			return it, true
		}
	}
	return domain.SubscriptionItem{}, false
}

func errorCode(rr *httptest.ResponseRecorder) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return body.Error.Code
}

// TestItemChange_OverlappingChanges_BillOnceFromTheLatestState: in every
// case below, the net billed is what one change from the starting state to
// the final state would bill, and the final state is what the requests that
// succeeded asked for. Before #1270 the second change was priced from the
// starting state, so each case billed a seat change twice.
//
// Mutation-verify: drop FOR NO KEY UPDATE from LockForItemChangeTx (or price
// from the handler's own read again) and every case bills the second change
// from the starting state.
func TestItemChange_OverlappingChanges_BillOnceFromTheLatestState(t *testing.T) {
	db := testutil.SetupTestDB(t)

	// half is a monthly amount prorated over the 15 of 30 days left.
	half := func(monthlyCents int64) int64 { return monthlyCents / 2 }

	cases := []struct {
		name       string
		qty        int64
		withKeeper bool
		first      func(r *itemRaceRig) raceReq
		second     func(r *itemRaceRig) raceReq
		wantSecond int
		// wantCode is the second request's error code when it is refused.
		wantCode    string
		wantRemoved bool
		wantQty     int64
		wantPlan    string // plan code
		wantNet     int64
	}{
		{
			// The issue's example: the customer ends on 8 seats and must pay
			// for 3 more, not 5 + 3.
			name: "5 to 10, then 5 to 8: the second is priced 10 to 8",
			qty:  5,
			first: func(*itemRaceRig) raceReq {
				return raceQty(10)
			},
			second: func(*itemRaceRig) raceReq {
				return raceQty(8)
			},
			wantSecond: http.StatusOK,
			wantQty:    8, wantPlan: "basic",
			wantNet: half(3 * raceBasicCents),
		},
		{
			name: "5 to 8, then 5 to 10: the second is priced 8 to 10",
			qty:  5,
			first: func(*itemRaceRig) raceReq {
				return raceQty(8)
			},
			second: func(*itemRaceRig) raceReq {
				return raceQty(10)
			},
			wantSecond: http.StatusOK,
			wantQty:    10, wantPlan: "basic",
			wantNet: half(5 * raceBasicCents),
		},
		{
			// A double click, or a retry without an Idempotency-Key.
			name: "the same upgrade twice: the second finds the item already on the plan",
			qty:  2,
			first: func(r *itemRaceRig) raceReq {
				return racePlan(r.plans["pro"].ID)
			},
			second: func(r *itemRaceRig) raceReq {
				return racePlan(r.plans["pro"].ID)
			},
			wantSecond: http.StatusUnprocessableEntity, wantCode: "validation_error",
			wantQty: 2, wantPlan: "pro",
			wantNet: half(2 * (raceProCents - raceBasicCents)),
		},
		{
			name: "the same decrease twice: the second finds the item already at that quantity",
			qty:  10,
			first: func(*itemRaceRig) raceReq {
				return raceQty(8)
			},
			second: func(*itemRaceRig) raceReq {
				return raceQty(8)
			},
			wantSecond: http.StatusUnprocessableEntity, wantCode: "validation_error",
			wantQty: 8, wantPlan: "basic",
			wantNet: -half(2 * raceBasicCents),
		},
		{
			// The quantity change must be priced on the plan the item is on
			// now, not on the plan it was on when the request was read.
			name: "an upgrade, then a quantity change: the quantity change is priced on the new plan",
			qty:  5,
			first: func(r *itemRaceRig) raceReq {
				return racePlan(r.plans["pro"].ID)
			},
			second: func(*itemRaceRig) raceReq {
				return raceQty(10)
			},
			wantSecond: http.StatusOK,
			wantQty:    10, wantPlan: "pro",
			wantNet: half(10*raceProCents - 5*raceBasicCents),
		},
		{
			// Removing the item credits every seat it has, including the
			// five the first change just charged for.
			name:       "5 to 10, then remove: the removal credits 10 seats",
			qty:        5,
			withKeeper: true,
			first: func(*itemRaceRig) raceReq {
				return raceQty(10)
			},
			second: func(*itemRaceRig) raceReq {
				return raceRemove
			},
			wantSecond:  http.StatusNoContent,
			wantRemoved: true,
			wantNet:     -half(5 * raceBasicCents),
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newItemRaceRig(t, db, fmt.Sprintf("Item Race %d", i), tc.qty, tc.withKeeper)
			first, second := r.race(t, tc.first(r), tc.second(r))

			if first.Code != http.StatusOK {
				t.Fatalf("first change: %d %s, want 200", first.Code, first.Body.String())
			}
			if second.Code != tc.wantSecond {
				t.Errorf("second change: %d %s, want %d", second.Code, second.Body.String(), tc.wantSecond)
			}
			if tc.wantCode != "" && errorCode(second) != tc.wantCode {
				t.Errorf("second change error code = %q, want %q (%s)", errorCode(second), tc.wantCode, second.Body.String())
			}

			it, live := r.item(t)
			switch {
			case tc.wantRemoved && live:
				t.Errorf("item is still on the subscription (qty %d), want it removed", it.Quantity)
			case !tc.wantRemoved && !live:
				t.Errorf("item is gone, want it on plan %s at qty %d", tc.wantPlan, tc.wantQty)
			case !tc.wantRemoved:
				if it.Quantity != tc.wantQty || it.PlanID != r.plans[tc.wantPlan].ID {
					t.Errorf("item = plan %s qty %d, want plan %s (%s) qty %d", it.PlanID, it.Quantity, r.plans[tc.wantPlan].ID, tc.wantPlan, tc.wantQty)
				}
			}

			if got := r.billedNet(); got != tc.wantNet {
				t.Errorf("net billed = %d cents, want %d (invoices %+v, credits %+v)", got, tc.wantNet, r.invoices.all(), r.credits.all())
			}
		})
	}
}

// TestItemWriters_SubscriptionRowFirst_NoDeadlock pins the lock order of
// item writers. A plan swap locks the subscription row with its period CAS
// and then writes the item. The billing run's scheduled-plan apply
// (ApplyDuePendingItemPlansAtomic) used to write the item first and then ask
// for the subscription row (for the item's billing interval). Run together
// on one item, each waited for the other: Postgres aborted one of them with
// a deadlock error. #1270 makes every item change take the subscription row
// first, like the swap, so every writer of subscription_items must too.
//
// Mutation-verify: drop lockSubscriptionRow from
// ApplyDuePendingItemPlansAtomic and one side fails with SQLSTATE 40P01.
func TestItemWriters_SubscriptionRowFirst_NoDeadlock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newItemRaceRig(t, db, "Item Writers Lock Order", 5, false)
	ctx := clock.WithEffectiveNow(r.ctx, proNow)

	// A scheduled change to the keeper plan, due at the period end.
	if _, err := r.store.SetItemPendingPlan(ctx, r.tenantID, r.itemID, r.plans["keeper"].ID, proPeriodEnd); err != nil {
		t.Fatalf("schedule plan change: %v", err)
	}
	sub, err := r.store.Get(ctx, r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}

	// The swap's order: the period CAS (here verify-only) holds the row ...
	tx, err := db.BeginTx(ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := r.store.ClosePeriodTx(ctx, tx, r.tenantID, r.subID, SnapshotOf(sub),
		*sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd, *sub.NextBillingAt, sub.BillingAnchorDay); err != nil {
		t.Fatalf("period CAS: %v", err)
	}

	// ... while the billing run applies the scheduled change.
	applied := make(chan error, 1)
	go func() {
		_, err := r.store.ApplyDuePendingItemPlansAtomic(ctx, r.tenantID, r.subID, proPeriodEnd)
		applied <- err
	}()
	testutil.WaitForLockWaiter(t, r.ctx, testutil.AdminPool(t), "subscription")

	// ... then the swap writes the item.
	_, swapErr := r.store.ApplyItemPlanImmediatelyTx(ctx, tx, r.tenantID, r.itemID, r.plans["pro"].ID, proNow)
	if swapErr == nil {
		swapErr = tx.Commit()
	} else {
		postgres.Rollback(tx)
	}
	applyErr := <-applied

	if swapErr != nil || applyErr != nil {
		t.Fatalf("plan write: %v; scheduled-plan apply: %v; want both to succeed, one after the other", swapErr, applyErr)
	}
	// The immediate change superseded the scheduled one: the apply that ran
	// after it found nothing due.
	it, live := r.item(t)
	if !live || it.PlanID != r.plans["pro"].ID || it.PendingPlanID != "" {
		t.Fatalf("item = live %v plan %s pending %q, want the pro plan and nothing scheduled", live, it.PlanID, it.PendingPlanID)
	}
}

// TestItemChange_SwapAfterAQuantityChange_Refused: a monthly-to-yearly swap
// prices the old period's refund and the new period's invoice from the
// items it read before its transaction. A quantity change that commits in
// between does not move the period, so the swap's period check passes. The
// swap must still see that the items changed and refuse with 409
// item_changed: with the billing engine wired, it would refund 5 seats of
// the old period when the customer has paid for 10.
//
// Mutation-verify: drop the item comparison after ClosePeriodTx in
// applyCrossIntervalPlanSwapTx and the swap commits (200).
func TestItemChange_SwapAfterAQuantityChange_Refused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newItemRaceRig(t, db, "Item Race Swap", 5, false)

	first, second := r.race(t, raceQty(10), racePlan(r.plans["basic-yearly"].ID))

	if first.Code != http.StatusOK {
		t.Fatalf("quantity change: %d %s, want 200", first.Code, first.Body.String())
	}
	if second.Code != http.StatusConflict || errorCode(second) != "item_changed" {
		t.Fatalf("swap: %d %s, want 409 item_changed", second.Code, second.Body.String())
	}
	it, live := r.item(t)
	if !live || it.PlanID != r.plans["basic"].ID || it.Quantity != 10 {
		t.Fatalf("item = live %v plan %s qty %d, want the basic plan at qty 10", live, it.PlanID, it.Quantity)
	}
	sub, err := r.store.Get(r.ctx, r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if !sub.CurrentBillingPeriodStart.Equal(proPeriodStart) || !sub.CurrentBillingPeriodEnd.Equal(proPeriodEnd) {
		t.Fatalf("period = [%v, %v), want it unchanged [%v, %v)", sub.CurrentBillingPeriodStart, sub.CurrentBillingPeriodEnd, proPeriodStart, proPeriodEnd)
	}
}
