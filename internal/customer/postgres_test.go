package customer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

func TestPostgresStore_CreateAndGet(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test Tenant")

	created, err := store.Create(ctx, tenantID, domain.Customer{
		ExternalID:  "cus_ext_001",
		DisplayName: "Acme Corp",
		Email:       "billing@acme.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("ID should be generated")
	}
	if created.TenantID != tenantID {
		t.Errorf("tenant_id: got %q, want %q", created.TenantID, tenantID)
	}

	got, err := store.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExternalID != "cus_ext_001" {
		t.Errorf("external_id: got %q", got.ExternalID)
	}
	if got.Email != "billing@acme.com" {
		t.Errorf("email: got %q", got.Email)
	}
}

func TestPostgresStore_GetByStripeCustomerID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test Tenant")

	created, err := store.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_ext_stripe", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetStripeCustomerID(ctx, tenantID, created.ID, "cus_stripe_abc"); err != nil {
		t.Fatalf("set stripe id: %v", err)
	}

	got, err := store.GetByStripeCustomerID(ctx, tenantID, "cus_stripe_abc")
	if err != nil {
		t.Fatalf("get by stripe id: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("resolved wrong customer: got %q, want %q", got.ID, created.ID)
	}

	// Unknown stripe id and empty id both fail closed with NotFound.
	if _, err := store.GetByStripeCustomerID(ctx, tenantID, "cus_stripe_nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown stripe id: want NotFound, got %v", err)
	}
	if _, err := store.GetByStripeCustomerID(ctx, tenantID, ""); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("empty stripe id: want NotFound, got %v", err)
	}
}

func TestPostgresStore_UniqueExternalID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	_, _ = store.Create(ctx, tenantID, domain.Customer{ExternalID: "dup", DisplayName: "First"})

	_, err := store.Create(ctx, tenantID, domain.Customer{ExternalID: "dup", DisplayName: "Second"})
	if err == nil {
		t.Fatal("expected error for duplicate external_id")
	}
}

func TestPostgresStore_RLSIsolation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenant1 := testutil.CreateTestTenant(t, db, "Tenant 1")
	tenant2 := testutil.CreateTestTenant(t, db, "Tenant 2")

	// Create customer in tenant1
	cust, _ := store.Create(ctx, tenant1, domain.Customer{ExternalID: "shared_id", DisplayName: "Tenant1 Customer"})

	// Should be visible from tenant1
	_, err := store.Get(ctx, tenant1, cust.ID)
	if err != nil {
		t.Fatalf("tenant1 should see its own customer: %v", err)
	}

	// Should NOT be visible from tenant2 (RLS)
	_, err = store.Get(ctx, tenant2, cust.ID)
	if err != errs.ErrNotFound {
		t.Errorf("tenant2 should NOT see tenant1's customer, got: %v", err)
	}

	// List from tenant2 should be empty
	list, _, err := store.List(ctx, customer.ListFilter{TenantID: tenant2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("tenant2 list should be empty, got %d", len(list))
	}
}

func TestPostgresStore_ListWithFilters(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	_, _ = store.Create(ctx, tenantID, domain.Customer{ExternalID: "a", DisplayName: "Alpha"})
	_, _ = store.Create(ctx, tenantID, domain.Customer{ExternalID: "b", DisplayName: "Beta"})
	_, _ = store.Create(ctx, tenantID, domain.Customer{ExternalID: "c", DisplayName: "Charlie"})

	// All
	all, total, err := store.List(ctx, customer.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 3 {
		t.Errorf("total: got %d, want 3", total)
	}
	if len(all) != 3 {
		t.Errorf("items: got %d, want 3", len(all))
	}

	// By external_id
	filtered, _, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, ExternalID: "b"})
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].DisplayName != "Beta" {
		t.Errorf("filtered: expected Beta, got %v", filtered)
	}

	// Pagination
	page, _, err := store.List(ctx, customer.ListFilter{TenantID: tenantID, Limit: 2})
	if err != nil {
		t.Fatalf("list paginated: %v", err)
	}
	if len(page) != 2 {
		t.Errorf("page: got %d, want 2", len(page))
	}
}

func TestPostgresStore_BillingProfile(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	cust, _ := store.Create(ctx, tenantID, domain.Customer{ExternalID: "bp_test", DisplayName: "BP"})

	// Upsert
	bp, err := store.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID:    cust.ID,
		LegalName:     "BP Inc.",
		Country:       "US",
		Currency:      "USD",
		ProfileStatus: domain.BillingProfileReady,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if bp.LegalName != "BP Inc." {
		t.Errorf("legal_name: got %q", bp.LegalName)
	}

	// Get
	got, err := store.GetBillingProfile(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Country != "US" {
		t.Errorf("country: got %q", got.Country)
	}

	// Update (upsert again)
	bp2, err := store.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID:    cust.ID,
		LegalName:     "BP LLC",
		Country:       "CA",
		Currency:      "CAD",
		ProfileStatus: domain.BillingProfileReady,
	})
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if bp2.LegalName != "BP LLC" {
		t.Errorf("updated legal_name: got %q", bp2.LegalName)
	}
	if bp2.Country != "CA" {
		t.Errorf("updated country: got %q", bp2.Country)
	}
}

