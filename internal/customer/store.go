package customer

import (
	"context"
	"database/sql"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type Store interface {
	Create(ctx context.Context, tenantID string, c domain.Customer) (domain.Customer, error)
	// CreateAudited runs the caller-supplied audit emission on the SAME
	// transaction as the customer INSERT (ADR-090 in-tx emission): the
	// customer row and its audit row commit or roll back together. The
	// service builds the emission (it owns audit-row content); the store
	// owns the transaction and exposes it. emit receives the persisted
	// customer so the row can reference the store-assigned id. nil emit =
	// unaudited write (narrow tests, internal callers).
	CreateAudited(ctx context.Context, tenantID string, c domain.Customer, emit func(tx *sql.Tx, out domain.Customer) error) (domain.Customer, error)
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
	GetByExternalID(ctx context.Context, tenantID, externalID string) (domain.Customer, error)
	List(ctx context.Context, filter ListFilter) ([]domain.Customer, int, error)
	// ListByTestClockID returns one page of the customers pinned to the
	// given test clock, with PII fields decrypted, and their total. Used
	// by the testclock domain via a narrow CustomerReader interface so
	// that domain never reads the customers table directly — keeps
	// decryption centralised on the customer-package read path.
	ListByTestClockID(ctx context.Context, tenantID, clockID string, limit, offset int) ([]domain.Customer, int, error)
	Update(ctx context.Context, tenantID string, c domain.Customer) (domain.Customer, error)

	UpsertBillingProfile(ctx context.Context, tenantID string, bp domain.CustomerBillingProfile) (domain.CustomerBillingProfile, error)
	GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error)

	// SetStripeCustomerID writes the canonical Stripe Customer mapping
	// onto the customers row (migration 0096). Replaces the older
	// UpsertPaymentSetup-on-store path that doubled as a 1:1 summary
	// cache. Card details and default-PM flag now live on
	// payment_methods rows, written by paymentmethods.Service.
	SetStripeCustomerID(ctx context.Context, tenantID, customerID, stripeCustomerID string) error

	// MarkEmailBounced records a permanent delivery failure for a customer.
	// Sender calls this via a narrow interface when SMTP returns a 5xx; the
	// Postmark webhook reuses the same path for async hard bounces
	// (ADR-098). Idempotent — repeated calls just refresh the timestamp.
	// Never downgrades an existing 'complained' (benign no-op, not an error).
	MarkEmailBounced(ctx context.Context, tenantID, customerID, reason string) error

	// MarkEmailComplained records a spam complaint — the most severe
	// recipient state, written only by the provider's SpamComplaint
	// webhook (ADR-098). Idempotent; outranks and is never replaced by
	// a later bounce.
	MarkEmailComplained(ctx context.Context, tenantID, customerID, reason string) error

	// ResetEmailStatus clears any prior bounce/complain flag on the
	// customer — called by the service layer when the email value
	// changes (operator edit, portal self-edit, billing-profile email
	// change). Without this reset, a bounced flag on the OLD address
	// silently suppresses sends to a brand-new untested address via
	// the email.RecipientSuppressionChecker gate. Idempotent.
	ResetEmailStatus(ctx context.Context, tenantID, customerID string) error

	// SetCostDashboardToken writes (or rotates) the public cost-dashboard
	// token on a customer row. Old tokens are discarded immediately —
	// the public route validates against this single column, so the
	// previous URL stops working as soon as the UPDATE commits.
	SetCostDashboardToken(ctx context.Context, tenantID, customerID, token string) error

	// GetByCostDashboardToken resolves the customer behind a public
	// cost-dashboard token. RLS-bypass query (the token IS the
	// credential — no tenant context yet); the returned customer's
	// tenant_id scopes everything that follows.
	GetByCostDashboardToken(ctx context.Context, token string) (domain.Customer, error)
}

type ListFilter struct {
	TenantID   string
	Status     string
	ExternalID string
	// Search filters by display_name, email, external_id, or id —
	// case-insensitive substring. display_name and email are encrypted
	// at rest, so the store CANNOT push this into SQL ILIKE; it streams
	// a bounded scan, decrypts, and matches in Go (see listSearch).
	// Empty = no filter. Backs the dashboard search box and the
	// command palette.
	Search string
	// IDs scopes the result to a specific set of customer IDs. Used by
	// other list pages (Invoices, Subscriptions, etc.) to fetch
	// exactly the customers referenced by their primary rows, avoiding
	// the "list-then-client-side-join" anti-pattern that surfaces
	// "Unknown" rows when a referenced customer falls off the default
	// 50-row page of an unrelated list. Empty = no ID filter applied.
	IDs     []string
	Limit   int
	Offset  int
	Sort    string // closed allow-list (validated in store); empty defaults to created_at
	SortDir string // "asc" or "desc"; empty defaults to desc
}
