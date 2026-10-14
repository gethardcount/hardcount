package domain

import "time"

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
	// SimEffectiveAt / TestClockID are the SIM AXIS (migration 0148, ADR-090
	// §5): the simulated instant this action landed at, and the test clock
	// whose world it landed in. NULL/empty on every wall-clock row, which is
	// almost all of them — hence the partial index.
	//
	// They exist because ADR-086 teardown hard-deletes a clock's entire
	// simulated graph: once the clock is gone, the audit log is the ONLY
	// surviving record of the simulation, and created_at (wall-clock, ADR-030)
	// collapses months of simulated events into the one instant the operator
	// clicked Advance. Only this axis can order or window them.
	SimEffectiveAt *time.Time `json:"sim_effective_at,omitempty"`
	TestClockID    string     `json:"test_clock_id,omitempty"`

	// SubjectDeleted: the resource this row names no longer exists, because the
	// test clock it belonged to was torn down (ADR-086 hard-deletes the whole
	// simulated graph). The ROW survives on purpose — it is the only remaining
	// record that the simulation happened — but a "View" link on it would 404, so
	// the dashboard must not offer one. Resolved at read time; never stored.
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
	// AuditActionCollect covers an operator manually charging a finalized
	// invoice's saved card (POST /invoices/{id}/collect). AuditActionSend
	// covers operator-initiated invoice emails (POST /invoices/{id}/send).
	// Both write an explicit audit row naming the money action they performed.
	// (Historically this also kept the catch-all middleware from recording them
	// as a generic "create"; that middleware is deleted — ADR-090 — and these
	// rows are now the only record either way.)
	AuditActionCollect = "collect"
	AuditActionSend    = "send"
	// AuditActionRetryTax covers operator-initiated tax recompute on a
	// pending/failed invoice. Persists the actor + before/after attention
	// so post-mortems can see who retried what and whether it cleared.
	AuditActionRetryTax = "retry_tax"
	// AuditActionRotate covers credential/token rotation (API keys,
	// webhook secrets, hosted-invoice public tokens). Its own action so
	// audit queries can surface "who rotated what, when" separately from
	// generic updates.
	AuditActionRotate = "rotate"
	// AuditActionRun covers an operator-triggered billing cycle
	// (POST /v1/billing/run). Its own action because the per-invoice
	// finalize rows a run writes are identical whether the scheduler or an
	// operator drove the cycle — only this row answers "who started it".
	// The wire string is NOT new: it is exactly what the middleware
	// catch-all has recorded for this route since day one, now given a
	// constant and an explicit emitter (ADR-090 frozen vocabulary).
	AuditActionRun = "run"
	// AuditActionExport covers BULK DATA EGRESS — the /v1/exports/*.csv
	// streams that hand an operator a whole table in one request (customers
	// and their PII, every invoice, every subscription, a year of usage
	// events, and the audit log itself).
	//
	// The first action in this vocabulary that records a READ: every other
	// value marks a state change. A tamper-evidence system that cannot show
	// who COPIED the evidence has a hole in its chain of custody — Stripe and
	// AWS CloudTrail both log data-export/read events. Emitted BEFORE the
	// first byte streams, and fail-closed (exportsHandler.auditExport,
	// ADR-090 §7).
	//
	// resource_type is the exported resource (customer / invoice /
	// subscription / usage_event / audit_log); resource_id is EMPTY — a bulk
	// export has no single subject.
	AuditActionExport = "export"
)

