package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The manual-invoice routes (POST /v1/invoices, POST /v1/invoices/{id}/line-items)
// used to get their audit rows from the HTTP catch-all middleware, which GUESSED
// the action/resource from the URL. These tests pin the ADR-090 replacement:
// an explicit in-tx emission on BOTH manual-create store paths and on the
// line-item add, each sharing fate with its business mutation.

var errEmitBoom = errors.New("injected audit emission failure")

// failingEmitter satisfies invoice.AuditLogger and fails every in-tx emission.
// Log (the legacy own-tx writer) is a no-op: only LogInTx is under test.
type failingEmitter struct{ calls int }

func (f *failingEmitter) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}

func (f *failingEmitter) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	f.calls++
	return errEmitBoom
}

// fixedNumberer hands the service a deterministic invoice number so a
// rolled-back create can be proven absent by number lookup.
type fixedNumberer struct{ number string }

func (n *fixedNumberer) NextInvoiceNumber(context.Context, string) (string, error) {
	return n.number, nil
}

// NextInvoiceNumberTx exists only to satisfy InvoiceNumberer — the manual
// create path allocates through NextInvoiceNumber.
func (n *fixedNumberer) NextInvoiceNumberTx(context.Context, *sql.Tx, string) (string, error) {
	return n.number, nil
}

func newAuditedInvoiceService(t *testing.T, db *postgres.DB, logger invoice.AuditLogger, number string) *invoice.Service {
	t.Helper()
	svc := invoice.NewService(invoice.NewPostgresStore(db), nil, &fixedNumberer{number: number})
	svc.SetAuditLogger(logger)
	return svc
}

func seedAuditCustomer(t *testing.T, db *postgres.DB, tenantID, suffix string) domain.Customer {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(postgres.WithLivemode(context.Background(), false), tenantID, domain.Customer{
		ExternalID:  "cus_manual_audit_" + suffix,
		DisplayName: "Manual Invoice Audit " + suffix,
	})
	if err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	return cust
}

// invoiceAuditRows returns every audit row the tenant has for resource_type
// "invoice". A fresh tenant per leg makes "exactly one row" / "no rows" exact.
func invoiceAuditRows(t *testing.T, logger *audit.Logger, ctx context.Context, tenantID string) []domain.AuditEntry {
	t.Helper()
	rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceType: "invoice"})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return rows
}

// wantNum compares a JSONB-round-tripped metadata number (float64) to an int64.
func wantNum(t *testing.T, meta map[string]any, key string, want int64) {
	t.Helper()
	got, ok := meta[key].(float64)
	if !ok {
		t.Errorf("metadata[%q]: got %#v (%T), want a number", key, meta[key], meta[key])
		return
	}
	if int64(got) != want {
		t.Errorf("metadata[%q]: got %d, want %d", key, int64(got), want)
	}
}

