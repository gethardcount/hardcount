package tenant

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

var invoicePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,20}$`)

var settingsPhonePattern = regexp.MustCompile(`^[\+\d\s\-\(\)]{7,20}$`)

// brandColorPattern accepts the lowercase 7-char hex form (#rrggbb). Short
// form (#rgb), uppercase, and alpha (#rrggbbaa) are rejected so we keep the
// downstream PDF parser trivially simple (3×hex pairs). UI normalizes on
// save; API clients must pre-normalize.
var brandColorPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// SettingsStore handles tenant settings CRUD.
type SettingsStore struct {
	db *postgres.DB
}

func NewSettingsStore(db *postgres.DB) *SettingsStore {
	return &SettingsStore{db: db}
}

// ListTenantIDs enumerates every tenant. Used by background schedulers
// (dunning sweep, approaching-due reminders) that must iterate all tenants
// and cannot carry a per-tenant context. The tenants table itself has no
// RLS, so this query runs on the bare pool without a transaction.
func (s *SettingsStore) ListTenantIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Pool.QueryContext(ctx, `SELECT id FROM tenants`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DefaultSettings returns a fresh TenantSettings populated with
// Velox defaults. Single source of truth for "what does an
// uninitialized tenant look like" — used by SettingsStore.Get
// when no row exists yet AND by bootstrap when seeding a new
// tenant. Pulling this here means the engine path and the API
// handler path produce identical defaults; pre-fix only the API
// handler synthesized them (line 333), so the engine path saw
// ErrNotFound and silently zero-taxed instead.
func DefaultSettings(tenantID string) domain.TenantSettings {
	return domain.TenantSettings{
		TenantID:        tenantID,
		DefaultCurrency: "USD",
		Timezone:        "UTC",
		InvoicePrefix:   "VLX",
		NetPaymentTerms: 30,
		TaxProvider:     "manual",
		TaxOnFailure:    "block",
	}
}

func (s *SettingsStore) Get(ctx context.Context, tenantID string) (domain.TenantSettings, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TenantSettings{}, err
	}
	defer postgres.Rollback(tx)

	var ts domain.TenantSettings
	err = tx.QueryRowContext(ctx, `
		SELECT tenant_id, default_currency, timezone, invoice_prefix,
			net_payment_terms, tax_provider, tax_rate, COALESCE(tax_name,''), tax_inclusive,
			COALESCE(default_product_tax_code,''), tax_on_failure,
			COALESCE(company_name,''),
			COALESCE(company_address_line1,''), COALESCE(company_address_line2,''),
			COALESCE(company_city,''), COALESCE(company_state,''),
			COALESCE(company_postal_code,''), COALESCE(company_country,''),
			COALESCE(company_email,''), COALESCE(company_phone,''), COALESCE(logo_url,''),
			COALESCE(brand_color,''),
			COALESCE(tax_id,''), COALESCE(support_url,''), COALESCE(invoice_footer,''),
			credit_balance_low_threshold_cents, created_at, updated_at
		FROM tenant_settings WHERE tenant_id = $1
	`, tenantID).Scan(&ts.TenantID, &ts.DefaultCurrency, &ts.Timezone, &ts.InvoicePrefix,
		&ts.NetPaymentTerms, &ts.TaxProvider, &ts.TaxRate, &ts.TaxName, &ts.TaxInclusive,
		&ts.DefaultProductTaxCode, &ts.TaxOnFailure,
		&ts.CompanyName,
		&ts.CompanyAddressLine1, &ts.CompanyAddressLine2,
		&ts.CompanyCity, &ts.CompanyState,
		&ts.CompanyPostalCode, &ts.CompanyCountry,
		&ts.CompanyEmail, &ts.CompanyPhone, &ts.LogoURL,
		&ts.BrandColor,
		&ts.TaxID, &ts.SupportURL, &ts.InvoiceFooter,
		&ts.CreditBalanceLowThresholdCents, &ts.CreatedAt, &ts.UpdatedAt)

	if err == sql.ErrNoRows {
		// Synthesize Velox defaults so callers don't have to handle
		// the missing-row case. Mirrors the API handler's behavior;
		// the engine path previously fell through to silent zero-tax
		// when ErrNotFound bubbled up. Now Get always returns a
		// usable struct unless something more serious broke.
		return DefaultSettings(tenantID), nil
	}
	if err != nil {
		return domain.TenantSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TenantSettings{}, err
	}
	return ts, nil
}

func (s *SettingsStore) Upsert(ctx context.Context, ts domain.TenantSettings) (domain.TenantSettings, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, ts.TenantID)
	if err != nil {
		return domain.TenantSettings{}, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	err = tx.QueryRowContext(ctx, `
		INSERT INTO tenant_settings (tenant_id, default_currency, timezone, invoice_prefix,
			net_payment_terms, tax_provider, tax_rate, tax_name, tax_inclusive, default_product_tax_code,
			tax_on_failure,
			company_name,
			company_address_line1, company_address_line2, company_city, company_state,
			company_postal_code, company_country,
			company_email, company_phone,
			logo_url, brand_color, tax_id, support_url, invoice_footer,
			credit_balance_low_threshold_cents, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$27)
		ON CONFLICT (tenant_id) DO UPDATE SET
			default_currency = EXCLUDED.default_currency, timezone = EXCLUDED.timezone,
			invoice_prefix = EXCLUDED.invoice_prefix, net_payment_terms = EXCLUDED.net_payment_terms,
			tax_provider = EXCLUDED.tax_provider,
			tax_rate = EXCLUDED.tax_rate,
			tax_name = EXCLUDED.tax_name,
			tax_inclusive = EXCLUDED.tax_inclusive,
			default_product_tax_code = EXCLUDED.default_product_tax_code,
			tax_on_failure = EXCLUDED.tax_on_failure,
			company_name = EXCLUDED.company_name,
			company_address_line1 = EXCLUDED.company_address_line1,
			company_address_line2 = EXCLUDED.company_address_line2,
			company_city = EXCLUDED.company_city, company_state = EXCLUDED.company_state,
			company_postal_code = EXCLUDED.company_postal_code,
			company_country = EXCLUDED.company_country,
			company_email = EXCLUDED.company_email, company_phone = EXCLUDED.company_phone,
			logo_url = EXCLUDED.logo_url,
			brand_color = EXCLUDED.brand_color,
			tax_id = EXCLUDED.tax_id, support_url = EXCLUDED.support_url,
			invoice_footer = EXCLUDED.invoice_footer,
			credit_balance_low_threshold_cents = EXCLUDED.credit_balance_low_threshold_cents,
			updated_at = EXCLUDED.updated_at
		RETURNING tenant_id, default_currency, timezone, invoice_prefix,
			net_payment_terms, tax_provider, tax_rate, COALESCE(tax_name,''), tax_inclusive,
			COALESCE(default_product_tax_code,''), tax_on_failure,
			COALESCE(company_name,''),
			COALESCE(company_address_line1,''), COALESCE(company_address_line2,''),
			COALESCE(company_city,''), COALESCE(company_state,''),
			COALESCE(company_postal_code,''), COALESCE(company_country,''),
			COALESCE(company_email,''), COALESCE(company_phone,''), COALESCE(logo_url,''),
			COALESCE(brand_color,''),
			COALESCE(tax_id,''), COALESCE(support_url,''), COALESCE(invoice_footer,''),
			credit_balance_low_threshold_cents, created_at, updated_at
	`, ts.TenantID, ts.DefaultCurrency, ts.Timezone, ts.InvoicePrefix,
		ts.NetPaymentTerms, ts.TaxProvider, ts.TaxRate, ts.TaxName, ts.TaxInclusive, ts.DefaultProductTaxCode,
		ts.TaxOnFailure,
		postgres.NullableString(ts.CompanyName),
		postgres.NullableString(ts.CompanyAddressLine1), postgres.NullableString(ts.CompanyAddressLine2),
		postgres.NullableString(ts.CompanyCity), postgres.NullableString(ts.CompanyState),
		postgres.NullableString(ts.CompanyPostalCode), postgres.NullableString(ts.CompanyCountry),
		postgres.NullableString(ts.CompanyEmail), postgres.NullableString(ts.CompanyPhone),
		postgres.NullableString(ts.LogoURL),
		postgres.NullableString(ts.BrandColor),
		postgres.NullableString(ts.TaxID), postgres.NullableString(ts.SupportURL),
		postgres.NullableString(ts.InvoiceFooter),
		ts.CreditBalanceLowThresholdCents, now,
	).Scan(&ts.TenantID, &ts.DefaultCurrency, &ts.Timezone, &ts.InvoicePrefix,
		&ts.NetPaymentTerms, &ts.TaxProvider, &ts.TaxRate, &ts.TaxName, &ts.TaxInclusive,
		&ts.DefaultProductTaxCode, &ts.TaxOnFailure,
		&ts.CompanyName,
		&ts.CompanyAddressLine1, &ts.CompanyAddressLine2,
		&ts.CompanyCity, &ts.CompanyState,
		&ts.CompanyPostalCode, &ts.CompanyCountry,
		&ts.CompanyEmail, &ts.CompanyPhone, &ts.LogoURL,
		&ts.BrandColor,
		&ts.TaxID, &ts.SupportURL, &ts.InvoiceFooter,
		&ts.CreditBalanceLowThresholdCents, &ts.CreatedAt, &ts.UpdatedAt)
	if err != nil {
		return domain.TenantSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TenantSettings{}, err
	}
	return ts, nil
}

// NextInvoiceNumber atomically allocates the next invoice number for a tenant
// in the mode carried on ctx (postgres.WithLivemode). Test mode and live mode
// each have their own monotonic sequence — Stripe-style — so test exploration
// doesn't burn numbers that should have belonged to live invoices.
//
// The UPSERT form lazily initializes tenant_settings on first call so a freshly
// bootstrapped tenant — which has a tenants row but no tenant_settings row —
// gets VLX-000001, VLX-000002, ... without operators needing to touch the
// settings endpoint first. Concurrent callers serialize on the row lock, so
// no number is handed out twice within a mode.
//
// The INSERT seeds the active mode's column to 2 so RETURNING (seq - 1)
// yields 1 for the first invoice and (seq - 1) yields the pre-increment value
// on every subsequent UPDATE call — keeping the sequence monotonic and
// gap-free across both code paths.
func (s *SettingsStore) NextInvoiceNumber(ctx context.Context, tenantID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)
	num, err := s.nextInvoiceNumberInTx(ctx, tx, tenantID)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return num, nil
}

// NextInvoiceNumberTx is the in-transaction variant. Used by callers
// that need to allocate the invoice number and insert the invoice in a
// single atomic step — e.g. the subscription handler's atomic
// AddItem+proration flow. Without this the number was allocated in its
// own tx and committed even when the subsequent invoice insert rolled
// back, leaving a sequence gap. ADR-030 atomic-proration follow-through.
func (s *SettingsStore) NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	return s.nextInvoiceNumberInTx(ctx, tx, tenantID)
}

func (s *SettingsStore) nextInvoiceNumberInTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	col := "invoice_next_seq_test"
	if postgres.Livemode(ctx) {
		col = "invoice_next_seq_live"
	}
	// SQL is built from a fixed allow-list of column names above —
	// no caller-controlled interpolation.
	q := `
		INSERT INTO tenant_settings (tenant_id, ` + col + `)
		VALUES ($1, 2)
		ON CONFLICT (tenant_id) DO UPDATE
			SET ` + col + ` = tenant_settings.` + col + ` + 1
		RETURNING invoice_prefix, ` + col + ` - 1
	`

	var prefix string
	var seq int
	if err := tx.QueryRowContext(ctx, q, tenantID).Scan(&prefix, &seq); err != nil {
		return "", err
	}
	return strings.ToUpper(prefix) + "-" + padSeq(seq), nil
}

// NextCreditNoteNumberTx allocates the next credit note number in the mode
// carried on ctx, on the CALLER's transaction — the one that inserts the
// credit note. A rolled-back create (a failed cancel, a refused swap) rolls
// the number back with it, so the sequence stays gap-free, the same contract
// NextInvoiceNumberTx keeps for invoices. Same per-mode-column rationale as
// NextInvoiceNumber above.
func (s *SettingsStore) NextCreditNoteNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	col := "credit_note_next_seq_test"
	if postgres.Livemode(ctx) {
		col = "credit_note_next_seq_live"
	}
	q := `
		INSERT INTO tenant_settings (tenant_id, ` + col + `)
		VALUES ($1, 2)
		ON CONFLICT (tenant_id) DO UPDATE
			SET ` + col + ` = tenant_settings.` + col + ` + 1
		RETURNING credit_note_prefix, ` + col + ` - 1
	`

	var prefix string
	var seq int
	if err := tx.QueryRowContext(ctx, q, tenantID).Scan(&prefix, &seq); err != nil {
		return "", err
	}
	return strings.ToUpper(prefix) + "-" + padSeq(seq), nil
}

func padSeq(n int) string {
	return padInt(n, 6)
}

func padInt(n, width int) string {
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// SettingsHandler handles HTTP for tenant settings.
type SettingsHandler struct {
	store       *SettingsStore
	auditLogger SettingsAuditRecorder
}

// SettingsAuditRecorder is the narrow audit surface the settings handler
// needs — declared here so internal/tenant stays decoupled from
// internal/audit. Production wires *audit.Logger via SetAuditLogger in
// router.go. Optional: nil = NO audit row at all. There is no longer a fallback:
// the generic middleware catch-all that used to record "a PUT happened" was
// deleted (ADR-090), and its replacement is a PURE OBSERVER that reports an
// uncovered mutation rather than writing one. A nil recorder here means the
// settings change is simply not audited — which is why the boot gate
// (audit.MustWired) exists to make sure it is never nil in production.
type SettingsAuditRecorder interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

func NewSettingsHandler(store *SettingsStore) *SettingsHandler {
	return &SettingsHandler{store: store}
}

// SetAuditLogger wires the audit recorder used to log settings changes with
// field-level before/after detail. Settings carry compliance-sensitive knobs
// (default_currency, timezone, net terms, tax config) — before this, a
// settings change produced only the catch-all row with
// metadata={"path":"/v1/settings"} and no record of WHAT changed.
func (h *SettingsHandler) SetAuditLogger(a SettingsAuditRecorder) { h.auditLogger = a }

func (h *SettingsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.get)
	r.Put("/", h.upsert)
	return r
}

func (h *SettingsHandler) get(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	// Get now synthesizes defaults on miss internally — the
	// ErrNotFound branch that used to live here became unreachable.
	// Any error coming back from Get is a real DB or scan failure.
	ts, err := h.store.Get(r.Context(), tenantID)
	if err != nil {
		slog.Error("get settings", "error", err)
		respond.InternalError(w, r)
		return
	}

	respond.JSON(w, r, http.StatusOK, ts)
}

func (h *SettingsHandler) upsert(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	// MERGE semantics: decode the body OVER the tenant's current
	// settings, so a field absent from the JSON keeps its stored value
	// and an explicitly-present value (including "" / 0) sets it. The
	// old code decoded into a zero struct — a partial body silently
	// WIPED every unsent field (an SDK call updating just the tax rate
	// reset the company address, invoice prefix, and net terms, which
	// validateSettings then "helpfully" re-defaulted). Get synthesizes
	// defaults on miss, so the merge base always exists; a Get failure
	// is a real DB error and must fail the save — writing against an
	// unknown base would resurrect replace semantics.
	ts, err := h.store.Get(r.Context(), tenantID)
	if err != nil {
		slog.Error("get settings for merge", "error", err)
		respond.InternalError(w, r)
		return
	}
	// The same pre-decode snapshot is the audit diff's honest "before"
	// (the synthesized defaults for a first-ever save).
	before := ts
	haveBefore := h.auditLogger != nil

	if err := json.NewDecoder(r.Body).Decode(&ts); err != nil {
		respond.BadRequest(w, r, "invalid JSON")
		return
	}
	ts.TenantID = tenantID

	if err := validateSettings(&ts); err != nil {
		respond.FromError(w, r, err, "tenant_settings")
		return
	}

	result, err := h.store.Upsert(r.Context(), ts)
	if err != nil {
		slog.Error("upsert settings", "error", err)
		respond.InternalError(w, r)
		return
	}

	// Field-level change audit. Diff computed over the JSON field names so new
	// settings fields are covered automatically.
	//
	// A save that changed no field mutated nothing an operator could later be
	// asked about, so it records nothing — and says so (MarkSkip), rather than
	// leaving a 200-with-no-row that the audit-coverage detector must read as a
	// lost audit row. (The deleted catch-all used to record every no-op save as a
	// generic "update setting" with an empty resource_id.)
	if h.auditLogger != nil && haveBefore {
		if changed := diffSettings(before, result); len(changed) > 0 {
			// Settings are ACCOUNT-PLANE: one set per tenant, not one per mode. The
			// row is therefore filed in the canonical (test) partition rather than
			// wherever the operator's Test/Live toggle happened to be pointing —
			// otherwise the same action lands in a different partition depending on
			// an unrelated UI switch, and "who changed this?" has no reliable
			// answer. Members and auth events do the same.
			//
			// Accepted loss, stated rather than glossed: an operator viewing the
			// audit log in LIVE mode does not see these rows. Closing that needs a
			// mode-neutral read path, and the obvious shape (an OR arm on livemode)
			// is exactly what defeated every audit index before migration 0147 —
			// so it needs a column, not a predicate. Trigger: an operator who
			// cannot find a settings change they know they made.
			_ = h.auditLogger.Log(postgres.WithLivemode(r.Context(), false), tenantID, "update", "setting", "", "Settings", map[string]any{
				"action":  "settings_updated",
				"changed": changed,
			})
		} else {
			audit.MarkSkip(r.Context())
		}
	}

	respond.JSON(w, r, http.StatusOK, result)
}

// diffSettings returns {json_field: {"from": old, "to": new}} for every field
// that differs between two settings snapshots, keyed by the wire (JSON) field
// names so the audit row matches the API vocabulary and new struct fields are
// picked up without touching this code. tenant_id and the row timestamps are
// excluded — they're identity/bookkeeping, not operator-changed configuration.
func diffSettings(before, after domain.TenantSettings) map[string]any {
	norm := func(ts domain.TenantSettings) map[string]any {
		b, err := json.Marshal(ts)
		if err != nil {
			return map[string]any{}
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return map[string]any{}
		}
		delete(m, "tenant_id")
		delete(m, "created_at")
		delete(m, "updated_at")
		return m
	}
	bm, am := norm(before), norm(after)
	changed := map[string]any{}
	for k, av := range am {
		if !reflect.DeepEqual(bm[k], av) {
			changed[k] = map[string]any{"from": bm[k], "to": av}
		}
	}
	// Fields present before but omitted after (omitempty: value cleared).
	for k, bv := range bm {
		if _, ok := am[k]; !ok {
			changed[k] = map[string]any{"from": bv, "to": nil}
		}
	}
	return changed
}

// validateSettings validates + applies defaults to a settings struct, returning
// a DomainError keyed to the offending field on failure. Structured as field
// errors so respond.FromError can route each message to the right input in the
// UI.
func validateSettings(ts *domain.TenantSettings) error {
	if email := strings.TrimSpace(ts.CompanyEmail); email != "" {
		at := strings.Index(email, "@")
		if at < 1 {
			return errs.Invalid("company_email", "must contain @")
		}
		host := email[at+1:]
		if !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
			return errs.Invalid("company_email", "domain must contain a dot")
		}
	}
	if phone := strings.TrimSpace(ts.CompanyPhone); phone != "" {
		if !settingsPhonePattern.MatchString(phone) {
			return errs.Invalid("company_phone", "must be 7-20 characters and contain only digits, spaces, +, -, (, )")
		}
	}

	if ts.DefaultCurrency == "" {
		ts.DefaultCurrency = "USD"
	}
	if ts.Timezone == "" {
		ts.Timezone = "UTC"
	}
	if ts.InvoicePrefix == "" {
		ts.InvoicePrefix = "VLX"
	}
	// Net 0 (due immediately) is a legal, industry-standard term — the
	// old `<= 0 → 30` coercion made it inexpressible. Only negatives
	// are invalid.
	if ts.NetPaymentTerms < 0 {
		return errs.Invalid("net_payment_terms", "cannot be negative (0 = due immediately)")
	}
	// credit.balance_low threshold (ADR-078): must be a positive amount
	// when set. Explicit null clears it (low alerts off); 0 or negative is
	// operator confusion — depleted (balance reaches 0) is its own event.
	if ts.CreditBalanceLowThresholdCents != nil && *ts.CreditBalanceLowThresholdCents <= 0 {
		return errs.Invalid("credit_balance_low_threshold_cents",
			"must be greater than 0 when set (send null to turn low-balance alerts off; depletion has its own event)")
	}

	// Canonical UPPERCASE before persisting — ValidateCurrency accepts
	// any case, and a stored "usd" breaks every currency equality
	// filter downstream (analytics, dunning; bitten once already).
	ts.DefaultCurrency = strings.ToUpper(strings.TrimSpace(ts.DefaultCurrency))
	if err := domain.ValidateCurrency(ts.DefaultCurrency); err != nil {
		return errs.Invalid("default_currency", err.Error())
	}
	// The tenant timezone anchors ALL billing date math (ADR-058); an
	// unloadable IANA name previously saved fine and then failed at
	// cycle close, far from the operator's edit. "Local" loads but is
	// whatever the host happens to run in — not a fixed zone.
	if ts.Timezone == "Local" {
		return errs.Invalid("timezone", "must be a fixed IANA zone name (e.g. 'America/New_York', 'UTC'), not 'Local'")
	}
	if _, err := time.LoadLocation(ts.Timezone); err != nil {
		return errs.Invalid("timezone", "must be a valid IANA zone name (e.g. 'America/New_York', 'Asia/Kolkata', 'UTC')")
	}
	// The prefix flows verbatim into invoice numbers (INV filenames,
	// PDF headers, emails) — control chars, slashes, or spaces there
	// corrupt Content-Disposition filenames and number parsing.
	ts.InvoicePrefix = strings.TrimSpace(ts.InvoicePrefix)
	if !invoicePrefixPattern.MatchString(ts.InvoicePrefix) {
		return errs.Invalid("invoice_prefix", "must be 1-20 characters: letters, digits, and hyphens only")
	}
	if ts.NetPaymentTerms > 365 {
		return errs.Invalid("net_payment_terms", "cannot exceed 365 days")
	}
	if len(ts.CompanyName) > 255 {
		return errs.Invalid("company_name", "must be at most 255 characters")
	}
	// company_name flows into the From: display name of every outbound email.
	// Control characters (CR/LF in particular) have no legitimate place in a
	// company name and are the classic email-header-injection vector — reject
	// at write time so the operator gets a clear error, on top of the
	// RFC-2047 encoding the sender applies at the wire boundary.
	if strings.ContainsFunc(ts.CompanyName, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errs.Invalid("company_name", "must not contain control characters or line breaks")
	}
	if len(ts.CompanyAddressLine1) > 200 {
		return errs.Invalid("company_address_line1", "must be at most 200 characters")
	}
	if len(ts.CompanyAddressLine2) > 200 {
		return errs.Invalid("company_address_line2", "must be at most 200 characters")
	}
	if len(ts.CompanyCity) > 100 {
		return errs.Invalid("company_city", "must be at most 100 characters")
	}
	if len(ts.CompanyState) > 100 {
		return errs.Invalid("company_state", "must be at most 100 characters")
	}
	if len(ts.CompanyPostalCode) > 20 {
		return errs.Invalid("company_postal_code", "must be at most 20 characters")
	}
	ts.CompanyCountry = strings.ToUpper(strings.TrimSpace(ts.CompanyCountry))
	if ts.CompanyCountry != "" && !iso3166Alpha2.MatchString(ts.CompanyCountry) {
		return errs.Invalid("company_country", "must be an ISO-3166 alpha-2 country code (e.g. 'IN', 'US')")
	}
	if len(ts.TaxID) > 50 {
		return errs.Invalid("tax_id", "must be at most 50 characters")
	}
	if url := strings.TrimSpace(ts.SupportURL); url != "" {
		if len(url) > 500 {
			return errs.Invalid("support_url", "must be at most 500 characters")
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return errs.Invalid("support_url", "must start with http:// or https://")
		}
	}
	ts.BrandColor = strings.ToLower(strings.TrimSpace(ts.BrandColor))
	if ts.BrandColor != "" && !brandColorPattern.MatchString(ts.BrandColor) {
		return errs.Invalid("brand_color", "must be a 7-character hex like #1f6feb")
	}
	if len(ts.InvoiceFooter) > 1000 {
		return errs.Invalid("invoice_footer", "must be at most 1000 characters")
	}
	if ts.TaxProvider == "" {
		ts.TaxProvider = "manual"
	}
	switch ts.TaxProvider {
	case "none", "manual", "stripe_tax":
	default:
		return errs.Invalid("tax_provider", "must be 'none', 'manual', or 'stripe_tax'")
	}
	if ts.TaxOnFailure == "" {
		ts.TaxOnFailure = "block"
	}
	switch ts.TaxOnFailure {
	case "block", "fallback_manual":
	default:
		return errs.Invalid("tax_on_failure", "must be 'block' or 'fallback_manual'")
	}
	if ts.TaxRate < 0 || ts.TaxRate > 100 {
		return errs.Invalid("tax_rate", "must be between 0 and 100 (e.g. 18.50 for 18.5%; up to 4 decimal places). ADR-042/043.")
	}
	ts.DefaultProductTaxCode = strings.TrimSpace(ts.DefaultProductTaxCode)
	if err := domain.ValidateStripeTaxCode("default_product_tax_code", ts.DefaultProductTaxCode); err != nil {
		return err
	}
	// Seed a sensible default SaaS tax code when stripe_tax is selected and
	// the tenant has not set their own. txcd_10103001 is Stripe's
	// "Software as a Service (SaaS) – business use" classification — the
	// right starting point for the overwhelming majority of Velox tenants.
	if ts.TaxProvider == "stripe_tax" && ts.DefaultProductTaxCode == "" {
		ts.DefaultProductTaxCode = "txcd_10103001"
	}
	return nil
}

// iso3166Alpha2 matches an uppercase 2-letter country code. Full validation
// against the registered list would require a dep we don't need at tenant
// setup time — format check is sufficient to catch typos.
var iso3166Alpha2 = regexp.MustCompile(`^[A-Z]{2}$`)
