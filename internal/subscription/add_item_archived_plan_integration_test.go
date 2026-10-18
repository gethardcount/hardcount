package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestAddItem_ArchivedPlan_RefusedOnBothPaths is the real-Postgres lock for
// #1194: POST /v1/subscriptions/{id}/items with an archived plan is a 409
// with Create's message, and nothing changes. ADR-067: an archived plan takes
// no new business; adding an item is new business on that plan.
//
// The handler has two add paths, and both are driven here:
//   - an active subscription mid-period takes the atomic path
//     (atomicAddItemWithProration → Service.AddItemTx on the handler's tx);
//   - a trialing subscription owes no proration, so it takes the plain path
//     (Service.AddItem, own tx).
//
// Each refusal is checked against the database (items, billing intervals and
// the item-change history are unchanged) and against the proration fakes (no
// invoice, no credit). The active-plan controls prove each path really ran:
// the atomic one cuts a proration invoice, the plain one does not.
func TestAddItem_ArchivedPlan_RefusedOnBothPaths(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "AddItem Archived Plan")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_add_arch", DisplayName: "Add Archived",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	ps := pricing.NewPostgresStore(db)
	newPlan := func(code string, cents int64) domain.Plan {
		p, err := ps.CreatePlan(ctx, tenantID, domain.Plan{
			Code: code, Name: code, Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
			BaseAmountCents: cents, Status: domain.PlanActive,
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", code, err)
		}
		return p
	}
	basePlan := newPlan("arch-base", 6000)
	archPlan := newPlan("arch-retired", 3000)
	okPlan := newPlan("arch-ok", 2000)
	// Archive the way an operator does: an active plan set to archived.
	archPlan.Status = domain.PlanArchived
	if _, err := ps.UpdatePlan(ctx, tenantID, archPlan); err != nil {
		t.Fatalf("archive plan: %v", err)
	}
	if got, err := ps.GetPlan(ctx, tenantID, archPlan.ID); err != nil || got.Status != domain.PlanArchived {
		t.Fatalf("plan must read back archived: status=%q err=%v", got.Status, err)
	}

	store := NewPostgresStore(db)
	svc := NewService(store, nil)
	svc.SetPlanReader(ps)

	invoices := &invoicesMock{
		// A PAID source invoice, so the atomic path's proration reaches the
		// charge-invoice write in the active-plan control.
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TotalAmountCents: 6000, CreatedAt: proPeriodStart,
		},
	}
	credits := &creditsMock{}
	h := NewHandler(svc)
	h.SetDB(db)
	h.SetProrationDeps(ps, invoices, credits)

	trialPeriodEnd := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	activeSub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-arch-active", DisplayName: "Active", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &proPeriodStart,
		CurrentBillingPeriodStart: &proPeriodStart,
		CurrentBillingPeriodEnd:   &proPeriodEnd,
		NextBillingAt:             &proPeriodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: basePlan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create active sub: %v", err)
	}
	trialSub, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-arch-trial", DisplayName: "Trial", CustomerID: cust.ID,
		Status: domain.SubscriptionTrialing, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &proPeriodStart,
		TrialStartAt:              &proPeriodStart,
		TrialEndAt:                &proPeriodEnd,
		CurrentBillingPeriodStart: &proPeriodEnd,
		CurrentBillingPeriodEnd:   &trialPeriodEnd,
		NextBillingAt:             &trialPeriodEnd,
		Items:                     []domain.SubscriptionItem{{PlanID: basePlan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create trialing sub: %v", err)
	}

	countRows := func(t *testing.T, table, subID string) int {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE subscription_id = $1`, subID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	tables := []string{"subscription_items", "billing_intervals", "subscription_item_changes"}
	snapshot := func(t *testing.T, subID string) map[string]int {
		t.Helper()
		out := make(map[string]int, len(tables))
		for _, tbl := range tables {
			out[tbl] = countRows(t, tbl, subID)
		}
		return out
	}

	reqCtx := clock.WithEffectiveNow(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), proNow)
	addItem := func(subID, planID string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(AddItemInput{PlanID: planID, Quantity: 1})
		rr := httptest.NewRecorder()
		h.addItem(rr, addItemURL(reqCtx, subID, body))
		return rr
	}

	cases := []struct {
		name         string
		sub          domain.Subscription
		wantInvoices int // proration invoices cut by the active-plan control
	}{
		{name: "active mid-period (atomic path, AddItemTx)", sub: activeSub, wantInvoices: 1},
		{name: "trialing (plain path, AddItem)", sub: trialSub, wantInvoices: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoicesBefore := len(invoices.createdInvoices)
			grantsBefore := len(credits.grantCalls)
			before := snapshot(t, tc.sub.ID)

			rr := addItem(tc.sub.ID, archPlan.ID)
			if rr.Code != http.StatusConflict {
				t.Fatalf("add item on archived plan: status %d, want 409; body: %s", rr.Code, rr.Body.String())
			}
			var resp struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error body: %v; body: %s", err, rr.Body.String())
			}
			wantMsg := `plan "` + archPlan.ID + `" is archived — unarchive it or pick an active plan`
			if resp.Error.Code != "invalid_state" || !strings.Contains(resp.Error.Message, wantMsg) {
				t.Fatalf("refusal: code=%q message=%q, want invalid_state with %q", resp.Error.Code, resp.Error.Message, wantMsg)
			}

			// Nothing changed: no item row, no interval, no change-history
			// row, no proration invoice or credit.
			after := snapshot(t, tc.sub.ID)
			for _, tbl := range tables {
				if after[tbl] != before[tbl] {
					t.Errorf("refused add changed %s: %d rows before, %d after", tbl, before[tbl], after[tbl])
				}
			}
			if n := len(invoices.createdInvoices) - invoicesBefore; n != 0 {
				t.Errorf("refused add cut %d proration invoice(s), want 0", n)
			}
			if n := len(credits.grantCalls) - grantsBefore; n != 0 {
				t.Errorf("refused add granted %d credit(s), want 0", n)
			}

			// Control: an active plan is added on the same path.
			rr = addItem(tc.sub.ID, okPlan.ID)
			if rr.Code != http.StatusCreated {
				t.Fatalf("add item on active plan: status %d, want 201; body: %s", rr.Code, rr.Body.String())
			}
			if got := countRows(t, "subscription_items", tc.sub.ID); got != before["subscription_items"]+1 {
				t.Fatalf("active-plan add: %d item rows, want %d", got, before["subscription_items"]+1)
			}
			if n := len(invoices.createdInvoices) - invoicesBefore; n != tc.wantInvoices {
				t.Fatalf("active-plan add cut %d proration invoice(s), want %d (proves which path ran)", n, tc.wantInvoices)
			}
		})
	}
}
