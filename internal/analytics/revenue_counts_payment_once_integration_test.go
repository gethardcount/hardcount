package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Revenue and the revenue chart count what customers paid, once, when the
// money arrived. The invoice that bought credit counts its payment; an invoice
// that credit paid adds nothing, since its money was already counted; an
// invoice paid partly by card counts the card part.
func TestRevenue_CountsEachPaymentOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Revenue Once")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_rev_once", DisplayName: "Revenue Once"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	paidAt := time.Now().UTC().Add(-time.Hour)
	paid := func(total, byPayment, byCredit int64) {
		inv := makePaidInvoice(t, ctx, store, tenantID, cust.ID, "USD", total)
		execBypass(t, db, `UPDATE invoices SET amount_paid_cents = $2, credits_applied_cents = $3, paid_at = $4 WHERE id = $1`,
			inv.ID, byPayment, byCredit, paidAt)
	}
	paid(100_00, 100_00, 0) // bought the credit
	paid(30_00, 0, 30_00)   // paid by that credit
	paid(20_00, 5_00, 15_00)
	const want = 105_00 // the two payments; credit not counted again

	h := NewHandler(db)
	get := func(path string, fn func(http.ResponseWriter, *http.Request)) []byte {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
		rr := httptest.NewRecorder()
		fn(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.Bytes()
	}

	var overview OverviewResponse
	if err := json.Unmarshal(get("/overview?period=30d", h.overview), &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.Revenue != want {
		t.Errorf("overview revenue %d, want %d", overview.Revenue, want)
	}

	var chart struct {
		Data []RevenueDataPoint `json:"data"`
	}
	if err := json.Unmarshal(get("/revenue-chart?period=30d", h.revenueChart), &chart); err != nil {
		t.Fatalf("decode revenue chart: %v", err)
	}
	var sum int64
	for _, p := range chart.Data {
		sum += p.RevenueCents
	}
	if sum != want {
		t.Errorf("revenue chart sums to %d, want %d", sum, want)
	}
}
