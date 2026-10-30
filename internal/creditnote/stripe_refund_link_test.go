package creditnote

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

type fakeRefundReader struct {
	facts domain.StripeRefundFacts
	err   error
	reads int
}

func (f *fakeRefundReader) ReadRefund(_ context.Context, _ string) (domain.StripeRefundFacts, error) {
	f.reads++
	return f.facts, f.err
}

// TestCheckLinkedStripeRefund: the operator's link of a refund made in
// Stripe to the credit note they create (ADR-134) is checked against Stripe:
// the refund must be of this invoice's own payment, not made by Hardcount,
// moved money, in the invoice's currency, and be exactly the note's card
// refund, with nothing to the credit balance, on a note issued at once.
func TestCheckLinkedStripeRefund(t *testing.T) {
	inv := paidCardInvoice()
	inv.TenantID = "t1"
	good := domain.StripeRefundFacts{ID: "re_1", PaymentIntentID: "pi_own", AmountCents: 4000, Currency: "usd", Status: domain.StripeRefundSucceeded}
	input := CreateInput{InvoiceID: inv.ID, StripeRefundID: "re_1", RefundAmountCents: 4000, AutoIssue: true}
	cases := []struct {
		name  string
		facts func(*domain.StripeRefundFacts)
		in    func(*CreateInput)
		inv   func(*domain.Invoice)
		field string // "" = accepted
	}{
		{name: "a refund of the invoice's payment"},
		{name: "a pending refund", facts: func(f *domain.StripeRefundFacts) { f.Status = domain.StripeRefundPending }, field: "stripe_refund_id"},
		{name: "a refund needing action", facts: func(f *domain.StripeRefundFacts) { f.Status = domain.StripeRefundRequiresAction }, field: "stripe_refund_id"},
		{name: "made by Hardcount", facts: func(f *domain.StripeRefundFacts) { f.HardcountCreditNoteID = "hc_cn_x" }, field: "stripe_refund_id"},
		{name: "another payment", facts: func(f *domain.StripeRefundFacts) { f.PaymentIntentID = "pi_dup" }, field: "stripe_refund_id"},
		{name: "no payment", facts: func(f *domain.StripeRefundFacts) { f.PaymentIntentID = "" }, field: "stripe_refund_id"},
		{name: "failed", facts: func(f *domain.StripeRefundFacts) { f.Status = domain.StripeRefundFailed }, field: "stripe_refund_id"},
		{name: "canceled", facts: func(f *domain.StripeRefundFacts) { f.Status = domain.StripeRefundCanceled }, field: "stripe_refund_id"},
		{name: "another currency", facts: func(f *domain.StripeRefundFacts) { f.Currency = "eur" }, field: "stripe_refund_id"},
		{name: "another amount", in: func(i *CreateInput) { i.RefundAmountCents = 3000 }, field: "refund_amount_cents"},
		{name: "part to the credit balance", in: func(i *CreateInput) { i.CreditAmountCents = 500 }, field: "credit_amount_cents"},
		{name: "the older refund_type", in: func(i *CreateInput) { i.RefundType = "refund" }, field: "refund_type"},
		{name: "a draft", in: func(i *CreateInput) { i.AutoIssue = false }, field: "auto_issue"},
		{name: "an unpaid invoice", inv: func(i *domain.Invoice) { i.Status, i.PaymentStatus = domain.InvoiceFinalized, domain.PaymentPending }, field: "stripe_refund_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := inv
			if tc.inv != nil {
				tc.inv(&i)
			}
			f := good
			if tc.facts != nil {
				tc.facts(&f)
			}
			in := input
			if tc.in != nil {
				tc.in(&in)
			}
			reader := &fakeRefundReader{facts: f}
			s := NewService(newMemStore(), &memInvoiceReader{invoices: map[string]domain.Invoice{i.ID: i}}, nil)
			s.SetStripeRefundRecording(&PostgresStore{}, nil, nil)
			s.SetRefundReader(reader)
			link, err := s.checkLinkedStripeRefund(context.Background(), "t1", in)
			if tc.field == "" {
				if err != nil || link == nil || link.facts.ID != "re_1" {
					t.Fatalf("link = %+v, err = %v; want accepted", link, err)
				}
				return
			}
			var fe *errs.DomainError
			if !errors.As(err, &fe) || fe.Field != tc.field {
				t.Fatalf("err = %v, want a validation error on %s", err, tc.field)
			}
		})
	}
}

// TestCheckLinkedStripeRefund_NotConnected: without a Stripe read wired the
// link is refused, never made on Hardcount's word alone.
func TestCheckLinkedStripeRefund_NotConnected(t *testing.T) {
	s := NewService(newMemStore(), &memInvoiceReader{}, nil)
	if _, err := s.checkLinkedStripeRefund(context.Background(), "t1", CreateInput{InvoiceID: "inv_1", StripeRefundID: "re_1", AutoIssue: true}); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("err = %v, want invalid state", err)
	}
}
