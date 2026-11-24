package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A top-up through the real router and database: an invoice created with one
// commit line, which stays a draft until it is paid.

type topupEnv struct {
	db  *postgres.DB
	ts  *httptest.Server
	key string
}

func newTopupEnv(t *testing.T, name string) topupEnv {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, name)
	return topupEnv{db: db, ts: ts, key: createAPIKeyWithMode(t, db, tenantID, false)}
}

func (e topupEnv) call(t *testing.T, method, path string, body any) flowResp {
	t.Helper()
	return flowDo(t, e.ts, method, path, flowReq{bearer: e.key, body: body})
}

// create makes a customer and a top-up for it, and returns both ids.
func (e topupEnv) create(t *testing.T) (customerID, invoiceID string) {
	t.Helper()
	customerID = flowStr(e.call(t, "POST", "/v1/customers", map[string]any{
		"external_id": "topup", "display_name": "Topup", "email": "topup@topup.test",
	}).want(t, 201, "create customer").body, "id")
	inv := e.call(t, "POST", "/v1/invoices", map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Prepaid credit", "quantity": 1, "unit_amount_cents": 9000, "commit_granted_cents": 10000}},
	}).want(t, 201, "create top-up").body
	if flowStr(inv, "billing_reason") != "topup" || flowStr(inv, "invoice_number") != "" {
		t.Fatalf("created billing_reason %q number %q, want an unnumbered topup", flowStr(inv, "billing_reason"), flowStr(inv, "invoice_number"))
	}
	return customerID, flowStr(inv, "id")
}

// A top-up is sold whole: its hosted page may already be open, so no line
// can be added to it, a charge or a second commit line alike.
func TestAddLineItem_TopupIsFrozen(t *testing.T) {
	e := newTopupEnv(t, "Topup Frozen Co")
	_, id := e.create(t)
	for name, line := range map[string]map[string]any{
		"a charge":      {"description": "Setup", "quantity": 1, "unit_amount_cents": 1000},
		"a commit line": {"description": "More credit", "quantity": 1, "unit_amount_cents": 1000, "commit_granted_cents": 1000},
	} {
		r := e.call(t, "POST", "/v1/invoices/"+id+"/line-items", line)
		if r.status != 409 || flowStr(r.body, "error", "code") != "invoice_not_manual" {
			t.Errorf("add %s: %d %s, want 409 invoice_not_manual", name, r.status, r.raw)
		}
	}
}

// An unpaid top-up owes nothing, so it stays out of every figure of money
// owed, even after a declined charge.
func TestTopup_UnpaidIsOutOfHomeOpenListOutstandingAndDunning(t *testing.T) {
	e := newTopupEnv(t, "Topup Unpaid Co")
	customerID, id := e.create(t)
	// A declined saved-card charge leaves the draft payment_status failed;
	// Stripe is not wired here, so the row is stamped as that charge leaves it.
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := e.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET payment_status = 'failed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	ov := e.call(t, "GET", "/v1/analytics/overview?period=30d", nil).want(t, 200, "overview").body
	for _, figure := range []string{"outstanding_ar", "open_invoices", "failed_payments"} {
		if got := flowInt(ov, figure); got != 0 {
			t.Errorf("Home %s = %d, want 0", figure, got)
		}
	}
	if open := flowList(e.call(t, "GET", "/v1/invoices?status=finalized", nil).want(t, 200, "open list").body, "data"); len(open) != 0 {
		t.Errorf("open list has %d invoices, want 0", len(open))
	}
	cust := e.call(t, "GET", "/v1/customers/"+customerID+"/overview", nil).want(t, 200, "customer overview").body
	if n, owed := flowInt(cust, "outstanding_balance", "unpaid_count"), flowInt(cust, "outstanding_balance", "total_cents"); n != 0 || owed != 0 {
		t.Errorf("Outstanding: %d unpaid, %d cents, want none", n, owed)
	}
	store := invoice.NewPostgresStore(e.db)
	failed, err := store.ListFailedWithoutDunningRun(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.ListAutoChargePending(ctx, walk.Key{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range append(failed, queued...) {
		if inv.ID == id {
			t.Errorf("the top-up is listed for dunning or automatic collection")
		}
	}
}
