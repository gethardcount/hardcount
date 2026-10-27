package payment

import (
	"context"
	"fmt"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// StripeBillingSync syncs billing profile data to Stripe customer objects.
// Mode-aware: uses the live or test client based on ctx livemode.
type StripeBillingSync struct {
	clients *StripeClients
}

// NewStripeBillingSync returns nil if clients has no configured modes. The
// caller then skips sync entirely rather than silently no-opping each call.
func NewStripeBillingSync(clients *StripeClients) *StripeBillingSync {
	if !clients.Has() {
		return nil
	}
	return &StripeBillingSync{clients: clients}
}

// SyncBillingProfile pushes the canonical billing-profile fields to the
// Stripe customer. customerEmail is the canonical recipient
// (customers.email) — the billing-profile email column was removed in
// migration 0100, so the email is now plumbed through as a separate
// argument by the caller rather than read off bp.
func (s *StripeBillingSync) SyncBillingProfile(ctx context.Context, stripeCustomerID, customerEmail string, bp domain.CustomerBillingProfile) error {
	sc, err := s.clients.ResolveCtx(ctx)
	if err != nil {
		return fmt.Errorf("stripe billing sync: read Stripe credentials: %w", err)
	}
	if sc == nil {
		return ErrStripeNotConfigured
	}

	params := &stripe.CustomerUpdateParams{}

	if bp.LegalName != "" {
		params.Name = stripe.String(bp.LegalName)
	}
	if customerEmail != "" {
		params.Email = stripe.String(customerEmail)
	}
	if bp.Phone != "" {
		params.Phone = stripe.String(bp.Phone)
	}

	// Address
	if bp.AddressLine1 != "" || bp.Country != "" {
		params.Address = &stripe.AddressParams{
			Line1:      stripe.String(bp.AddressLine1),
			Line2:      stripe.String(bp.AddressLine2),
			City:       stripe.String(bp.City),
			State:      stripe.String(bp.State),
			PostalCode: stripe.String(bp.PostalCode),
			Country:    stripe.String(bp.Country),
		}
	}

	// Tax exempt status — Stripe's customer object accepts {none, exempt,
	// reverse}. Mirror our CustomerTaxStatus so Stripe Tax calculations
	// honor the tenant's classification when the customer is referenced.
	switch bp.TaxStatus {
	case tax.StatusExempt:
		params.TaxExempt = stripe.String(string(stripe.CustomerTaxExemptExempt))
	case tax.StatusReverseCharge:
		params.TaxExempt = stripe.String(string(stripe.CustomerTaxExemptReverse))
	case tax.StatusStandard, "":
		params.TaxExempt = stripe.String(string(stripe.CustomerTaxExemptNone))
	}

	_, err = sc.V1Customers.Update(ctx, stripeCustomerID, params)
	if err != nil {
		return fmt.Errorf("stripe customer update: %w", err)
	}

	// Reconcile tax_ids[] on the Stripe Customer. Velox stores a single
	// (type, value) on the billing profile; Stripe stores a collection
	// keyed by id. Reconcile = list existing → delete the ones that
	// don't match desired → create if desired isn't already present.
	// Best-effort: failure logs but doesn't unwind the customer update
	// (per the Velox→Stripe sync pattern). Phase 2 of the sync gap-
	// closure (2026-05-29) — Stripe Dashboard's Tax IDs tab now mirrors
	// the operator's billing profile, matching Lago/Recurly/Chargebee.
	if err := s.reconcileTaxIDs(ctx, sc, stripeCustomerID, bp.TaxIDType, bp.TaxID); err != nil {
		return fmt.Errorf("stripe customer tax_ids reconcile: %w", err)
	}
	return nil
}

// reconcileTaxIDs aligns the Stripe Customer's tax_ids[] collection
// with the single (desiredType, desiredValue) Velox tracks per
// billing profile. Idempotent: re-running with the same desired pair
// is a no-op (list shows it already present, nothing to delete or
// create). Empty desired = clear all existing.
//
// Stripe tax_ids are immutable — changing a value requires
// delete-old + create-new. The reconcile expresses that without
// callers needing to know.
func (s *StripeBillingSync) reconcileTaxIDs(ctx context.Context, sc *stripe.Client, stripeCustomerID, desiredType, desiredValue string) error {
	existingMatches := false
	for tid, err := range sc.V1TaxIDs.List(ctx, &stripe.TaxIDListParams{Customer: stripe.String(stripeCustomerID)}) {
		if err != nil {
			return fmt.Errorf("list tax_ids: %w", err)
		}
		if desiredType != "" && desiredValue != "" &&
			string(tid.Type) == desiredType && tid.Value == desiredValue {
			existingMatches = true
			continue
		}
		// Drift: this tax_id doesn't match the operator's current
		// billing profile. Delete so the next reconcile leaves
		// exactly the desired pair (or nothing) on file.
		if _, delErr := sc.V1TaxIDs.Delete(ctx, tid.ID, &stripe.TaxIDDeleteParams{
			Customer: stripe.String(stripeCustomerID),
		}); delErr != nil {
			return fmt.Errorf("delete tax_id %s: %w", tid.ID, delErr)
		}
	}
	if desiredType != "" && desiredValue != "" && !existingMatches {
		if _, err := sc.V1TaxIDs.Create(ctx, &stripe.TaxIDCreateParams{
			Customer: stripe.String(stripeCustomerID),
			Type:     stripe.String(desiredType),
			Value:    stripe.String(desiredValue),
		}); err != nil {
			return fmt.Errorf("create tax_id: %w", err)
		}
	}
	return nil
}
