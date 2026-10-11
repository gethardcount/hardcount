package paymentmethods

import (
	"context"
	"errors"
	"fmt"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// CustomerStripeLink is the narrow surface the adapter uses to
// read/write the Stripe Customer ID mapping. customers.stripe_customer_id
// has been the canonical home since migration 0096; this interface
// abstracts the customer store so the adapter doesn't gain a hard
// dependency on internal/customer (which would create a cycle).
type CustomerStripeLink interface {
	Get(ctx context.Context, tenantID, customerID string) (domain.Customer, error)
	SetStripeCustomerID(ctx context.Context, tenantID, customerID, stripeCustomerID string) error
	// GetBillingProfile returns the billing profile (legal_name, phone,
	// address, tax_status) so EnsureStripeCustomer can pre-populate
	// the Stripe Customer object at create time instead of leaving
	// email/name/address null. ErrNotFound = customer doesn't have a
	// profile yet — adapter passes only the Customer-level fields.
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)
}

// StripeAdapter wires the payment-methods Service to the existing
// payment.StripeClients pool. Mode-aware — ForCtx(ctx) returns the right
// client for the caller's livemode, set from the auth ctx.
type StripeAdapter struct {
	clients      *payment.StripeClients
	customerLink CustomerStripeLink
}

func NewStripeAdapter(clients *payment.StripeClients, customerLink CustomerStripeLink) *StripeAdapter {
	return &StripeAdapter{clients: clients, customerLink: customerLink}
}

var _ StripeAPI = (*StripeAdapter)(nil)

func (a *StripeAdapter) client(ctx context.Context) (*stripe.Client, error) {
	c := a.clients.ForCtx(ctx)
	if c == nil {
		return nil, errors.New("stripe not configured for this mode")
	}
	return c, nil
}

