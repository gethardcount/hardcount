package customer

import (
	"context"
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

var stripeSyncProfile = domain.CustomerBillingProfile{
	LegalName:       "Acme GmbH",
	Country:         "DE",
	TaxStatus:       tax.StatusExempt,
	TaxExemptReason: "charity",
	TaxIDType:       "eu_vat",
	TaxID:           "DE123456789",
}

// A customer edit pushes to Stripe only the one field it can change there,
// the email, and only when the email changed. The billing profile is pushed
// when it is saved, never from a customer edit: the full sync resets
// tax_exempt and removes every tax ID that does not match the profile.
func TestUpdate_StripeSync(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		email      string // the customer's email at create
		hasProfile bool
		unlinked   bool
		input      UpdateInput
		want       []syncCall
	}{
		{
			name:  "email changed, no profile: the email alone",
			email: "old@acme.test",
			input: UpdateInput{Email: "new@acme.test"},
			want:  []syncCall{{method: "SyncCustomer", stripeCustomerID: "cus_stripe", email: "new@acme.test"}},
		},
		{
			name:       "email changed, with a profile: the email alone",
			email:      "old@acme.test",
			hasProfile: true,
			input:      UpdateInput{Email: "new@acme.test"},
			want:       []syncCall{{method: "SyncCustomer", stripeCustomerID: "cus_stripe", email: "new@acme.test"}},
		},
		{
			name:       "renamed, email unchanged: no Stripe call",
			email:      "old@acme.test",
			hasProfile: true,
			input:      UpdateInput{DisplayName: "Acme Two"},
		},
		{
			name:  "no email, no profile, renamed: no Stripe call",
			input: UpdateInput{DisplayName: "Acme Two"},
		},
		{
			name:     "no Stripe customer linked: no Stripe call",
			email:    "old@acme.test",
			unlinked: true,
			input:    UpdateInput{Email: "new@acme.test"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := newMemoryStore()
			svc := NewService(mem)
			cust, err := svc.Create(ctx, "t1", CreateInput{ExternalID: "ext_1", DisplayName: "Acme", Email: tc.email})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if !tc.unlinked {
				if err := mem.SetStripeCustomerID(ctx, "t1", cust.ID, "cus_stripe"); err != nil {
					t.Fatalf("link Stripe customer: %v", err)
				}
			}
			if tc.hasProfile {
				bp := stripeSyncProfile
				bp.CustomerID = cust.ID
				if _, err := svc.UpsertBillingProfile(ctx, "t1", bp); err != nil {
					t.Fatalf("save profile: %v", err)
				}
			}
			syncer := &recordingSyncer{}
			svc.SetStripeSyncer(syncer)

			if _, err := svc.Update(ctx, "t1", cust.ID, tc.input); err != nil {
				t.Fatalf("update: %v", err)
			}
			if !reflect.DeepEqual(syncer.calls, tc.want) {
				t.Errorf("Stripe sync calls =\n%+v\nwant\n%+v", syncer.calls, tc.want)
			}
		})
	}
}

// A profile save pushes the saved profile, with the customer's email, to the
// linked Stripe customer, once.
func TestUpsertBillingProfile_StripeSync(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		unlinked bool
		wantSync bool
	}{
		{name: "linked: one full sync with the saved profile", wantSync: true},
		{name: "no Stripe customer linked: no Stripe call", unlinked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := newMemoryStore()
			svc := NewService(mem)
			cust, err := svc.Create(ctx, "t1", CreateInput{ExternalID: "ext_1", DisplayName: "Acme", Email: "billing@acme.test"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if !tc.unlinked {
				if err := mem.SetStripeCustomerID(ctx, "t1", cust.ID, "cus_stripe"); err != nil {
					t.Fatalf("link Stripe customer: %v", err)
				}
			}
			syncer := &recordingSyncer{}
			svc.SetStripeSyncer(syncer)

			bp := stripeSyncProfile
			bp.CustomerID = cust.ID
			saved, err := svc.UpsertBillingProfile(ctx, "t1", bp)
			if err != nil {
				t.Fatalf("save profile: %v", err)
			}

			var want []syncCall
			if tc.wantSync {
				want = []syncCall{{method: "SyncBillingProfile", stripeCustomerID: "cus_stripe", email: "billing@acme.test", profile: saved}}
			}
			if !reflect.DeepEqual(syncer.calls, want) {
				t.Errorf("Stripe sync calls =\n%+v\nwant\n%+v", syncer.calls, want)
			}
		})
	}
}
