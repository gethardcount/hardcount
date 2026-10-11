package tax

import (
	"context"
	"testing"
)

// A scoped calculation carries an idempotency key derived from the scope and
// the exact request: a retried billing step sending the same lines reuses
// Stripe's first calculation instead of paying for another; any change to the
// request (new usage, another scope) is a fresh calculation; an unscoped
// calculation (a tax retry, which must mint a new one) gets no key.
func TestCalculate_ScopedIdempotencyKey(t *testing.T) {
	keyFor := func(ctx context.Context, amount int64) string {
		p, be := newCapturingProvider()
		req := Request{Currency: "usd", CustomerAddress: Address{Country: "US", State: "WA", PostalCode: "98101"},
			LineItems: []RequestLine{{Ref: "line_0", AmountCents: amount, Quantity: 1}}}
		_, _ = p.Calculate(ctx, req) // the fake backend returns no lines; only the request matters here
		if be.params == nil {
			t.Fatal("no calculation request was sent")
		}
		return idemKey(be.params)
	}
	scoped := WithCalculationScope(context.Background(), "cycle:sub_1:2026-06-01")

	first, retry := keyFor(scoped, 5000), keyFor(scoped, 5000)
	if first == "" || first != retry {
		t.Fatalf("retry key %q, first %q: want the same non-empty key", retry, first)
	}
	if changed := keyFor(scoped, 5100); changed == first {
		t.Error("a changed request reused the key; it must get a fresh calculation")
	}
	if other := keyFor(WithCalculationScope(context.Background(), "cycle:sub_2:2026-06-01"), 5000); other == first {
		t.Error("another subscription's identical request reused the key")
	}
	if none := keyFor(context.Background(), 5000); none != "" {
		t.Errorf("unscoped calculation key = %q, want none", none)
	}
}
