package domain

import (
	"time"
)

// CustomerTaxStatus drives the tax decision for a customer. It mirrors the
// CHECK constraint on customer_billing_profiles.tax_status.
type CustomerTaxStatus string

const (
	// TaxStatusStandard: a regular taxable customer. Tax is applied by the
	// provider's rules.
	TaxStatusStandard CustomerTaxStatus = "standard"
	// TaxStatusExempt: the customer is not taxable (a reseller, a non-profit,
	// a government entity with a certificate on file). Tax is zero, and
	// exempt_reason is recorded on the invoice for audit.
	TaxStatusExempt CustomerTaxStatus = "exempt"
	// TaxStatusReverseCharge: a B2B buyer accounts for the tax itself in its
	// own jurisdiction (EU VAT reverse charge, Indian GST RCM, and so on). Tax
	// is zero on the seller's invoice. The PDF prints the legally required
	// legend that tells the buyer to self-assess.
	TaxStatusReverseCharge CustomerTaxStatus = "reverse_charge"
)

// CustomerStatus says whether a customer is active or archived.
type CustomerStatus string

const (
	CustomerStatusActive   CustomerStatus = "active"
	CustomerStatusArchived CustomerStatus = "archived"
)

// CustomerEmailStatus mirrors the CHECK constraint on customers.email_status.
// 'unknown' is the default: no delivery outcome has been seen. 'bounced' is
// set by Sender on an SMTP 5xx, or later by provider webhooks (SES/SendGrid)
// that plug into the same customer.MarkEmailBounced code path.
type CustomerEmailStatus string

const (
	EmailStatusUnknown    CustomerEmailStatus = "unknown"
	EmailStatusOK         CustomerEmailStatus = "ok"
	EmailStatusBounced    CustomerEmailStatus = "bounced"
	EmailStatusComplained CustomerEmailStatus = "complained"
)

// Customer is one of a tenant's customers: the account Hardcount bills.
type Customer struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id,omitempty"`
	ExternalID  string `json:"external_id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	// AdditionalEmails are CC'd on billing documents and billing-state emails
	// (invoice, receipt, dunning, payment failed, credit note). They are never
	// CC'd on emails that carry a credential (setup links, resets, invites).
	// They are stored encrypted next to email, as a JSON array under the same
	// encryptor. The service validates them, allows at most 10, and
	// normalizes them to lowercase addr-specs (ADR-082).
	AdditionalEmails []string       `json:"additional_emails,omitempty"`
	Status           CustomerStatus `json:"status"`
	// EmailStatus is the deliverability signal that bounce-capture hooks set.
	// Most rows stay 'unknown' until a send outcome is seen. A partner that
	// never sends stays at the default forever.
	EmailStatus        CustomerEmailStatus `json:"email_status,omitempty"`
	EmailLastBouncedAt *time.Time          `json:"email_last_bounced_at,omitempty"`
	EmailBounceReason  string              `json:"email_bounce_reason,omitempty"`
	// Customer deliberately has no CostDashboardToken field. The public
	// cost-dashboard credential is stored only as a SHA-256 blind index (a
	// hash that a lookup can match), so a read has no plaintext to fill in.
	// The raw token exists in the process only for the length of one rotate
	// response. A field here with a json tag would disclose the credential
	// again on every authenticated customer GET and List. The field is
	// absent, not just untagged, so nobody can bring that leak back by adding
	// a tag. Resolve a presented token with
	// customer.Store.GetByCostDashboardToken, which hashes before it
	// compares.

	// TestClockID pins this customer to a test clock, as in Stripe (ADR-027).
	// It is set at create time, and then every subscription and invoice of
	// this customer runs on that clock's simulated time. It is empty for
	// live-mode customers, and for test-mode customers that are not pinned.
	// It cannot be changed after creation. To switch clocks, delete and
	// recreate the customer, as in Stripe.
	TestClockID string `json:"test_clock_id,omitempty"`
	// DunningPolicyID assigns this customer to a dunning policy, also called
	// a campaign (ADR-036). Dunning is the retries and reminders for an
	// unpaid invoice. Empty means the tenant's default policy. PATCH can
	// change it at any time. A change affects only the next dunning run
	// started for this customer's invoices. Runs already in flight keep their
	// original policy.
	DunningPolicyID string `json:"dunning_policy_id,omitempty"`
	// StripeCustomerID is the Stripe Customer object mapped 1:1 to this
	// Hardcount customer. It is created on the first payment-method action
	// (paymentmethods.StripeAdapter.EnsureStripeCustomer). It is empty for a
	// customer that never went through a payment-method flow. This field is
	// the single source of truth for the mapping.
	StripeCustomerID string `json:"stripe_customer_id,omitempty"`
	// Livemode is the mode the customer row lives in, live or test. Lookups
	// that resolve a customer outside any tenant context fill it in, for
	// example GetByCostDashboardToken on the public cost-dashboard route. The
	// caller must pin it onto ctx with postgres.WithLivemode before any
	// TxTenant read. Otherwise the row-level security (RLS) livemode
	// predicate defaults to live, and a test-mode customer's reads return
	// nothing.
	Livemode  bool      `json:"livemode,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BillingProfileStatus says whether a customer's billing profile is missing,