// B2 — POST /v1/invoices. The route has TWO store paths (bare header when the
// request carries no lines; CreateWithLineItems when it does). Both must audit,
// and both must share fate with the emission.
func TestInvoiceCreate_AuditsBothStorePathsAndSharesFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	logger := audit.NewLogger(db)
	store := invoice.NewPostgresStore(db)

	t.Run("bare-header create commits one create row", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Invoice Create Bare OK")
		cust := seedAuditCustomer(t, db, tenantID, "bare_ok")
		svc := newAuditedInvoiceService(t, db, logger, "INV-BARE-OK")

		inv, err := svc.Create(ctx, tenantID, invoice.CreateInput{
			CustomerID: cust.ID,
			Currency:   "USD",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		rows := invoiceAuditRows(t, logger, ctx, tenantID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for the bare-header create; got %d: %+v", len(rows), rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionCreate {
			t.Errorf("action: got %q, want %q", row.Action, domain.AuditActionCreate)
		}
		if row.ResourceID != inv.ID {
			t.Errorf("resource_id: got %q, want the invoice id %q", row.ResourceID, inv.ID)
		}
		if row.ResourceLabel != inv.InvoiceNumber {
			t.Errorf("resource_label: got %q, want the invoice number %q", row.ResourceLabel, inv.InvoiceNumber)
		}
		if row.Metadata["customer_id"] != cust.ID {
			t.Errorf("metadata customer_id: got %v, want %q", row.Metadata["customer_id"], cust.ID)
		}
		if row.Metadata["currency"] != "USD" {
			t.Errorf("metadata currency: got %v, want USD", row.Metadata["currency"])
		}
		wantNum(t, row.Metadata, "total_amount_cents", 0)
		wantNum(t, row.Metadata, "line_item_count", 0)
	})

	t.Run("create-with-line-items commits one create row carrying the line count", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Invoice Create Lines OK")
		cust := seedAuditCustomer(t, db, tenantID, "lines_ok")
		svc := newAuditedInvoiceService(t, db, logger, "INV-LINES-OK")

		inv, err := svc.Create(ctx, tenantID, invoice.CreateInput{
			CustomerID: cust.ID,
			Currency:   "USD",
			LineItems: []invoice.AddLineItemInput{
				{Description: "Setup fee", Quantity: 1, UnitAmountCents: 5000},
				{Description: "Support", Quantity: 2, UnitAmountCents: 2500},
			},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if inv.TotalAmountCents != 10000 {
			t.Fatalf("invoice total: got %d, want 10000", inv.TotalAmountCents)
		}

		rows := invoiceAuditRows(t, logger, ctx, tenantID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for the create-with-lines path; got %d: %+v", len(rows), rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionCreate {
			t.Errorf("action: got %q, want %q", row.Action, domain.AuditActionCreate)
		}
		if row.ResourceID != inv.ID || row.ResourceLabel != inv.InvoiceNumber {
			t.Errorf("resource: got (%q,%q), want (%q,%q)", row.ResourceID, row.ResourceLabel, inv.ID, inv.InvoiceNumber)
		}
		wantNum(t, row.Metadata, "total_amount_cents", 10000)
		wantNum(t, row.Metadata, "line_item_count", 2)
	})

	t.Run("emit failure rolls the bare-header create back", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Invoice Create Bare Fail")
		cust := seedAuditCustomer(t, db, tenantID, "bare_fail")
		emitter := &failingEmitter{}
		svc := newAuditedInvoiceService(t, db, emitter, "INV-BARE-FAIL")

		_, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"})
		if !errors.Is(err, errEmitBoom) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if emitter.calls != 1 {
			t.Fatalf("emitter ran %d times, want exactly 1 (the test is vacuous otherwise)", emitter.calls)
		}

		if _, err := store.GetByNumber(ctx, tenantID, "INV-BARE-FAIL"); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("the invoice must roll back with its failed audit emission; GetByNumber err = %v", err)
		}
		if rows := invoiceAuditRows(t, logger, ctx, tenantID); len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back create: %+v", rows)
		}
	})

	t.Run("emit failure rolls the create-with-line-items back", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Invoice Create Lines Fail")
		cust := seedAuditCustomer(t, db, tenantID, "lines_fail")
		emitter := &failingEmitter{}
		svc := newAuditedInvoiceService(t, db, emitter, "INV-LINES-FAIL")

		_, err := svc.Create(ctx, tenantID, invoice.CreateInput{
			CustomerID: cust.ID,
			Currency:   "USD",
			LineItems: []invoice.AddLineItemInput{
				{Description: "Setup fee", Quantity: 1, UnitAmountCents: 5000},
			},
		})
		if !errors.Is(err, errEmitBoom) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if emitter.calls != 1 {
			t.Fatalf("emitter ran %d times, want exactly 1", emitter.calls)
		}

		if _, err := store.GetByNumber(ctx, tenantID, "INV-LINES-FAIL"); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("the composed invoice (header + lines) must roll back with its failed audit emission; GetByNumber err = %v", err)
		}
		if rows := invoiceAuditRows(t, logger, ctx, tenantID); len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back create: %+v", rows)
		}
	})
}

