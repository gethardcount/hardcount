package domain

import "time"

// AuditEntry is one row of the audit log: who did what to which resource,
// and when.
type AuditEntry struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id,omitempty"`
	ActorType     string         `json:"actor_type"` // api_key, user, system
	ActorID       string         `json:"actor_id"`
	ActorName     string         `json:"actor_name,omitempty"` // resolved from api_keys.name when ActorType=api_key
	Action        string         `json:"action"`
	ResourceType  string         `json:"resource_type"`
	ResourceID    string         `json:"resource_id"`
	ResourceLabel string         `json:"resource_label,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	IPAddress     string         `json:"ip_address,omitempty"`
	RequestID     string         `json:"request_id,omitempty"` // chi request UUID — joins back to server logs
	CreatedAt     time.Time      `json:"created_at"`
	// SimEffectiveAt and TestClockID are the sim axis (migration 0148,
	// ADR-090 §5). SimEffectiveAt is the simulated instant the action
	// happened at, and TestClockID is the test clock whose simulated time it
	// happened in. Both are NULL or empty on every wall-clock row, which is
	// almost every row, so their index is partial.
	//
	// They exist because ADR-086 teardown hard-deletes everything a test
	// clock simulated. Once the clock is gone, the audit log is the only
	// record left of the simulation. Its created_at is wall-clock time
	// (ADR-030), which puts months of simulated events at the one instant
	// the operator clicked Advance. Only this axis can order those events or
	// select a window of them.
	SimEffectiveAt *time.Time `json:"sim_effective_at,omitempty"`
	TestClockID    string     `json:"test_clock_id,omitempty"`

	// SubjectDeleted means the resource this row names no longer exists,
	// because its test clock was torn down (ADR-086 hard-deletes everything
	// the clock simulated). The row stays on purpose: it is the only record
	// left that the simulation happened. A "View" link on it would return
	// 404, so the dashboard must not offer one. It is worked out at read
	// time and never stored.
	SubjectDeleted bool `json:"subject_deleted,omitempty"`
}

// Well-known audit actions.
const (
	AuditActionCreate   = "create"
	AuditActionUpdate   = "update"
	AuditActionDelete   = "delete"
	AuditActionActivate = "activate"
	AuditActionCancel   = "cancel"
	AuditActionPause    = "pause"
	AuditActionResume   = "resume"
	AuditActionFinalize = "finalize"
	AuditActionVoid     = "void"
	AuditActionRevoke   = "revoke"
	AuditActionGrant    = "grant"
	AuditActionRefund   = "refund"
	// AuditActionCollect covers an operator charging a finalized invoice's
	// saved card by hand (POST /invoices/{id}/collect). AuditActionSend
	// covers an operator sending an invoice email (POST /invoices/{id}/send).
	// Each writes an explicit audit row that names the money action it
	// performed. There is no catch-all audit middleware (ADR-090), so these
	// rows are the only record of either action.
	AuditActionCollect = "collect"
	AuditActionSend    = "send"
	// AuditActionRetryTax covers an operator recomputing tax on a pending
	// invoice. The row keeps the actor and the attention before and after,
	// so a post-mortem can see who retried what and whether it cleared.
	AuditActionRetryTax = "retry_tax"
	// AuditActionRotate covers rotating a credential or token (API keys,
	// webhook secrets, hosted-invoice public tokens). It has its own action
	// so audit queries can show who rotated what, and when, apart from
	// ordinary updates.
	AuditActionRotate = "rotate"
	// AuditActionRun covers an operator starting a billing cycle
	// (POST /v1/billing/run). It has its own action because the per-invoice
	// finalize rows of a run are identical whether the scheduler or an
	// operator started the cycle. Only this row answers "who started it".
	// Its string is the one older rows for this route already carry, because
	// ADR-090 freezes the audit vocabulary.
	AuditActionRun = "run"
	// AuditActionExport covers bulk data egress: the /v1/exports/*.csv
	// streams that hand an operator a whole table in one request. They cover
	// customers and their PII, every invoice, every subscription, a year of
	// usage events, and the audit log itself.
	//
	// It is the first action in this vocabulary that records a read. Every
	// other value marks a state change. A tamper-evidence system that cannot
	// show who copied the evidence has a hole in its chain of custody, and
	// Stripe and AWS CloudTrail both log data-export and read events. The
	// row is written before the first byte streams, and it fails closed
	// (exportsHandler.auditExport, ADR-090 §7).
	//
	// resource_type is the exported resource (customer, invoice,
	// subscription, usage_event or audit_log). resource_id is empty, because
	// a bulk export has no single subject.
	AuditActionExport = "export"
)

// TenantSettings is one tenant's account settings, such as its currency,
// timezone, document numbering, tax setup and the company details its
// documents print.
type TenantSettings struct {
	TenantID        string `json:"tenant_id"`
	DefaultCurrency string `json:"default_currency"`
	Timezone        string `json:"timezone"`
	InvoicePrefix   string `json:"invoice_prefix"`
	// CreditNotePrefix starts every credit note number (CN-000001). Changing
	// it renames no issued note: the number is stored when the note is made.
	CreditNotePrefix string `json:"credit_note_prefix"`
	NetPaymentTerms  int    `json:"net_payment_terms"`
	// CreditBalanceLowThresholdCents turns on the credit.balance_low
	// outbound event (ADR-078). A ledger write that takes a customer's
	// balance from >= this value to < it emits the event. Nil turns
	// low-balance alerts off. balance_depleted (>0→0) and balance_recovered
	// (0→>0) fire whatever this is set to.
	CreditBalanceLowThresholdCents *int64  `json:"credit_balance_low_threshold_cents,omitempty"`
	TaxRate                        float64 `json:"tax_rate"` // Percent rate (4-decimal precision via NUMERIC(7,4)). 7.25 = 7.25%. ADR-042/043.
	TaxName                        string  `json:"tax_name,omitempty"`
	TaxInclusive                   bool    `json:"tax_inclusive"`
	// TaxProvider selects the backend that computes tax. 'none' skips tax
	// entirely, and 'manual' uses the flat tenant-level rate below. Future
	// providers (for example 'stripe_tax') will be added once their
	// integrations are verified end to end.
	TaxProvider string `json:"tax_provider"`
	// TaxOnFailure is "block" (ADR-041 removed "fallback_manual"). It is
	// kept for forward compatibility. When the provider fails, the invoice
	// waits in tax_status=pending, and the TaxRetrier reconciler picks it up
	// on the next tick.
	TaxOnFailure string `json:"tax_on_failure,omitempty"`
	// DefaultProductTaxCode is the Stripe product tax code for a plan that
	// has none of its own. It defaults to txcd_10103001 (SaaS, business use)
	// on first save, and a tenant overrides it when its products differ.
	// Only the stripe_tax provider reads it. The manual and none providers
	// ignore it.
	DefaultProductTaxCode string `json:"default_product_tax_code,omitempty"`
	CompanyName           string `json:"company_name,omitempty"`
	// The registered business address, as flat fields. It is printed in the
	// invoice "From" block and used to seed tax_home_country on first save.
	// The fields mirror the customer side's billing_profile.Address shape,
	// so the code stays consistent and the frontend can share components.
	CompanyAddressLine1 string `json:"company_address_line1,omitempty"`
	CompanyAddressLine2 string `json:"company_address_line2,omitempty"`
	CompanyCity         string `json:"company_city,omitempty"`
	CompanyState        string `json:"company_state,omitempty"`
	CompanyPostalCode   string `json:"company_postal_code,omitempty"`
	// CompanyCountry (ISO-3166 alpha-2) is the country the business is
	// registered in. It is distinct from TaxHomeCountry: a business can be
	// registered in country A and tax-resident in country B. That is rare,
	// but common enough in multinational setups that Stripe and QuickBooks
	// model the two separately.
	CompanyCountry string `json:"company_country,omitempty"`
	CompanyEmail   string `json:"company_email,omitempty"`
	CompanyPhone   string `json:"company_phone,omitempty"`
	// LogoURL is shown in emails and on the hosted invoice page. The PDFs
	// do not print it.
	LogoURL string `json:"logo_url,omitempty"`
	// BrandColor is a 7-character hex accent color (#rrggbb) for the company
	// name and a thin bar on invoice PDFs. Empty means no brand, and the PDF
	// uses its neutral default palette.
	BrandColor string `json:"brand_color,omitempty"`
	// TaxID is the seller's tax identifier (VAT, EIN, GSTIN, ABN, ...),
	// printed in the invoice "From" block. It is separate from customer.TaxID
	// and billing_profile.TaxID, which hold the buyer's identifier.
	TaxID string `json:"tax_id,omitempty"`
	// SupportURL is where customers get help with a bill: linked in emails
	// and on the hosted invoice page, and printed at the foot of invoice and
	// credit-note PDFs.
	SupportURL string `json:"support_url,omitempty"`
	// InvoiceFooter is printed under the totals on invoice PDFs and on the
	// hosted invoice page. Each invoice keeps the footer it was issued with
	// (copied into invoices.footer at issue, ADR-109), so an edit applies to
	// invoices issued after it. A draft shows the current text.
	InvoiceFooter string `json:"invoice_footer,omitempty"`
	// There is no audit_fail_closed setting (ADR-089), and migration 0149
	// dropped its column. Its 503 after a committed change would be cached
	// by the idempotency layer and leave the change behind an error
	// permanently. Fail-closed comes from the design instead of a setting:
	// audit rows commit in the business transaction (ADR-090, LogInTx). So
	// there is no window after commit to guard, and nothing to opt into.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StripeProviderCredentials holds a tenant's connection to its own Stripe
// account for one mode (live or test). Hardcount is a billing engine, not a
// merchant of record. Tenants bring their own Stripe accounts, and Hardcount
// runs billing through the keys they supply. Secret keys and webhook signing
// secrets are encrypted at rest. The plaintext exists only in transit
// (Connect call → Stripe verify → DB row).
type StripeProviderCredentials struct {
	ID                 string     `json:"id"`
	TenantID           string     `json:"tenant_id"`
	Livemode           bool       `json:"livemode"`
	StripeAccountID    string     `json:"stripe_account_id,omitempty"`
	StripeAccountName  string     `json:"stripe_account_name,omitempty"`
	SecretKeyPrefix    string     `json:"secret_key_prefix,omitempty"`
	SecretKeyLast4     string     `json:"secret_key_last4"`
	PublishableKey     string     `json:"publishable_key"`
	WebhookSecretLast4 string     `json:"webhook_secret_last4,omitempty"`
	HasWebhookSecret   bool       `json:"has_webhook_secret"`
	VerifiedAt         *time.Time `json:"verified_at,omitempty"`
	LastVerifiedError  string     `json:"last_verified_error,omitempty"`
	// LastVerifiedErrorKind says what kind of failure LastVerifiedError is:
	// "rejected" (Stripe refused the key) or "unavailable" (Stripe was not
	// reached, or answered 429/5xx, so the key was not checked). Empty when
	// the check passed, and on rows that failed before migration 0178.
	LastVerifiedErrorKind string    `json:"last_verified_error_kind,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	// RetriesQueued is set only on the Connect response (ADR-019). It is the
	// number of invoices stuck on provider config that the post-connect
	// goroutine is about to retry in the background. It is not stored. It
	// is computed for the response, so the dashboard can show "Retrying N
	// stuck invoices" without a second round trip. It is always zero on
	// List and Get responses.
	RetriesQueued int `json:"retries_queued,omitempty"`
}
