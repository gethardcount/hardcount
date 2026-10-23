package invoice

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestMapInvoiceUniqueViolation_RoutesByConstraint locks in the
// constraint-name routing that replaced the pre-2026-05-28 generic
// AlreadyExists mapping. Every known invoice unique index must route
// to its own DomainError code so the proration retry path can
// distinguish proration-source collisions (idempotent replay) from
// billing-period collisions (distinct bug). See ADR-030 cross-flow
// audit + feedback_no_silent_fallbacks memory.
func TestMapInvoiceUniqueViolation_RoutesByConstraint(t *testing.T) {
	cases := []struct {
		name           string
		constraintName string
		wantField      string
		wantCode       string
	}{
		{"billing idempotency → billing_period", idxInvoicesBillingIdempotency, "billing_period", "invoice_billing_period_taken"},
		{"proration dedup → proration_source", idxInvoicesProrationDedup, "proration_source", "invoice_proration_source_taken"},
		{"invoice number → invoice_number", idxInvoicesInvoiceNumberUnique, "invoice_number", "invoice_number_taken"},
		{"public token → public_token", idxInvoicesPublicTokenUnique, "public_token", "invoice_public_token_collision"},
		{"threshold cycle → threshold_cycle", idxInvoicesThresholdPerCycle, "threshold_cycle", "invoice_threshold_cycle_taken"},
		{"stripe invoice id → stripe_invoice_id", idxInvoicesStripeInvoiceID, "stripe_invoice_id", "invoice_stripe_id_taken"},
		{"unknown constraint → generic", "some_future_index", "", "invoice_unique_violation"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pgErr := &pgconn.PgError{
				Code:           "23505",
				ConstraintName: tc.constraintName,
			}
			mapped := mapInvoiceUniqueViolation(pgErr, domain.Invoice{
				InvoiceNumber:  "INV-001",
				SubscriptionID: "sub_x",
			})
			if mapped == nil {
				t.Fatalf("expected mapped error, got nil")
			}
			if !errors.Is(mapped, errs.ErrAlreadyExists) {
				t.Errorf("expected ErrAlreadyExists wrapping, got: %v", mapped)
			}
			if got := errs.Field(mapped); got != tc.wantField {
				t.Errorf("field: got %q, want %q", got, tc.wantField)
			}
			if got := errs.Code(mapped); got != tc.wantCode {
				t.Errorf("code: got %q, want %q", got, tc.wantCode)
			}
		})
	}
}

func TestMapInvoiceUniqueViolation_NonUniqueErrorPassesThrough(t *testing.T) {
	// FK violations, NOT NULL violations, anything else: mapper returns
	// nil and caller surfaces the original err. Locks in the
	// "only-rewrites-unique-violations" contract — pre-fix had no
	// equivalent guard since the call site was inline; the helper now
	// owns the predicate.
	pgErr := &pgconn.PgError{Code: "23503"} // foreign_key_violation
	if mapped := mapInvoiceUniqueViolation(pgErr, domain.Invoice{}); mapped != nil {
		t.Errorf("expected nil for non-unique-violation, got %v", mapped)
	}
	if mapped := mapInvoiceUniqueViolation(errors.New("some other error"), domain.Invoice{}); mapped != nil {
		t.Errorf("expected nil for non-pg error, got %v", mapped)
	}
}

// The billing-period conflict names the period the way every other surface
// shows it: inclusive last day in the invoice's billing timezone. Pre-fix it
// printed the stored exclusive end ("2026-11-01..2026-12-01") for a period the
// dashboard, PDF and hosted page show as "Nov 1 – Nov 30".
func TestMapInvoiceUniqueViolation_BillingPeriodMessageIsInclusive(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: idxInvoicesBillingIdempotency}
	cases := []struct {
		name string
		inv  domain.Invoice
		want string
	}{
		{
			name: "New York period, stamped zone",
			inv: domain.Invoice{
				SubscriptionID:     "sub_x",
				BillingTimezone:    "America/New_York",
				BillingPeriodStart: time.Date(2026, 11, 1, 0, 0, 0, 0, ny),
				BillingPeriodEnd:   time.Date(2026, 12, 1, 0, 0, 0, 0, ny),
			},
			want: `invoice already exists for subscription "sub_x" period Nov 1, 2026 – Nov 30, 2026`,
		},
		{
			name: "legacy row without a zone falls back to UTC",
			inv: domain.Invoice{
				SubscriptionID:     "sub_x",
				BillingPeriodStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
				BillingPeriodEnd:   time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			},
			want: `invoice already exists for subscription "sub_x" period Nov 1, 2026 – Nov 30, 2026`,
		},
		{
			name: "zero-length period names the single day",
			inv: domain.Invoice{
				SubscriptionID:     "sub_x",
				BillingTimezone:    "America/New_York",
				BillingPeriodStart: time.Date(2026, 11, 1, 15, 0, 0, 0, ny),
				BillingPeriodEnd:   time.Date(2026, 11, 1, 15, 0, 0, 0, ny),
			},
			want: `invoice already exists for subscription "sub_x" period Nov 1, 2026`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapped := mapInvoiceUniqueViolation(pgErr, tc.inv)
			var de *errs.DomainError
			if !errors.As(mapped, &de) {
				t.Fatalf("expected *errs.DomainError, got %T: %v", mapped, mapped)
			}
			if de.Message != tc.want {
				t.Errorf("message:\n got  %q\n want %q", de.Message, tc.want)
			}
		})
	}
}
