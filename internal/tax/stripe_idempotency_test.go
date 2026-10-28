package tax

import (
	"bytes"
	"context"
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// capturingBackend implements stripe.Backend and records the params + path of
// the single outbound Call so the test can assert the exact request SHAPE the
// SDK would put on the wire — without any network. It also sets a fake
// transaction id on the response so Reverse/Commit return cleanly.
type capturingBackend struct {
	method string
	path   string
	params stripe.ParamsContainer
}

func (b *capturingBackend) Call(method, path, _ string, params stripe.ParamsContainer, v stripe.LastResponseSetter) error {
	b.method, b.path, b.params = method, path, params
	if txn, ok := v.(*stripe.TaxTransaction); ok {
		txn.ID = "tax_txn_fake"
	}
	return nil
}

func (b *capturingBackend) CallStreaming(string, string, string, stripe.ParamsContainer, stripe.StreamingLastResponseSetter) error {
	return nil
}

func (b *capturingBackend) CallRaw(string, string, string, []byte, *stripe.Params, stripe.LastResponseSetter) error {
	return nil
}

func (b *capturingBackend) CallMultipart(string, string, string, string, *bytes.Buffer, *stripe.Params, stripe.LastResponseSetter) error {
	return nil
}

func (b *capturingBackend) SetMaxNetworkRetries(int64) {}

// staticClientResolver hands the same *stripe.Client to every ctx — enough for
// these unit tests, which only exercise param building, not per-tenant routing.
type staticClientResolver struct{ c *stripe.Client }

func (r staticClientResolver) ResolveCtx(context.Context) (*stripe.Client, error) { return r.c, nil }

// newCapturingProvider wires a StripeTaxProvider whose Stripe client routes
// every API call into the returned capturingBackend.
func newCapturingProvider() (*StripeTaxProvider, *capturingBackend) {
	be := &capturingBackend{}
	client := stripe.NewClient("sk_test_fake", stripe.WithBackends(&stripe.Backends{
		API:         be,
		Connect:     be,
		Uploads:     be,
		MeterEvents: be,
	}))
	return NewStripeTaxProvider(staticClientResolver{c: client}), be
}

// TestReverse_RequestShape pins the money-correctness ACI property for the
// credit-note tax-reversal path: a retried reversal must NOT create a second
// (double) reversal. That guarantee rests entirely on two request fields, both
// derived from the reversal Reference:
//
//   - Reference            = ref            (Stripe enforces account-wide uniqueness)
//   - IdempotencyKey header = "hardcount_tax_rev_" + ref
//
// plus, for partial reversals, a NEGATIVE FlatAmount (Stripe requires the
// reversal amount in negative smallest-currency units). If a refactor silently
// drops the idempotency key, changes its prefix, sends the wrong reference, or
// flips the partial amount sign, this test fails — closing the G1-class
// regression window where double tax-reversal could ship with CI green.
func TestReverse_RequestShape(t *testing.T) {
	t.Run("full reversal", func(t *testing.T) {
		p, be := newCapturingProvider()
		res, err := p.Reverse(context.Background(), ReversalRequest{
			OriginalTransactionID: "tax_txn_orig",
			Reference:             "cn_abc123",
			Mode:                  ReversalModeFull,
		})
		if err != nil {
			t.Fatalf("Reverse: unexpected error: %v", err)
		}
		if res == nil || res.TransactionID != "tax_txn_fake" {
			t.Fatalf("ReversalResult = %+v, want TransactionID=tax_txn_fake", res)
		}
		if be.path != "/v1/tax/transactions/create_reversal" {
			t.Errorf("path = %q, want /v1/tax/transactions/create_reversal", be.path)
		}
		params, ok := be.params.(*stripe.TaxTransactionCreateReversalParams)
		if !ok {
			t.Fatalf("captured params type = %T, want *stripe.TaxTransactionCreateReversalParams", be.params)
		}
		if got := stripe.StringValue(params.Reference); got != "cn_abc123" {
			t.Errorf("Reference = %q, want cn_abc123 (Stripe-side dedup key)", got)
		}
		if got := idemKey(params); got != "hardcount_tax_rev_cn_abc123" {
			t.Errorf("IdempotencyKey = %q, want hardcount_tax_rev_cn_abc123 (retry-dedup; prefix+ref must not drift)", got)
		}
		if got := stripe.StringValue(params.Mode); got != string(ReversalModeFull) {
			t.Errorf("Mode = %q, want full", got)
		}
		// Full reversals must NOT pin an amount — Stripe reverses the whole
		// original transaction. A stray FlatAmount would under/over-reverse tax.
		if params.FlatAmount != nil {
			t.Errorf("FlatAmount = %d, want nil for full reversal", *params.FlatAmount)
		}
	})

	t.Run("partial reversal sends negative FlatAmount", func(t *testing.T) {
		p, be := newCapturingProvider()
		_, err := p.Reverse(context.Background(), ReversalRequest{
			OriginalTransactionID: "tax_txn_orig",
			Reference:             "cn_partial",
			Mode:                  ReversalModePartial,
			GrossAmountCents:      4499,
		})
		if err != nil {
			t.Fatalf("Reverse(partial): unexpected error: %v", err)
		}
		params, ok := be.params.(*stripe.TaxTransactionCreateReversalParams)
		if !ok {
			t.Fatalf("captured params type = %T, want *stripe.TaxTransactionCreateReversalParams", be.params)
		}
		if got := stripe.StringValue(params.Mode); got != string(ReversalModePartial) {
			t.Errorf("Mode = %q, want partial", got)
		}
		if params.FlatAmount == nil {
			t.Fatalf("FlatAmount = nil, want -4499 (partial reversal must send a negative amount)")
		}
		if *params.FlatAmount != -4499 {
			t.Errorf("FlatAmount = %d, want -4499 (Stripe requires negative smallest-currency units; sign must not flip)", *params.FlatAmount)
		}
		if got := idemKey(params); got != "hardcount_tax_rev_cn_partial" {
			t.Errorf("IdempotencyKey = %q, want hardcount_tax_rev_cn_partial", got)
		}
	})

	t.Run("reference falls back to credit note id", func(t *testing.T) {
		p, be := newCapturingProvider()
		_, err := p.Reverse(context.Background(), ReversalRequest{
			OriginalTransactionID: "tax_txn_orig",
			CreditNoteID:          "cn_fallback",
			// Reference intentionally empty — must fall back to CreditNoteID
			// for BOTH the Reference field and the idempotency key, so the
			// legacy credit-note caller still dedups.
		})
		if err != nil {
			t.Fatalf("Reverse(fallback): unexpected error: %v", err)
		}
		params := be.params.(*stripe.TaxTransactionCreateReversalParams)
		if got := stripe.StringValue(params.Reference); got != "cn_fallback" {
			t.Errorf("Reference = %q, want cn_fallback (fallback to CreditNoteID)", got)
		}
		if got := idemKey(params); got != "hardcount_tax_rev_cn_fallback" {
			t.Errorf("IdempotencyKey = %q, want hardcount_tax_rev_cn_fallback (key must use the same ref)", got)
		}
	})
}

// TestCommit_RequestShape pins the money-correctness ACI property for the
// tax-commit path: a retried finalize/commit must NOT create a second (double)
// tax_transaction. That rests on:
//
//   - Reference            = invoiceID                (Stripe-side uniqueness)
//   - IdempotencyKey header = "hardcount_tax_commit_" + invoiceID
//
// On a within-window retry Stripe returns the cached original transaction id
// (lets the reconciler recover a commit whose local persist failed). If a
// refactor drops the key or sends the wrong reference, a duplicate
// tax_transaction could be created — double tax committed upstream with CI
// green. This test fails before that ships.
func TestCommit_RequestShape(t *testing.T) {
	p, be := newCapturingProvider()
	id, err := p.Commit(context.Background(), "taxcalc_ref", "hc_inv_123")
	if err != nil {
		t.Fatalf("Commit: unexpected error: %v", err)
	}
	if id != "tax_txn_fake" {
		t.Errorf("Commit returned id = %q, want tax_txn_fake", id)
	}
	if be.path != "/v1/tax/transactions/create_from_calculation" {
		t.Errorf("path = %q, want /v1/tax/transactions/create_from_calculation", be.path)
	}
	params, ok := be.params.(*stripe.TaxTransactionCreateFromCalculationParams)
	if !ok {
		t.Fatalf("captured params type = %T, want *stripe.TaxTransactionCreateFromCalculationParams", be.params)
	}
	if got := stripe.StringValue(params.Calculation); got != "taxcalc_ref" {
		t.Errorf("Calculation = %q, want taxcalc_ref", got)
	}
	if got := stripe.StringValue(params.Reference); got != "hc_inv_123" {
		t.Errorf("Reference = %q, want hc_inv_123 (invoice-keyed Stripe dedup)", got)
	}
	if got := idemKey(params); got != "hardcount_tax_commit_hc_inv_123" {
		t.Errorf("IdempotencyKey = %q, want hardcount_tax_commit_hc_inv_123 (prefix+invoiceID must not drift)", got)
	}
}

// idemKey reads the request-level Idempotency-Key off any Stripe params struct
// (it lives on the embedded Params, sent as an HTTP header — form:"-").
func idemKey(p stripe.ParamsContainer) string {
	return stripe.StringValue(p.GetParams().IdempotencyKey)
}

var _ stripe.Backend = (*capturingBackend)(nil)
