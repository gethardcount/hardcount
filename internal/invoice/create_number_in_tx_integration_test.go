package invoice_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestCreate_FailedCreateUsesNoNumber: a manual invoice takes its number on
// the transaction that inserts it. A create that fails in that transaction
// gives the number back, so the next invoice takes it and the tenant's
// numbers keep no gap. The failure here is the in-transaction audit row,
// which runs after the invoice and its lines are inserted.
func TestCreate_FailedCreateUsesNoNumber(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Invoice Number In Tx")
	cust := seedAuditCustomer(t, db, tenantID, "number_in_tx")
	settings := tenant.NewSettingsStore(db)
	store := invoice.NewPostgresStore(db)

	nextSeq := func() int {
		t.Helper()
		seq, err := settings.NextInvoiceSequence(ctx, tenantID)
		if err != nil {
			t.Fatalf("read next invoice sequence: %v", err)
		}
		return seq
	}
	countInvoices := func() int {
		t.Helper()
		_, total, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("list invoices: %v", err)
		}
		return total
	}

	svc := invoice.NewService(store, nil, settings)
	first, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if first.InvoiceNumber != "HC-000001" {
		t.Fatalf("first invoice number: got %q, want HC-000001", first.InvoiceNumber)
	}
	seqBefore := nextSeq()

	emitter := &failingEmitter{}
	failing := invoice.NewService(store, nil, settings)
	failing.SetAuditLogger(emitter)
	for _, tc := range []struct {
		name  string
		input invoice.CreateInput
	}{
		{"bare header", invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"}},
		{"with lines", invoice.CreateInput{CustomerID: cust.ID, Currency: "USD", LineItems: []invoice.AddLineItemInput{
			{Description: "Setup fee", Quantity: 1, UnitAmountCents: 5000},
		}}},
	} {
		if _, err := failing.Create(ctx, tenantID, tc.input); !errors.Is(err, errEmitBoom) {
			t.Fatalf("%s: want the injected audit failure; got %v", tc.name, err)
		}
	}
	if emitter.calls != 2 {
		t.Fatalf("audit emission ran %d times, want 2 (the test proves nothing otherwise)", emitter.calls)
	}
	if n := countInvoices(); n != 1 {
		t.Fatalf("tenant has %d invoices after two failed creates, want 1", n)
	}
	if got := nextSeq(); got != seqBefore {
		t.Fatalf("next invoice sequence after two failed creates: got %d, want %d (each failed create used up a number)", got, seqBefore)
	}

	next, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID, Currency: "USD"})
	if err != nil {
		t.Fatalf("create after the failures: %v", err)
	}
	if want := fmt.Sprintf("HC-%06d", seqBefore); next.InvoiceNumber != want {
		t.Fatalf("invoice after the failures: got %q, want %q, the number the failed creates would have had", next.InvoiceNumber, want)
	}
}
