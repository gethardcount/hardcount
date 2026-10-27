package api

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

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

// A credit application racing the void. The void locks the invoice and adds
// up the applications inside that lock, so an application either commits
// before the lock and is handed back, or waits and then finds the invoice
// voided and draws nothing. Either way the customer ends with all of the
// credit. Each round runs both at once on a new invoice.
func TestVoid_RacingCreditApplication_ReturnsWhatWasDrawn(t *testing.T) {
	skipShort(t)
	const rounds = 8
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

		var wg sync.WaitGroup
		start := make(chan struct{})
		var drawn int64
		var applyErr error
		var voided flowResp
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			drawn, applyErr = r.credits.ApplyToInvoice(r.ctx, r.tenantID, r.custID, invoiceID, 25)
		}()
		go func() {
			defer wg.Done()
			<-start
			voided = r.call("POST", "/v1/invoices/"+invoiceID+"/void", flowReq{})
		}()
		close(start)
		wg.Wait()
		voided.want(t, 200, "void")
		if applyErr != nil {
			t.Fatalf("round %d: racing application: %v", round, applyErr)
		}
		if got := r.balance(); got != held {
			t.Fatalf("round %d: balance after the void = %d, want %d (the racing application drew %d)", round, got, held, drawn)
		}
	}
}