// EnsureStripeCustomer resolves (or lazily creates) the Stripe customer
// for this Velox customer. Single source of truth: customers.stripe_customer_id
// (migration 0096). A Velox customer without a Stripe Customer record
// gets one created here on first PM action; subsequent calls
// short-circuit on the persisted ID.
//
// Stripe client lookup uses explicit (tenantID, livemode) rather than
// `ForCtx` so this method works on BOTH authenticated portal requests
// (auth.TenantID populated) and public token-authenticated requests
// (hosted-invoice Pay flow — no auth ctx, tenantID comes from the
// public_token row, livemode pinned by hostedinvoice.resolveInvoice).
func (a *StripeAdapter) EnsureStripeCustomer(ctx context.Context, tenantID, customerID string) (string, error) {
	cust, err := a.customerLink.Get(ctx, tenantID, customerID)
	if err != nil {
		// A read error is not "no Stripe customer yet": creating one here
		// re-mapped the customer to a new, empty Stripe customer, stranding
		// the saved card on the old one.
		return "", fmt.Errorf("read customer %s: %w", customerID, err)
	}
	if cust.StripeCustomerID != "" {
		return cust.StripeCustomerID, nil
	}

	livemode := postgres.Livemode(ctx)
	sc := a.clients.For(ctx, tenantID, livemode)
	if sc == nil {
		return "", fmt.Errorf("stripe not configured for tenant=%s livemode=%v", tenantID, livemode)
	}

	// Pre-populate the Stripe Customer with the Velox-side fields
	// (email + display name + billing profile if present) at create
	// time. Pre-2026-05-29 this created the row with metadata only,
	// leaving email/name/address null on Stripe — Checkout couldn't
	// pre-fill the email field, Stripe Dashboard showed a blank
	// customer row for support, and downstream UpsertBillingProfile
	// was the only path that ever populated those fields. Reading
	// + pushing here matches Lago / Recurly / Chargebee: every
	// platform's create-customer-on-Stripe call carries the
	// upstream's known fields, not just an ID.
	params := &stripe.CustomerCreateParams{
		Params: stripe.Params{
			Metadata: map[string]string{
				"velox_tenant_id":   tenantID,
				"velox_customer_id": customerID,
			},
		},
	}
	if cust.Email != "" {
		params.Email = stripe.String(cust.Email)
	}
	if cust.DisplayName != "" {
		params.Name = stripe.String(cust.DisplayName)
	}
	// Billing profile is best-effort: a customer can be created via
	// API without a profile (set up later via UpsertBillingProfile).
	// ErrNotFound is the normal case for brand-new customers.
	bp, bpErr := a.customerLink.GetBillingProfile(ctx, tenantID, customerID)
	if bpErr == nil {
		if bp.LegalName != "" {
			// Legal name on the billing profile overrides display
			// name on Stripe — matches SyncBillingProfile's choice.
			params.Name = stripe.String(bp.LegalName)
		}
		if bp.Phone != "" {
			params.Phone = stripe.String(bp.Phone)
		}
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
		switch bp.TaxStatus {
		case domain.TaxStatusExempt:
			params.TaxExempt = stripe.String(string(stripe.CustomerTaxExemptExempt))
		case domain.TaxStatusReverseCharge:
			params.TaxExempt = stripe.String(string(stripe.CustomerTaxExemptReverse))
		case domain.TaxStatusStandard, "":
			params.TaxExempt = stripe.String(string(stripe.CustomerTaxExemptNone))
		}
		// Pre-populate tax_ids[] on the new Stripe Customer when the
		// billing profile has one. Stripe accepts tax_id_data on
		// create; later updates flow through the dedicated tax_ids
		// reconcile in payment.StripeBillingSync. Without this, a
		// new Stripe Customer would briefly show an empty Tax IDs
		// tab even though the operator already filled in VAT/GST.
		if bp.TaxID != "" && bp.TaxIDType != "" {
			params.TaxIDData = []*stripe.CustomerCreateTaxIDDataParams{{
				Type:  stripe.String(bp.TaxIDType),
				Value: stripe.String(bp.TaxID),
			}}
		}
	}

	stripeCust, err := sc.V1Customers.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe create customer: %w", err)
	}

	// Persist on the customer row so future calls short-circuit. The
	// returned id must be the RECORDED one: a card saved on a Stripe
	// customer Velox has not recorded can never be charged. If the write
	// fails, re-read — a concurrent first-PM action may have recorded its
	// own customer, and that winner is the one to use; otherwise fail.
	if err := a.customerLink.SetStripeCustomerID(ctx, tenantID, customerID, stripeCust.ID); err != nil {
		if now, rerr := a.customerLink.Get(ctx, tenantID, customerID); rerr == nil && now.StripeCustomerID != "" {
			return now.StripeCustomerID, nil
		}
		return "", fmt.Errorf("record stripe customer %s for %s: %w", stripeCust.ID, customerID, err)
	}
	return stripeCust.ID, nil
}

func (a *StripeAdapter) CreateSetupIntent(ctx context.Context, stripeCustomerID string, metadata map[string]string) (string, string, error) {
	sc, err := a.client(ctx)
	if err != nil {
		return "", "", err
	}
	si, err := sc.V1SetupIntents.Create(ctx, &stripe.SetupIntentCreateParams{
		Customer:           stripe.String(stripeCustomerID),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		Usage:              stripe.String("off_session"),
		Params:             stripe.Params{Metadata: metadata},
	})
	if err != nil {
		return "", "", fmt.Errorf("stripe create setup intent: %w", err)
	}
	return si.ClientSecret, si.ID, nil
}

