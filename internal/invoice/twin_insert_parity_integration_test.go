package invoice_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCreateInvoice_TxAndNonTxPersistIdenticalColumns is the twin-drift guard
// for the two hand-maintained INSERT statements behind
// CreateInvoiceWithLineItems and CreateInvoiceWithLineItemsTx. They must
// persist the SAME field set: the 2026-07-13 manual-test pass found the Tx
// twin silently dropping billing_timezone (its column list was never updated
// when the ADR-077 stamp shipped), which broke "an issued invoice keeps its
// dates" for every coordinator-tx invoice — day-1 start-now create, atomic
// cancel-final, swaps, proration. Rather than pin one field, this inserts an
// identical fully-populated invoice through BOTH paths and diffs every column
// of the two rows (ignoring identity/uniqueness columns), so the NEXT field
// added to one twin but not the other fails here instead of silently
// vanishing in production.
func TestCreateInvoice_TxAndNonTxPersistIdenticalColumns(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Twin Parity Corp")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_twin_parity", DisplayName: "Twin Parity",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	store := invoice.NewPostgresStore(db)
	ps := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	pe := time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)
	issued := ps
	due := ps.AddDate(0, 0, 30)
	proto := func(num string) domain.Invoice {
		return domain.Invoice{
			CustomerID:    cust.ID,
			InvoiceNumber: num,
			Status:        domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentPending,
			Currency:      "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: ps, BillingPeriodEnd: pe,
			IssuedAt: &issued, DueAt: &due,
			NetPaymentTermDays: 30,
			Memo:               "twin parity",
			TaxFacts:           domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
			BillingReason:      domain.BillingReasonSubscriptionCreate,
			BillingTimezone:    "Asia/Kolkata",
			IsSimulated:        false,
		}
	}
	line := func() []domain.InvoiceLineItem {
		return []domain.InvoiceLineItem{{
			Description: "base", LineType: domain.LineTypeBaseFee,
			Quantity: 1, UnitAmountCents: 5000, AmountCents: 5000, Currency: "USD",
		}}
	}

	nonTx, err := store.CreateWithLineItems(ctx, tenantID, proto("TWIN-NONTX-1"), line())
	if err != nil {
		t.Fatalf("non-tx create: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	viaTx, err := store.CreateWithLineItemsTx(ctx, tx, tenantID, proto("TWIN-TX-1"), line())
	if err != nil {
		t.Fatalf("tx create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Diff every real column of the two rows via row_to_json, ignoring
	// identity/uniqueness/token columns that legitimately differ.
	ignore := map[string]bool{
		"id": true, "invoice_number": true, "created_at": true, "updated_at": true,
		"public_token_encrypted": true, "public_token_hash": true,
	}
	rowJSON := func(id string) map[string]any {
		t.Helper()
		var raw []byte
		q, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin read: %v", err)
		}
		defer postgres.Rollback(q)
		if err := q.QueryRowContext(ctx, `SELECT row_to_json(i) FROM invoices i WHERE id=$1`, id).Scan(&raw); err != nil {
			t.Fatalf("row_to_json(%s): %v", id, err)
		}
		var m map[string]any
		if err := jsonUnmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}
	a, b := rowJSON(nonTx.ID), rowJSON(viaTx.ID)
	for col, av := range a {
		if ignore[col] {
			continue
		}
		bv, ok := b[col]
		if !ok {
			t.Errorf("column %q present via non-tx path but missing via tx path", col)
			continue
		}
		if toComparable(av) != toComparable(bv) {
			t.Errorf("twin drift on column %q: non-tx=%v tx=%v", col, av, bv)
		}
	}
	// The field that started all this gets a direct pin too.
	if b["billing_timezone"] != "Asia/Kolkata" {
		t.Errorf("tx path billing_timezone = %v, want Asia/Kolkata (ADR-077 stamp)", b["billing_timezone"])
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func toComparable(v any) string { return fmt.Sprint(v) }
