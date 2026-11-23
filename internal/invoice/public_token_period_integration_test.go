package invoice_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The hosted page looks its invoice up by token on a context with no mode,
// before it knows the tenant. An invoice with no billing timezone of its own
// shows its period in the tenant's, which is read from the tenant's settings
// under the invoice's mode: the period reads in Kolkata, not in UTC.
func TestGetByPublicToken_PeriodInTheTenantsZone(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	store.SetEncryptor(enc)
	settings := tenant.NewSettingsStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Public Token Period")
	if _, err := settings.Update(ctx, tenantID, func(ts *domain.TenantSettings) error {
		ts.Timezone = "Asia/Kolkata"
		return nil
	}); err != nil {
		t.Fatalf("set the tenant's timezone: %v", err)
	}
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_period", DisplayName: "Period"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	start := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC) // May 2 00:00 IST
	end := time.Date(2026, 5, 31, 18, 30, 0, 0, time.UTC)  // Jun 1 00:00 IST
	issued := end
	inv, err := store.CreateWithLineItems(clock.WithEffectiveNow(ctx, end), tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-PERIOD-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 100, TotalAmountCents: 100, AmountDueCents: 100,
		BillingPeriodStart: start, BillingPeriodEnd: end, IssuedAt: &issued,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	token, err := invoice.GeneratePublicToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := store.SetPublicToken(ctx, tenantID, inv.ID, token); err != nil {
		t.Fatalf("set public token: %v", err)
	}

	svc := invoice.NewService(store, clock.Real(), settings)
	got, live, err := svc.GetByPublicToken(context.Background(), token)
	if err != nil {
		t.Fatalf("GetByPublicToken: %v", err)
	}
	want := domain.FormatInclusivePeriod(start, end, domain.LoadLocationOrUTC("Asia/Kolkata"))
	if live || got.ID != inv.ID || got.BillingPeriodDisplay != want {
		t.Errorf("invoice %s (live %v), period %q; want %s in test mode, %q", got.ID, live, got.BillingPeriodDisplay, inv.ID, want)
	}
}
