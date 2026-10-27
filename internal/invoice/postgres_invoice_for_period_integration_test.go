package invoice_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// GetInvoiceForPeriod is the invoice holding a subscription period's slot,
// the row a cycle close that hit the slot heals. A manual invoice with the
// same subscription and period does not hold the slot (#1331), so it is not
// that invoice: alone it reads as no invoice, and beside a cycle invoice the
// cycle invoice is returned, even when the manual one is newer.
func TestGetInvoiceForPeriod_SkipsManualInvoices(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := postgres.WithLivemode(context.Background(), false)
	invStore := invoice.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "Invoice For Period Corp")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_ifp", DisplayName: "IFP"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{Code: "ifp", Name: "IFP", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 1000})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	periodStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-ifp", DisplayName: "IFP Sub", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar, StartedAt: &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	create := func(num string, reason domain.InvoiceBillingReason, createdAt time.Time) domain.Invoice {
		t.Helper()
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: num,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
			SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: 1000,
			BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd, BillingReason: reason,
			CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("create %s invoice: %v", reason, err)
		}
		return inv
	}

	create("IFP-MANUAL", domain.BillingReasonManual, periodStart.Add(time.Hour))
	if got, err := invStore.GetInvoiceForPeriod(ctx, tenantID, sub.ID, periodStart, periodEnd); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("only a manual invoice in the period: got %s (%s), err %v; want ErrNotFound", got.ID, got.BillingReason, err)
	}

	cycle := create("IFP-CYCLE", domain.BillingReasonSubscriptionCycle, periodStart.Add(2*time.Hour))
	create("IFP-MANUAL-2", domain.BillingReasonManual, periodStart.Add(3*time.Hour))
	got, err := invStore.GetInvoiceForPeriod(ctx, tenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("get invoice for period: %v", err)
	}
	if got.ID != cycle.ID {
		t.Errorf("invoice for period = %s (%s), want the cycle invoice %s", got.ID, got.BillingReason, cycle.ID)
	}
}