// incomplete or ready.
type BillingProfileStatus string

const (
	BillingProfileMissing    BillingProfileStatus = "missing"
	BillingProfileIncomplete BillingProfileStatus = "incomplete"
	BillingProfileReady      BillingProfileStatus = "ready"
)

// CustomerBillingProfile is a customer's billing details: legal name, phone,
// address, currency and tax settings.
type CustomerBillingProfile struct {
	CustomerID string `json:"customer_id"`
	TenantID   string `json:"tenant_id,omitempty"`
	LegalName  string `json:"legal_name,omitempty"`
	// The billing profile has no email of its own. customers.email is the one
	// canonical recipient, and customers.additional_emails is the CC list
	// (ADR-082). A second copy of the address on the profile could drift from
	// customers.email and break the bounce-tracking key. ADR-082 also records
	// when a customer_email_contacts table (per-contact names, routing per
	// email type, per-contact deliverability) would be added: when a design
	// partner asks for per-recipient preferences.

	Phone        string `json:"phone,omitempty"`
	AddressLine1 string `json:"address_line1,omitempty"`
	AddressLine2 string `json:"address_line2,omitempty"`
	City         string `json:"city,omitempty"`
	State        string `json:"state,omitempty"`
	PostalCode   string `json:"postal_code,omitempty"`
	Country      string `json:"country,omitempty"`
	Currency     string `json:"currency,omitempty"`
	// TaxStatus drives tax-decision routing, whatever provider is configured.
	// 'standard' is the default. 'exempt' and 'reverse_charge' both zero the
	// invoice tax, but print different legends on the PDF: exempt shows the
	// certificate reason, and reverse charge tells the buyer to self-assess.
	// It mirrors the CHECK constraint on customer_billing_profiles.tax_status.
	TaxStatus       CustomerTaxStatus    `json:"tax_status"`
	TaxExemptReason string               `json:"tax_exempt_reason,omitempty"`
	TaxID           string               `json:"tax_id"`
	TaxIDType       string               `json:"tax_id_type"`
	ProfileStatus   BillingProfileStatus `json:"profile_status"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

// PaymentSetupStatus is the state of a customer's payment setup.
type PaymentSetupStatus string

const (
	PaymentSetupMissing PaymentSetupStatus = "missing"
	PaymentSetupPending PaymentSetupStatus = "pending"
	PaymentSetupReady   PaymentSetupStatus = "ready"
	PaymentSetupError   PaymentSetupStatus = "error"
)

// CustomerPaymentSetup is a customer's payment setup: its status, whether a
// default payment method is present, and that method's card details.
type CustomerPaymentSetup struct {
	CustomerID                  string             `json:"customer_id"`
	TenantID                    string             `json:"tenant_id,omitempty"`
	SetupStatus                 PaymentSetupStatus `json:"setup_status"`
	DefaultPaymentMethodPresent bool               `json:"default_payment_method_present"`
	PaymentMethodType           string             `json:"payment_method_type,omitempty"`
	StripeCustomerID            string             `json:"stripe_customer_id,omitempty"`
	StripePaymentMethodID       string             `json:"stripe_payment_method_id,omitempty"`
	CardBrand                   string             `json:"card_brand,omitempty"`
	CardLast4                   string             `json:"card_last4,omitempty"`
	CardExpMonth                int                `json:"card_exp_month,omitempty"`
	CardExpYear                 int                `json:"card_exp_year,omitempty"`
	LastVerifiedAt              *time.Time         `json:"last_verified_at,omitempty"`
	CreatedAt                   time.Time          `json:"created_at"`
	UpdatedAt                   time.Time          `json:"updated_at"`
}
