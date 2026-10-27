package tax

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/stripe/stripe-go/v82"
)

type nilClients struct{}

func (nilClients) ResolveCtx(context.Context) (*stripe.Client, error) { return nil, nil }

// A calculation exists, so a missing client at commit means the credentials
// are gone: nothing was committed. ("", nil) read as success and
// the commit-retry sweep counted it as recovered.
func TestCommit_NoClientIsAnError(t *testing.T) {
	p := NewStripeTaxProvider(nilClients{})
	if _, err := p.Commit(postgres.WithLivemode(context.Background(), true), "taxcalc_1", "inv_1"); err == nil {
		t.Fatal("Commit with no client = nil error, want an error")
	}
}

// errClients fails every credential read.
type errClients struct{ err error }

func (c errClients) ResolveCtx(context.Context) (*stripe.Client, error) { return nil, c.err }

// Stripe credentials that cannot be read are an error at every entry point,
// never read as "no Stripe connected for this mode".
func TestStripeTax_UnreadableCredentialsAreAnError(t *testing.T) {
	boom := errors.New("boom")
	p := NewStripeTaxProvider(errClients{err: boom})
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, err := p.Calculate(ctx, Request{Currency: "USD", LineItems: []RequestLine{{}}}); !errors.Is(err, boom) {
		t.Errorf("Calculate: err = %v, want it to wrap the read error", err)
	}
	if _, err := p.Commit(ctx, "taxcalc_1", "inv_1"); !errors.Is(err, boom) {
		t.Errorf("Commit: err = %v, want it to wrap the read error", err)
	}
	if _, err := p.Reverse(ctx, ReversalRequest{OriginalTransactionID: "tax_txn_1", CreditNoteID: "cn_1"}); !errors.Is(err, boom) {
		t.Errorf("Reverse: err = %v, want it to wrap the read error", err)
	}
}