// CreateSetupCheckoutSession creates a Checkout Session in setup mode so
// the customer can be redirected to Stripe's hosted UI to enter card
// details without being charged. Mirrors payment.PortalHandler but for
// the self-serve /me path — the metadata we attach here is what the
// setup_intent.succeeded webhook routes back to the right customer.
func (a *StripeAdapter) CreateSetupCheckoutSession(ctx context.Context, stripeCustomerID, successURL, cancelURL string, metadata map[string]string) (string, string, error) {
	sc, err := a.client(ctx)
	if err != nil {
		return "", "", err
	}
	sess, err := sc.V1CheckoutSessions.Create(ctx, &stripe.CheckoutSessionCreateParams{
		Customer:           stripe.String(stripeCustomerID),
		Mode:               stripe.String(string(stripe.CheckoutSessionModeSetup)),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(cancelURL),
		// Propagate metadata to BOTH the Checkout Session AND the
		// underlying SetupIntent. The setup_intent.succeeded webhook
		// reads velox_customer_id off the SetupIntent's metadata —
		// without SetupIntentData here, that field is empty and the
		// PM attach silently skips (handler logs "missing
		// velox_customer_id" and returns nil, leaving the customer
		// with a card on Stripe's side but no payment_methods row
		// locally — the exact symptom that surfaced 2026-05-26).
		// Stripe does not auto-copy Session metadata to SetupIntent.
		SetupIntentData: &stripe.CheckoutSessionCreateSetupIntentDataParams{Metadata: metadata},
		Params:          stripe.Params{Metadata: metadata},
	})
	if err != nil {
		return "", "", fmt.Errorf("stripe create setup checkout session: %w", err)
	}
	return sess.URL, sess.ID, nil
}

func (a *StripeAdapter) DetachPaymentMethod(ctx context.Context, stripePaymentMethodID string) error {
	sc, err := a.client(ctx)
	if err != nil {
		return err
	}
	if _, err := sc.V1PaymentMethods.Detach(ctx, stripePaymentMethodID, nil); err != nil {
		// Stripe returns 404 for "already detached" / "no such payment
		// method". Translate to ErrNotFound so callers can decide
		// whether to treat it as success.
		var se *stripe.Error
		if errors.As(err, &se) && se.HTTPStatusCode == 404 {
			return errs.ErrNotFound
		}
		return fmt.Errorf("stripe detach: %w", err)
	}
	return nil
}

// SetDefaultPaymentMethod points the Stripe Customer's
// `invoice_settings.default_payment_method` at the given PM. Resolves
// the Stripe Customer ID via the customerLink — the local PM row was
// already updated, the Stripe Customer must already exist (the PM
// couldn't have been attached otherwise). Returns errs.ErrNotFound if
// the Velox customer has no linked Stripe customer (out-of-band data
// drift); the Service treats that as a soft skip + audit row entry,
// not a fail, because the local default is still authoritative.
func (a *StripeAdapter) SetDefaultPaymentMethod(ctx context.Context, tenantID, customerID, stripePaymentMethodID string) error {
	cust, err := a.customerLink.Get(ctx, tenantID, customerID)
	if err != nil {
		return fmt.Errorf("lookup velox customer: %w", err)
	}
	if cust.StripeCustomerID == "" {
		return errs.ErrNotFound
	}
	sc, err := a.client(ctx)
	if err != nil {
		return err
	}
	_, err = sc.V1Customers.Update(ctx, cust.StripeCustomerID, &stripe.CustomerUpdateParams{
		InvoiceSettings: &stripe.CustomerUpdateInvoiceSettingsParams{
			DefaultPaymentMethod: stripe.String(stripePaymentMethodID),
		},
	})
	if err != nil {
		return fmt.Errorf("stripe set default pm: %w", err)
	}
	return nil
}

func (a *StripeAdapter) FetchPaymentMethodCard(ctx context.Context, stripePaymentMethodID string) (CardMetadata, error) {
	sc, err := a.client(ctx)
	if err != nil {
		return CardMetadata{}, err
	}
	pm, err := sc.V1PaymentMethods.Retrieve(ctx, stripePaymentMethodID, nil)
	if err != nil {
		return CardMetadata{}, fmt.Errorf("stripe retrieve pm: %w", err)
	}
	if pm.Card == nil {
		return CardMetadata{}, nil
	}
	return CardMetadata{
		Brand:       string(pm.Card.Brand),
		Last4:       pm.Card.Last4,
		ExpMonth:    int(pm.Card.ExpMonth),
		ExpYear:     int(pm.Card.ExpYear),
		Fingerprint: pm.Card.Fingerprint,
	}, nil
}