type TenantSettings struct {
	TenantID        string `json:"tenant_id"`
	DefaultCurrency string `json:"default_currency"`
	Timezone        string `json:"timezone"`
	InvoicePrefix   string `json:"invoice_prefix"`
	NetPaymentTerms int    `json:"net_payment_terms"`
	// CreditBalanceLowThresholdCents arms the credit.balance_low outbound
	// event: a ledger write that crosses a customer's balance from >= to
	// < this value emits the event (ADR-078). Nil = low alerts off.
	// balance_depleted (>0→0) and balance_recovered (0→>0) fire regardless.
	CreditBalanceLowThresholdCents *int64  `json:"credit_balance_low_threshold_cents,omitempty"`
	TaxRate                        float64 `json:"tax_rate"` // Percent rate (4-decimal precision via NUMERIC(7,4)). 7.25 = 7.25%. ADR-042/043.
	TaxName                        string  `json:"tax_name,omitempty"`
	TaxInclusive                   bool    `json:"tax_inclusive"`
	// TaxProvider selects the backend used to compute tax. 'none' skips tax
	// entirely; 'manual' uses the flat tenant-level rate below. Future
	// providers (e.g. 'stripe_tax') will be added once their integrations are
	// end-to-end verified.
	TaxProvider string `json:"tax_provider"`
	// TaxOnFailure is "block" (ADR-041 removed "fallback_manual"). Retained
	// for forward-compat. Defers invoice to tax_status=pending on provider
	// failure; TaxRetrier reconciler picks it up on the next tick.
	// Older context (pre-ADR-041) follows:
	// Old purpose: controls what the engine does when the configured provider
	// fails transiently. 'block' (default) defers the invoice to
	// tax_status=pending and lets a retry worker try again — the safe choice
	// for jurisdictions where the manual flat rate would be wrong.
	// 'fallback_manual' preserves the legacy behaviour: silently apply the
	// tenant-configured manual rate and proceed. Opt-in only; existing
	// tenants are migrated to fallback_manual to avoid a silent policy flip.
	TaxOnFailure string `json:"tax_on_failure,omitempty"`
	// DefaultProductTaxCode is the Stripe product tax code applied when a
	// plan doesn't carry its own. Defaults to txcd_10103001 (SaaS, business
	// use) on first save; tenants override when their product mix diverges.
	// Only consulted by the stripe_tax provider; manual/none ignore it.
	DefaultProductTaxCode string `json:"default_product_tax_code,omitempty"`
	CompanyName           string `json:"company_name,omitempty"`
	// Structured registered-business address. Printed in the invoice "From"
	// block and used to seed tax_home_country on first save. Flat fields
	// mirror the billing_profile.Address shape on the customer side for
	// codebase consistency and shared frontend components.
	CompanyAddressLine1 string `json:"company_address_line1,omitempty"`
	CompanyAddressLine2 string `json:"company_address_line2,omitempty"`
	CompanyCity         string `json:"company_city,omitempty"`
	CompanyState        string `json:"company_state,omitempty"`
	CompanyPostalCode   string `json:"company_postal_code,omitempty"`
	// CompanyCountry (ISO-3166 alpha-2) is the registered-business country.
	// Distinct from TaxHomeCountry: business can be registered in country A
	// but tax-resident in country B (rare, but common enough in multinational
	// setups that Stripe/QuickBooks model them separately).
	CompanyCountry string `json:"company_country,omitempty"`
	CompanyEmail   string `json:"company_email,omitempty"`
	CompanyPhone   string `json:"company_phone,omitempty"`
	LogoURL        string `json:"logo_url,omitempty"`
	// BrandColor is a 7-char hex accent color (#rrggbb) applied to the
	// company name and a thin bar on invoice PDFs. Empty = no brand —
	// the PDF falls back to its neutral default palette.
	BrandColor string `json:"brand_color,omitempty"`
	// TaxID is the SELLER's tax identifier (VAT, EIN, GSTIN, ABN, ...),
	// printed in the invoice "From" block. Separate from customer.TaxID /
	// billing_profile.TaxID which hold the BUYER's identifier.
	TaxID string `json:"tax_id,omitempty"`
	// SupportURL appears in the invoice footer so customers have a
	// self-serve channel for billing questions.
	SupportURL string `json:"support_url,omitempty"`
	// InvoiceFooter is the tenant-level default footer text applied to
	// newly issued invoices. Per-invoice override via invoices.footer is
	// already supported and takes precedence.
	InvoiceFooter string `json:"invoice_footer,omitempty"`
	// audit_fail_closed was retired by ADR-089 (the post-commit 503 swap it
	// controlled was cached by the Idempotency layer, permanently stranding
	// committed mutations behind an error) and its column was DROPPED by
	// migration 0149. Fail-closed is now structural, not a setting: audit rows
	// ride the business transaction (ADR-090 LogInTx), so there is no
	// post-commit window to police and nothing left to opt into.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StripeProviderCredentials holds a tenant's connection to their own Stripe
// account for a single mode (live or test). Velox is a billing engine, not a
// merchant of record — tenants bring their own Stripe accounts and Velox
// orchestrates billing through the supplied keys. Secret keys and webhook
// signing secrets are encrypted at rest; the plaintext is only present in
// transit (Connect call → Stripe verify → DB row).
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
	// RetriesQueued is set ONLY on the Connect-response path
	// (ADR-019) — the count of stuck-on-provider-config invoices
	// the post-connect goroutine is about to retry in the
	// background. Not persisted; computed at response time so the
	// dashboard can render "Retrying N stuck invoices" without a
	// second round-trip. Always zero on List / Get responses.
	RetriesQueued int `json:"retries_queued,omitempty"`
}
