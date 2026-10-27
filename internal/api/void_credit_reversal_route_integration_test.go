package api

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/config"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// voidRig is the real router and database, a test-mode key, one customer,
// and the credit service the engine and the dunning retry apply credit
// through.
type voidRig struct {
	t        *testing.T
	ctx      context.Context
	call     func(method, path string, req flowReq) flowResp
	credits  *credit.Service
	db       *postgres.DB
	tenantID string
	custID   string
}

func newVoidRig(t *testing.T) *voidRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil,
		WithRateLimits(config.RateLimits{GeneralPerMinute: 10_000, IngestPerSecond: 1000})))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, "Void Reversal Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	r := &voidRig{
		t:        t,
		ctx:      postgres.WithLivemode(context.Background(), false),
		credits:  credit.NewService(credit.NewPostgresStore(db)),
		db:       db,
		tenantID: tenantID,
	}
	r.call = func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	r.custID = flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "void-rev", "display_name": "Void Reversal Customer",
	}}).want(t, 201, "create customer").body, "id")
	return r
}

// finalizedInvoice creates and finalizes a 100.00 USD manual invoice. The
// customer holds no credit while it finalizes, so none is applied.
func (r *voidRig) finalizedInvoice() string {
	r.t.Helper()
	id := flowStr(r.call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": r.custID, "currency": "USD",
		"line_items": []map[string]any{{"description": "Seats", "quantity": 1, "unit_amount_cents": 10000}},
	}}).want(r.t, 201, "create invoice").body, "id")
	r.call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(r.t, 200, "finalize")
	return id
}

// grant gives the customer cents of USD credit.
func (r *voidRig) grant(cents int64) {
	r.t.Helper()
	if _, err := r.credits.Grant(r.ctx, r.tenantID, credit.GrantInput{
		CustomerID: r.custID, Currency: "USD", AmountCents: cents, Description: "fixture",
	}); err != nil {
		r.t.Fatalf("grant %d: %v", cents, err)
	}
}

func (r *voidRig) balance() int64 {
	r.t.Helper()
	for _, b := range flowList(r.call("GET", "/v1/credits/balance/"+r.custID, flowReq{}).want(r.t, 200, "balance").body, "data") {
		if flowStr(b, "currency") == "USD" {
			return flowInt(b, "balance_cents")
		}
	}
	return 0
}

// A void hands back every credit the invoice used. The reversal added up the
// invoice's usage rows from one page of the ledger list, at most 100 rows, so
// an invoice paid by 101 separate credit applications got 100 of them back.
// Each application here draws 0.01: the customer is granted 0.01, and the
// apply draws all of it.
func TestVoid_ReturnsEveryCreditApplication(t *testing.T) {
	skipShort(t)
	const applications = 101
	r := newVoidRig(t)
	invoiceID := r.finalizedInvoice()

	for i := range applications {
		r.grant(1)
		drawn, err := r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.custID, invoiceID, 1)
		if err != nil || drawn != 1 {
			t.Fatalf("application %d: drew %d (%v), want 1", i+1, drawn, err)
		}
	}
	if got := r.balance(); got != 0 {
		t.Fatalf("balance after %d applications = %d, want 0", applications, got)
	}

	r.call("POST", "/v1/invoices/"+invoiceID+"/void", flowReq{}).want(t, 200, "void")

	if got := r.balance(); got != applications {
		t.Errorf("balance after the void = %d, want %d: every application handed back", got, applications)
	}
}

// A credit application racing the void, with the order forced. A test
// transaction holds the invoice row; the void queues on it first, then the
// application. When the test lets go, the void runs first. The application
// must wait for the void and then find the invoice voided and draw nothing:
// it locks the invoice before it writes a usage row. Were it to write its
// usage row first, the void's sum would miss that row, and the customer would
// end short by what it drew. Each round uses a new invoice.
func TestVoid_RacingCreditApplication_ReturnsWhatWasDrawn(t *testing.T) {
	skipShort(t)
	const rounds = 3
	r := newVoidRig(t)
	var held int64
	for round := range rounds {
		invoiceID := r.finalizedInvoice()
		// One application already on the invoice, so every void has
		// something to hand back.
		r.grant(10)
		if drawn, err := r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.custID, invoiceID, 10); err != nil || drawn != 10 {
			t.Fatalf("round %d: first application drew %d (%v), want 10", round, drawn, err)
		}
		r.grant(25)
		held += 35

		gate, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			t.Fatalf("round %d: begin gate: %v", round, err)
		}
		if _, err := gate.ExecContext(r.ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID); err != nil {
			postgres.Rollback(gate)
			t.Fatalf("round %d: hold the invoice: %v", round, err)
		}

		var wg sync.WaitGroup
		var voided flowResp
		var drawn int64
		var applyErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			voided = r.call("POST", "/v1/invoices/"+invoiceID+"/void", flowReq{})
		}()
		r.waitForLockWaiters(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			drawn, applyErr = r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.custID, invoiceID, 25)
		}()
		r.waitForLockWaiters(2)
		postgres.Rollback(gate)
		wg.Wait()

		voided.want(t, 200, "void")
		if applyErr != nil {
			t.Fatalf("round %d: racing application: %v", round, applyErr)
		}
		if drawn != 0 {
			t.Errorf("round %d: the application after the void drew %d, want 0", round, drawn)
		}
		if got := r.balance(); got != held {
			t.Fatalf("round %d: balance after the void = %d, want %d (the racing application drew %d)", round, got, held, drawn)
		}
	}
}

// waitForLockWaiters waits until n sessions of the test database wait on a
// lock.
func (r *voidRig) waitForLockWaiters(n int) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := r.db.Pool.QueryRowContext(r.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			r.t.Fatalf("read lock waiters: %v", err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d sessions wait on a lock after 10s, want %d", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
