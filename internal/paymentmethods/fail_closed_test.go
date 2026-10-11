package paymentmethods

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type blipLink struct {
	CustomerStripeLink
	sets int
}

var errBlip = errors.New("connection reset")

func (b *blipLink) Get(context.Context, string, string) (domain.Customer, error) {
	return domain.Customer{}, errBlip
}
func (b *blipLink) SetStripeCustomerID(context.Context, string, string, string) error {
	b.sets++
	return nil
}

// A customer read error is not "no Stripe customer yet": creating and
// recording a new one re-mapped the customer and stranded their saved card.
func TestEnsureStripeCustomer_ReadErrorCreatesNothing(t *testing.T) {
	link := &blipLink{}
	a := NewStripeAdapter(nil, link)
	// The read error itself must surface (the old code dropped it and went
	// on to create a Stripe customer; here that failed only for lack of a
	// client).
	if _, err := a.EnsureStripeCustomer(context.Background(), "t1", "cus_1"); !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the customer read error", err)
	}
	if link.sets != 0 {
		t.Fatalf("SetStripeCustomerID called %d time(s) after a failed read", link.sets)
	}
}