func TestPostgresStore_Update(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	cust, _ := store.Create(ctx, tenantID, domain.Customer{ExternalID: "upd", DisplayName: "Original"})

	cust.DisplayName = "Updated"
	cust.Email = "new@example.com"
	updated, err := store.Update(ctx, tenantID, cust)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DisplayName != "Updated" {
		t.Errorf("display_name: got %q", updated.DisplayName)
	}
	if updated.Email != "new@example.com" {
		t.Errorf("email: got %q", updated.Email)
	}
}

// TestService_Update_ResetsBounceStateOnEmailChange asserts the
// service-layer reset of email_status / email_bounce_reason /
// email_last_bounced_at. Without this reset, a bounced flag on the
// previous email would carry to the new one and the suppression gate
// would silently drop sends to a brand-new untested address.
//
// Goes through service.Update (not the store directly) because the
// reset logic lives in the service — it has both the prior and new
// email values in scope and the store doesn't.
//
// Three cases:
//  1. Email changes → bounce state resets to 'unknown' + NULLs.
//  2. Email doesn't change (same plaintext) → bounce state preserved.
//  3. Only display_name changes → bounce state preserved.
func TestService_Update_ResetsBounceStateOnEmailChange(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := customer.NewPostgresStore(db)
	svc := customer.NewService(store)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Bounce-reset")

	cust, _ := store.Create(ctx, tenantID, domain.Customer{
		ExternalID:  "br",
		DisplayName: "Original",
		Email:       "old@example.com",
	})

	// Mark the customer as bounced — simulates the post-5xx state
	// that ReportBounce → MarkEmailBounced writes.
	if err := store.MarkEmailBounced(ctx, tenantID, cust.ID, "550 5.1.1 user unknown"); err != nil {
		t.Fatalf("seed bounce: %v", err)
	}
	bounced, err := store.Get(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get bounced: %v", err)
	}
	if bounced.EmailStatus != domain.EmailStatusBounced {
		t.Fatalf("expected bounced status; got %q", bounced.EmailStatus)
	}
	if bounced.EmailBounceReason == "" {
		t.Fatal("expected bounce reason populated")
	}

	// Case 1: change the email via the service — bounce state should
	// reset to unknown and NULL out the reason + last-bounced timestamp.
	_, err = svc.Update(ctx, tenantID, cust.ID, customer.UpdateInput{Email: "new@example.com"})
	if err != nil {
		t.Fatalf("update with new email: %v", err)
	}
	updated, _ := store.Get(ctx, tenantID, cust.ID)
	if updated.EmailStatus != domain.EmailStatusUnknown {
		t.Errorf("expected email_status reset to unknown after email change; got %q", updated.EmailStatus)
	}
	if updated.EmailBounceReason != "" {
		t.Errorf("expected email_bounce_reason cleared; got %q", updated.EmailBounceReason)
	}
	if updated.EmailLastBouncedAt != nil {
		t.Errorf("expected email_last_bounced_at cleared; got %v", *updated.EmailLastBouncedAt)
	}

	// Case 2: re-bounce, then no-op update (same email value) — state
	// must be preserved. Edit that doesn't change the email shouldn't
	// erase real bounce signal.
	if err := store.MarkEmailBounced(ctx, tenantID, cust.ID, "550 again"); err != nil {
		t.Fatalf("re-bounce: %v", err)
	}
	_, err = svc.Update(ctx, tenantID, cust.ID, customer.UpdateInput{DisplayName: "Renamed", Email: "new@example.com"})
	if err != nil {
		t.Fatalf("update no-op-email: %v", err)
	}
	noop, _ := store.Get(ctx, tenantID, cust.ID)
	if noop.EmailStatus != domain.EmailStatusBounced {
		t.Errorf("bounce state must persist when email value unchanged; got %q", noop.EmailStatus)
	}
	if noop.EmailBounceReason == "" {
		t.Error("bounce reason must persist when email value unchanged")
	}
	if noop.DisplayName != "Renamed" {
		t.Errorf("display_name should still update; got %q", noop.DisplayName)
	}

	// Migration 0100 collapsed BP.email — customers.email is now the
	// single canonical recipient. There's no BP→customer sync left to
	// exercise here. The Case 1/2 paths (via svc.Update) already cover
	// the email-change reset behavior end-to-end.
}
