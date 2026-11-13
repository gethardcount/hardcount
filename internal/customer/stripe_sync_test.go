package customer

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tax"
)

// recordingSyncer records each Stripe sync the service asks for.
type recordingSyncer struct {
	calls []syncCall
}

type syncCall struct {
	method           string
	stripeCustomerID string
	email            string
	profile          domain.CustomerBillingProfile
}

func (r *recordingSyncer) SyncCustomer(_ context.Context, stripeCustomerID, email string) error {
	r.calls = append(r.calls, syncCall{method: "SyncCustomer", stripeCustomerID: stripeCustomerID, email: email})
	return nil
}

func (r *recordingSyncer) SyncBillingProfile(_ context.Context, stripeCustomerID, email string, bp domain.CustomerBillingProfile) error {
	r.calls = append(r.calls, syncCall{method: "SyncBillingProfile", stripeCustomerID: stripeCustomerID, email: email, profile: bp})
	return nil
}

// profileReadFails is the memory store with a billing-profile read that
// fails, as a database error would.
type profileReadFails struct {
	*memoryStore
}

var errProfileRead = errors.New("connection reset")

func (profileReadFails) GetBillingProfile(context.Context, string, string) (domain.CustomerBillingProfile, error) {
	return domain.CustomerBillingProfile{}, errProfileRead
}

// A customer edit pushes to Stripe only what Hardcount holds and has just
// read. The full sync resets tax_exempt and removes every tax ID that does
// not match the profile, so it must never run on a profile that is missing
// or could not be read.
func TestUpdate_StripeSync(t *testing.T) {
	ctx := context.Background()
	profile := domain.CustomerBillingProfile{
		LegalName:       "Acme GmbH",
		Country:         "DE",
		TaxStatus:       tax.StatusExempt,
		TaxExemptReason: "charity",
		TaxIDType:       "eu_vat",
		TaxID:           "DE123456789",
	}

	for _, tc := range []struct {
		name       string
		hasProfile bool
		readFails  bool
		want       []syncCall
	}{
		{
			name:       "profile read: the full sync carries it",
			hasProfile: true,
			want:       []syncCall{{method: "SyncBillingProfile", stripeCustomerID: "cus_stripe", email: "new@acme.test", profile: profile}},
		},
		{
			name: "no profile: only the customer's own fields",
			want: []syncCall{{method: "SyncCustomer", stripeCustomerID: "cus_stripe", email: "new@acme.test"}},
		},
		{
			name:       "profile read fails: no Stripe call",
			hasProfile: true,
			readFails:  true,
			want:       nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := newMemoryStore()
			var store Store = mem
			if tc.readFails {
				store = profileReadFails{mem}
			}
			svc := NewService(store)
			cust, err := svc.Create(ctx, "t1", CreateInput{ExternalID: "ext_1", DisplayName: "Acme", Email: "old@acme.test"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := mem.SetStripeCustomerID(ctx, "t1", cust.ID, "cus_stripe"); err != nil {
				t.Fatalf("link Stripe customer: %v", err)
			}
			if tc.hasProfile {
				bp := profile
				bp.CustomerID = cust.ID
				if _, err := svc.UpsertBillingProfile(ctx, "t1", bp); err != nil {
					t.Fatalf("save profile: %v", err)
				}
			}
			syncer := &recordingSyncer{}
			svc.SetStripeSyncer(syncer)

			updated, err := svc.Update(ctx, "t1", cust.ID, UpdateInput{Email: "new@acme.test"})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if updated.Email != "new@acme.test" {
				t.Errorf("email = %q, want the update saved", updated.Email)
			}
			for i := range syncer.calls {
				// Compare what the profile says, not the bookkeeping the
				// store adds.
				p := &syncer.calls[i].profile
				p.CustomerID, p.TenantID, p.ProfileStatus = "", "", ""
			}
			if !reflect.DeepEqual(syncer.calls, tc.want) {
				t.Errorf("Stripe sync calls =\n%+v\nwant\n%+v", syncer.calls, tc.want)
			}
		})
	}
}
