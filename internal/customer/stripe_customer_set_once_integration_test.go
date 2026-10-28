package customer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A customer's Stripe customer id is set once (#976, ADR-127). The stale-hold
// sweep asks Stripe about a lost charge through the customer the charge went
// to; an empty answer means "nothing was charged" only while that customer is
// the one on file. Writing the same id again is fine; writing a different one
// over it is refused and leaves the row unchanged. An unknown customer is
// still not-found.
func TestSetStripeCustomerID_SetOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Stripe Customer Set Once")
	store := customer.NewPostgresStore(db)
	c, err := store.Create(ctx, tenantID, domain.Customer{ExternalID: "ext_set_once", DisplayName: "Set Once"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	if err := store.SetStripeCustomerID(ctx, tenantID, c.ID, "cus_first"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := store.SetStripeCustomerID(ctx, tenantID, c.ID, "cus_first"); err != nil {
		t.Fatalf("writing the same id again: %v, want a no-op", err)
	}
	err = store.SetStripeCustomerID(ctx, tenantID, c.ID, "cus_second")
	if err == nil {
		t.Fatal("overwriting a different Stripe customer id succeeded: a lost charge's customer could change under the stale-hold sweep")
	}
	if errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("overwrite refused as not-found (%v): the customer exists, the refusal must say why", err)
	}
	got, err := store.Get(ctx, tenantID, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.StripeCustomerID != "cus_first" {
		t.Fatalf("stripe_customer_id = %q after a refused overwrite, want cus_first", got.StripeCustomerID)
	}

	if err := store.SetStripeCustomerID(ctx, tenantID, "cus_does_not_exist", "cus_x"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown customer: err = %v, want ErrNotFound", err)
	}
}
