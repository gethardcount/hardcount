package invoice_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Money received another way for a top-up is one call: the payment issues it,
// numbers it, marks it paid out of band and grants its credit.
func TestTopup_RecordPayment_IssuesPaysAndGrantsInOneCall(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	outbox := webhook.NewOutboxStore(db)
	invStore := invoice.NewPostgresStore(db)
	invStore.SetOutboxEnqueuer(outbox)
	creditStore := credit.NewPostgresStore(db)
	creditStore.SetOutboxEnqueuer(outbox)
	invStore.SetCommitFunder(credit.NewService(creditStore))
	settings := tenant.NewSettingsStore(db)
	invStore.SetInvoiceNumberer(settings)
	invSvc := invoice.NewService(invStore, nil, settings)

	tenantID := testutil.CreateTestTenant(t, db, "Topup Record Payment")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_topup_offline", DisplayName: "Offline"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	grant := int64(10000)
	draft, err := invSvc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: cust.ID, Currency: "USD",
		LineItems: []invoice.AddLineItemInput{{Description: "Prepaid credit", Quantity: 1, UnitAmountCents: 9000, CommitGrantedCents: &grant}},
	})
	if err != nil {
		t.Fatalf("create top-up: %v", err)
	}

	paid, err := invSvc.RecordOfflinePayment(ctx, tenantID, draft.ID, "Wire 42")
	if err != nil {
		t.Fatalf("record payment: %v", err)
	}
	if paid.Status != domain.InvoicePaid || paid.InvoiceNumber == "" || !paid.PaidOutOfBand || paid.AmountPaidCents != 9000 {
		t.Fatalf("after recording: %s %q out-of-band=%t paid %d, want paid, numbered, out of band, 9000",
			paid.Status, paid.InvoiceNumber, paid.PaidOutOfBand, paid.AmountPaidCents)
	}
	if paid.IssuedAt == nil || paid.PaidAt == nil || !paid.IssuedAt.Equal(*paid.PaidAt) || paid.DueAt != nil {
		t.Errorf("issued_at %v paid_at %v due_at %v, want issued at the payment and no due date", paid.IssuedAt, paid.PaidAt, paid.DueAt)
	}
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var grants int
	var granted int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger
		WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, draft.ID).Scan(&grants, &granted); err != nil {
		t.Fatal(err)
	}
	if grants != 1 || granted != grant {
		t.Errorf("commit grants %d of %d cents, want one of %d", grants, granted, grant)
	}
}
