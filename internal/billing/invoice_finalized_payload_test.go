package billing

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// invoiceFinalizedKeys is the invoice.finalized payload both senders build:
// this engine (invoices born finalized, and the subscription handler's
// proration invoice) and invoice.Service.Finalize (drafts). The invoice
// package pins the same list, so the two payloads cannot drift apart.
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

// The invoice.finalized payload carries credits_applied_cents beside
// amount_due_cents (#1281). Credit is applied when the invoice is collected,
// after this event, so amount_due_cents here is the amount before that credit;
// the payload says how much credit was on the invoice when it was sent, read
// from the invoice, so the two fields add up as they do on the invoice itself.
func TestDispatchInvoiceFinalized_PayloadCarriesCreditsApplied(t *testing.T) {
	d := &capturingEventDispatcher{}
	e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, billingTestClock())
	e.SetEventDispatcher(d)

	e.DispatchInvoiceFinalized(context.Background(), "t1", domain.Invoice{
		ID:                  "vlx_inv_1",
		InvoiceNumber:       "INV-0001",
		CustomerID:          "cus_1",
		Status:              domain.InvoiceFinalized,
		PaymentStatus:       domain.PaymentPending,
		TotalAmountCents:    1000,
		AmountDueCents:      700,
		CreditsAppliedCents: 300,
		Currency:            "USD",
	})

	if len(d.events) != 1 || d.events[0].eventType != domain.EventInvoiceFinalized {
		t.Fatalf("want one invoice.finalized event, got %+v", d.events)
	}
	p := d.events[0].payload
	if got := slices.Sorted(maps.Keys(p)); !slices.Equal(got, invoiceFinalizedKeys) {
		t.Errorf("payload keys:\n got %v\nwant %v", got, invoiceFinalizedKeys)
	}
	if got := p["credits_applied_cents"]; got != int64(300) {
		t.Errorf("credits_applied_cents: got %#v, want int64(300) (the invoice's own value)", got)
	}
	if got := p["amount_due_cents"]; got != int64(700) {
		t.Errorf("amount_due_cents: got %#v, want int64(700)", got)
	}
	if got := p["total_amount_cents"]; got != int64(1000) {
		t.Errorf("total_amount_cents: got %#v, want int64(1000)", got)
	}
}

// The field is sent when it is zero too, which is the usual value: an engine
// invoice is sent before its first collection applies any credit. A receiver
// can then rely on the key being there.
func TestDispatchInvoiceFinalized_ZeroCreditsAppliedIsSent(t *testing.T) {
	d := &capturingEventDispatcher{}
	e := NewEngine(nil, nil, nil, nil, nil, nil, nil, nil, billingTestClock())
	e.SetEventDispatcher(d)

	e.DispatchInvoiceFinalized(context.Background(), "t1", domain.Invoice{
		ID: "vlx_inv_2", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		TotalAmountCents: 500, AmountDueCents: 500, Currency: "USD",
	})

	if len(d.events) != 1 {
		t.Fatalf("want one event, got %d", len(d.events))
	}
	v, ok := d.events[0].payload["credits_applied_cents"]
	if !ok {
		t.Fatal("credits_applied_cents missing from the payload when it is zero")
	}
	if v != int64(0) {
		t.Errorf("credits_applied_cents: got %#v, want int64(0)", v)
	}
}
