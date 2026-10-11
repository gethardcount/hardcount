package tax

import (
	"context"
	"testing"

	"github.com/stripe/stripe-go/v82"
)

type nilClients struct{}

func (nilClients) ForCtx(context.Context) *stripe.Client { return nil }

// A calculation exists, so a missing client at commit means the credentials
// are gone or unreadable: nothing was committed. ("", nil) read as success and
// the commit-retry sweep counted it as recovered.
func TestCommit_NoClientIsAnError(t *testing.T) {
	p := NewStripeTaxProvider(nilClients{})
	if _, err := p.Commit(context.Background(), "taxcalc_1", "inv_1"); err == nil {
		t.Fatal("Commit with no client = nil error, want an error")
	}
}
