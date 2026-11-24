package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// topupHarness wires the invoice store with credit as its commit funder and
// the tenant settings as its numberer, as wire.go does, around one customer.
type topupHarness struct {
	db        *postgres.DB
	ctx       context.Context
	tenantID  string
	custID    string
	invSvc    *invoice.Service
	creditSvc *credit.Service
}

func newTopupHarness(t *testing.T, name string) *topupHarness {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	outbox := webhook.NewOutboxStore(db)
	invStore := invoice.NewPostgresStore(db)
	invStore.SetOutboxEnqueuer(outbox)
	creditStore := credit.NewPostgresStore(db)
	creditStore.SetOutboxEnqueuer(outbox)
	creditSvc := credit.NewService(creditStore)
	invStore.SetCommitFunder(creditSvc)
	settings := tenant.NewSettingsStore(db)
	invStore.SetInvoiceNumberer(settings)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_topup_offline", DisplayName: "Offline"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	return &topupHarness{db: db, ctx: ctx, tenantID: tenantID, custID: cust.ID,
		invSvc: invoice.NewService(invStore, nil, settings), creditSvc: creditSvc}
}

func (h *topupHarness) create(t *testing.T, ctx context.Context, item invoice.AddLineItemInput) (domain.Invoice, error) {
	t.Helper()
	return h.invSvc.Create(ctx, h.tenantID, invoice.CreateInput{CustomerID: h.custID, Currency: "USD", LineItems: []invoice.AddLineItemInput{item}})
}

func (h *topupHarness) topup(t *testing.T) domain.Invoice {
	t.Helper()
	grant := int64(10000)
	draft, err := h.create(t, h.ctx, invoice.AddLineItemInput{Description: "Prepaid credit", Quantity: 1, UnitAmountCents: 9000, CommitGrantedCents: &grant})
	if err != nil {
		t.Fatalf("create top-up: %v", err)
	}
	return draft
}

// Money received another way for a top-up is one call: the payment issues it,
// numbers it, marks it paid out of band and grants its credit.
func TestTopup_RecordPayment_IssuesPaysAndGrantsInOneCall(t *testing.T) {
	h := newTopupHarness(t, "Topup Record Payment")
	draft := h.topup(t)

	paid, err := h.invSvc.RecordOfflinePayment(h.ctx, h.tenantID, draft.ID, "Wire 42")
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
	tx, err := h.db.BeginTx(h.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var grants int
	var granted int64
	if err := tx.QueryRowContext(h.ctx, `SELECT count(*), COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger
		WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, draft.ID).Scan(&grants, &granted); err != nil {
		t.Fatal(err)
	}
	if grants != 1 || granted != 10000 {
		t.Errorf("commit grants %d of %d cents, want one of 10000", grants, granted)
	}
}

// A top-up's settle takes the customer's credit lock before the tenant's
// number row, the order a commit refund and a cancel keep (customer lock,
// then a number). While the settle waits for a customer lock another
// transaction holds, the number row must stay free: with the number taken
// first, an invoice create for the tenant would wait behind the settle, and
// a refund holding the customer lock and asking for a number would deadlock
// with it.
func TestTopup_SettleTakesTheCustomerLockBeforeTheNumber(t *testing.T) {
	h := newTopupHarness(t, "Topup Lock Order")
	draft := h.topup(t)

	holder, err := h.db.BeginTx(h.ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(holder)
	if err := h.creditSvc.LockCustomerCreditTx(h.ctx, holder, h.tenantID, h.custID); err != nil {
		t.Fatalf("hold the customer's credit lock: %v", err)
	}

	settled := make(chan error, 1)
	go func() {
		_, err := h.invSvc.RecordOfflinePayment(h.ctx, h.tenantID, draft.ID, "Wire 7")
		settled <- err
	}()
	testutil.WaitForLockWaiter(t, h.ctx, testutil.AdminPool(t), "pg_advisory_xact_lock")

	// With the settle waiting, an ordinary invoice for the tenant takes the
	// next number at once.
	createCtx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	if _, err := h.create(t, createCtx, invoice.AddLineItemInput{Description: "Setup", Quantity: 1, UnitAmountCents: 1000}); err != nil {
		t.Fatalf("an invoice created while the settle waits for the customer lock: %v (the settle holds the number row)", err)
	}

	if err := holder.Commit(); err != nil {
		t.Fatalf("release the customer lock: %v", err)
	}
	select {
	case err := <-settled:
		if err != nil {
			t.Fatalf("settle after the lock was released: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the settle did not finish after the customer lock was released")
	}
}