// B3 — POST /v1/invoices/{id}/line-items. Wire vocabulary stays frozen: the add
// is action=update on the INVOICE, discriminated by metadata.action.
func TestInvoiceAddLineItem_AuditsAndSharesFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	logger := audit.NewLogger(db)
	store := invoice.NewPostgresStore(db)

	// seedDraft creates the draft WITHOUT the service (store path, nil emit), so
	// the only audit row a leg can produce is the line-item add itself.
	seedDraft := func(t *testing.T, tenantID, suffix string) domain.Invoice {
		t.Helper()
		cust := seedAuditCustomer(t, db, tenantID, suffix)
		inv, err := store.Create(ctx, tenantID, domain.Invoice{
			CustomerID:    cust.ID,
			InvoiceNumber: "INV-ADD-" + suffix,
			Status:        domain.InvoiceDraft,
			PaymentStatus: domain.PaymentPending,
			Currency:      "USD",
			BillingReason: domain.BillingReasonManual,
		})
		if err != nil {
			t.Fatalf("seed draft invoice: %v", err)
		}
		return inv
	}

	t.Run("add commits the line and one update row", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Invoice AddLine OK")
		inv := seedDraft(t, tenantID, "ok")
		svc := newAuditedInvoiceService(t, db, logger, "INV-UNUSED")

		item, err := svc.AddLineItem(ctx, tenantID, inv.ID, invoice.AddLineItemInput{
			Description:     "Onboarding",
			Quantity:        3,
			UnitAmountCents: 2000,
		})
		if err != nil {
			t.Fatalf("AddLineItem: %v", err)
		}
		if item.AmountCents != 6000 {
			t.Fatalf("line amount: got %d, want 6000", item.AmountCents)
		}

		after, err := store.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get invoice: %v", err)
		}
		if after.SubtotalCents != 6000 {
			t.Fatalf("invoice subtotal after add: got %d, want 6000", after.SubtotalCents)
		}

		rows := invoiceAuditRows(t, logger, ctx, tenantID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for the line-item add; got %d: %+v", len(rows), rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionUpdate {
			t.Errorf("action: got %q, want %q — the frozen vocabulary carries the add as an invoice update", row.Action, domain.AuditActionUpdate)
		}
		if row.ResourceType != "invoice" || row.ResourceID != inv.ID {
			t.Errorf("resource: got (%q,%q), want (invoice,%q)", row.ResourceType, row.ResourceID, inv.ID)
		}
		if row.ResourceLabel != inv.InvoiceNumber {
			t.Errorf("resource_label: got %q, want the invoice number %q", row.ResourceLabel, inv.InvoiceNumber)
		}
		if row.Metadata["action"] != "line_item_added" {
			t.Errorf("metadata action discriminator: got %v, want line_item_added", row.Metadata["action"])
		}
		if row.Metadata["description"] != "Onboarding" {
			t.Errorf("metadata description: got %v, want Onboarding", row.Metadata["description"])
		}
		wantNum(t, row.Metadata, "amount_cents", 6000)
		wantNum(t, row.Metadata, "quantity", 3)
	})

	t.Run("emit failure rolls the line and the totals rewrite back", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Invoice AddLine Fail")
		inv := seedDraft(t, tenantID, "fail")
		emitter := &failingEmitter{}
		svc := newAuditedInvoiceService(t, db, emitter, "INV-UNUSED")

		_, err := svc.AddLineItem(ctx, tenantID, inv.ID, invoice.AddLineItemInput{
			Description:     "Onboarding",
			Quantity:        3,
			UnitAmountCents: 2000,
		})
		if !errors.Is(err, errEmitBoom) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if emitter.calls != 1 {
			t.Fatalf("emitter ran %d times, want exactly 1", emitter.calls)
		}

		items, err := store.ListLineItems(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("list line items: %v", err)
		}
		if len(items) != 0 {
			t.Errorf("line item survived a failed emission: %+v", items)
		}
		after, err := store.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get invoice: %v", err)
		}
		if after.SubtotalCents != 0 {
			t.Errorf("invoice subtotal: got %d, want 0 — the totals rewrite must roll back too", after.SubtotalCents)
		}
		if rows := invoiceAuditRows(t, logger, ctx, tenantID); len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back line-item add: %+v", rows)
		}
	})

	t.Run("rejected add (non-draft invoice) emits nothing", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Invoice AddLine Rejected")
		inv := seedDraft(t, tenantID, "rejected")
		if _, err := store.UpdateStatus(ctx, tenantID, inv.ID, domain.InvoiceFinalized); err != nil {
			t.Fatalf("finalize seed invoice: %v", err)
		}
		svc := newAuditedInvoiceService(t, db, logger, "INV-UNUSED")

		// The store refuses the add under the row lock — no row changed, so the
		// emission must never run. A row here would be fabricated evidence of a
		// mutation that did not happen.
		if _, err := svc.AddLineItem(ctx, tenantID, inv.ID, invoice.AddLineItemInput{
			Description:     "Too late",
			Quantity:        1,
			UnitAmountCents: 100,
		}); err == nil {
			t.Fatal("adding a line to a finalized invoice must fail")
		}

		if rows := invoiceAuditRows(t, logger, ctx, tenantID); len(rows) != 0 {
			t.Errorf("audit row written for a REJECTED line-item add: %+v", rows)
		}
	})
}
