package invoice

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The same list the billing engine's test pins for its own invoice.finalized
// sender: an operator-finalized draft and an engine-born invoice send the same
// fields.
var invoiceFinalizedKeys = []string{
	"amount_due_cents",
	"credits_applied_cents",
	"currency",
	"customer_id",
	"invoice_id",
	"invoice_number",
	"payment_status",
	"status",
	"total_amount_cents",
}

// Finalizing a draft sends invoice.finalized with credits_applied_cents read
// from the finalized row (#1281). The value is normally 0, because credit is
// applied when the invoice is collected, after this event. A draft built
// before credit moved to collection (ADR-116) can already carry credit, and
// the payload must then say so, or amount_due_cents and total_amount_cents
// disagree with no field to explain the gap.
func TestFinalize_InvoiceFinalizedPayloadCarriesCreditsApplied(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	events := &captureEvents{}
	svc.SetEventDispatcher(events)
	ctx := context.Background()

	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems:  []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	draft := store.invoices[inv.ID]
	draft.CreditsAppliedCents = 2500
	draft.AmountDueCents = draft.TotalAmountCents - 2500
	store.invoices[inv.ID] = draft

	final, err := svc.Finalize(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}

	var payload map[string]any
	for _, ev := range events.events {
		if ev.eventType == domain.EventInvoiceFinalized {
			payload = ev.payload
		}
	}
	if payload == nil {
		t.Fatalf("no invoice.finalized event; got %+v", events.events)
	}
	if got := slices.Sorted(maps.Keys(payload)); !slices.Equal(got, invoiceFinalizedKeys) {
		t.Errorf("payload keys:\n got %v\nwant %v", got, invoiceFinalizedKeys)
	}
	if got := payload["credits_applied_cents"]; got != int64(2500) {
		t.Errorf("credits_applied_cents: got %#v, want int64(2500)", got)
	}
	if got := payload["amount_due_cents"]; got != final.AmountDueCents {
		t.Errorf("amount_due_cents: got %#v, want %d (the finalized row)", got, final.AmountDueCents)
	}
	if got := payload["total_amount_cents"]; got != final.TotalAmountCents {
		t.Errorf("total_amount_cents: got %#v, want %d", got, final.TotalAmountCents)
	}
}
