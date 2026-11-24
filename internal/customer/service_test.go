package customer

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

type memoryStore struct {
	customers       map[string]domain.Customer
	billingProfiles map[string]domain.CustomerBillingProfile
	costDashHashes  map[string]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		customers:       make(map[string]domain.Customer),
		billingProfiles: make(map[string]domain.CustomerBillingProfile),
		costDashHashes:  make(map[string]string),
	}
}

func (m *memoryStore) Create(ctx context.Context, tenantID string, c domain.Customer) (domain.Customer, error) {
	for _, existing := range m.customers {
		if existing.TenantID == tenantID && existing.ExternalID == c.ExternalID {
			return domain.Customer{}, fmt.Errorf("%w: customer with external_id %q already exists", errs.ErrAlreadyExists, c.ExternalID)
		}
	}
	c.ID = fmt.Sprintf("hc_cus_%d", len(m.customers)+1)
	c.TenantID = tenantID
	c.Status = domain.CustomerStatusActive
	// Stamped as the Postgres store stamps them, from ctx's time.
	now := clock.Now(ctx)
	c.CreatedAt, c.UpdatedAt = now, now
	m.customers[c.ID] = c
	return c, nil
}

// CreateAudited: the memory fake has no real tx, so the emission runs with a
// nil *sql.Tx — and to keep SHARED-FATE semantics faithful to the Postgres
// store, an emission error rolls the in-memory write back exactly as the
// aborted tx would. The real rollback coupling is pinned against Postgres in
// create_audit_integration_test.go.
func (m *memoryStore) CreateAudited(ctx context.Context, tenantID string, c domain.Customer, emit func(tx *sql.Tx, out domain.Customer) error) (domain.Customer, error) {
	out, err := m.Create(ctx, tenantID, c)
	if err != nil {
		return domain.Customer{}, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			delete(m.customers, out.ID) // shared fate: roll the write back
			return domain.Customer{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

func (m *memoryStore) Get(_ context.Context, tenantID, id string) (domain.Customer, error) {
	c, ok := m.customers[id]
	if !ok || c.TenantID != tenantID {
		return domain.Customer{}, errs.ErrNotFound
	}
	return c, nil
}

func (m *memoryStore) GetByExternalID(_ context.Context, tenantID, externalID string) (domain.Customer, error) {
	for _, c := range m.customers {
		if c.TenantID == tenantID && c.ExternalID == externalID {
			return c, nil
		}
	}
	return domain.Customer{}, errs.ErrNotFound
}

func (m *memoryStore) List(_ context.Context, filter ListFilter) ([]domain.Customer, int, error) {
	var result []domain.Customer
	for _, c := range m.customers {
		if c.TenantID != filter.TenantID {
			continue
		}
		if filter.Status != "" && string(c.Status) != filter.Status {
			continue
		}
		result = append(result, c)
	}
	return result, len(result), nil
}

// WithCustomerRulesLock: the fake has no locks or transactions, so fn runs
// with a nil *sql.Tx on the stored customer. The lock's ordering is tested
// against Postgres (internal/api customer_rules_lock_e2e_test.go).
func (m *memoryStore) WithCustomerRulesLock(_ context.Context, tenantID, id string, fn func(tx *sql.Tx, locked domain.Customer) error) error {
	c, ok := m.customers[id]
	if !ok || c.TenantID != tenantID {
		return errs.ErrNotFound
	}
	return fn(nil, c)
}

func (m *memoryStore) UpdateTx(_ context.Context, _ *sql.Tx, c domain.Customer, emailChanged bool) (domain.Customer, error) {
	if _, ok := m.customers[c.ID]; !ok {
		return domain.Customer{}, errs.ErrNotFound
	}
	if emailChanged {
		c.EmailStatus = domain.EmailStatusUnknown
		c.EmailLastBouncedAt = nil
		c.EmailBounceReason = ""
	}
	m.customers[c.ID] = c
	return c, nil
}

func (m *memoryStore) UpsertBillingProfileTx(_ context.Context, _ *sql.Tx, tenantID string, bp domain.CustomerBillingProfile) (domain.CustomerBillingProfile, error) {
	bp.TenantID = tenantID
	m.billingProfiles[bp.CustomerID] = bp
	return bp, nil
}

func (m *memoryStore) GetBillingProfile(_ context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error) {
	bp, ok := m.billingProfiles[customerID]
	if !ok {
		return domain.CustomerBillingProfile{}, errs.ErrNotFound
	}
	return bp, nil
}

func (m *memoryStore) SetStripeCustomerID(_ context.Context, tenantID, customerID, stripeCustomerID string) error {
	c, ok := m.customers[customerID]
	if !ok || c.TenantID != tenantID {
		return errs.ErrNotFound
	}
	c.StripeCustomerID = stripeCustomerID
	m.customers[customerID] = c
	return nil
}

func (m *memoryStore) MarkEmailBounced(_ context.Context, tenantID, customerID, reason string) error {
	c, ok := m.customers[customerID]
	if !ok || c.TenantID != tenantID {
		return errs.ErrNotFound
	}
	if c.EmailStatus == domain.EmailStatusComplained {
		// Mirror the store's monotonic guard: a bounce never
		// downgrades a complaint (benign no-op).
		return nil
	}
	now := time.Now().UTC()
	c.EmailStatus = domain.EmailStatusBounced
	c.EmailLastBouncedAt = &now
	c.EmailBounceReason = reason
	m.customers[customerID] = c
	return nil
}

func (m *memoryStore) MarkEmailComplained(_ context.Context, tenantID, customerID, reason string) error {
	c, ok := m.customers[customerID]
	if !ok || c.TenantID != tenantID {
		return errs.ErrNotFound
	}
	now := time.Now().UTC()
	c.EmailStatus = domain.EmailStatusComplained
	c.EmailLastBouncedAt = &now
	c.EmailBounceReason = reason
	m.customers[customerID] = c
	return nil
}

// costDashHashes mirrors customers.cost_dashboard_token_hash: the fake stores
// the SAME blind index the real store does, keyed by customer id, and resolves
// by hashing the presented token. Keeping the raw token here instead would let
// a regression that writes or compares plaintext still pass against the fake —
// the fake would simply agree with itself. (customer id -> sha256 hex)
func (m *memoryStore) SetCostDashboardToken(_ context.Context, tenantID, customerID, token string) error {
	c, ok := m.customers[customerID]
	if !ok || c.TenantID != tenantID {
		return errs.ErrNotFound
	}
	if m.costDashHashes == nil {
		m.costDashHashes = map[string]string{}
	}
	hash := HashCostDashboardToken(token)
	if token != "" {
		// Mirror the partial UNIQUE index: another customer holding
		// the same token is a collision, not a silent overwrite.
		for id, other := range m.costDashHashes {
			if id == customerID {
				continue
			}
			if other == hash {
				return fmt.Errorf("set cost dashboard token: collision: %s", token)
			}
		}
	}
	m.costDashHashes[customerID] = hash
	m.customers[customerID] = c
	return nil
}

func (m *memoryStore) GetByCostDashboardToken(_ context.Context, token string) (domain.Customer, error) {
	if token == "" {
		return domain.Customer{}, errs.ErrNotFound
	}
	hash := HashCostDashboardToken(token)
	for id, h := range m.costDashHashes {
		if h == hash {
			return m.customers[id], nil
		}
	}
	return domain.Customer{}, errs.ErrNotFound
}

func TestCustomerService_Create(t *testing.T) {
	svc := NewService(newMemoryStore())
	ctx := context.Background()

	t.Run("valid input", func(t *testing.T) {
		c, err := svc.Create(ctx, "tenant1", CreateInput{
			ExternalID:  "cus_123",
			DisplayName: "Acme Corp",
			Email:       "billing@acme.com",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.ExternalID != "cus_123" {
			t.Errorf("got external_id %q, want %q", c.ExternalID, "cus_123")
		}
		if c.DisplayName != "Acme Corp" {
			t.Errorf("got display_name %q, want %q", c.DisplayName, "Acme Corp")
		}
		if c.TenantID != "tenant1" {
			t.Errorf("got tenant_id %q, want %q", c.TenantID, "tenant1")
		}
	})

	t.Run("missing external_id", func(t *testing.T) {
		_, err := svc.Create(ctx, "tenant1", CreateInput{DisplayName: "Test"})
		if err == nil {
			t.Fatal("expected error for missing external_id")
		}
	})

	t.Run("missing display_name", func(t *testing.T) {
		_, err := svc.Create(ctx, "tenant1", CreateInput{ExternalID: "test"})
		if err == nil {
			t.Fatal("expected error for missing display_name")
		}
	})

	t.Run("duplicate external_id same tenant", func(t *testing.T) {
		_, err := svc.Create(ctx, "tenant1", CreateInput{
			ExternalID:  "cus_123",
			DisplayName: "Duplicate",
		})
		if !testing.Verbose() && err == nil {
			t.Fatal("expected error for duplicate external_id")
		}
	})
}

func TestCustomerService_Get(t *testing.T) {
	store := newMemoryStore()
	svc := NewService(store)
	ctx := context.Background()

	created, _ := svc.Create(ctx, "tenant1", CreateInput{
		ExternalID:  "cus_456",
		DisplayName: "Test Corp",
	})

	t.Run("found", func(t *testing.T) {
		got, err := svc.Get(ctx, "tenant1", created.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != created.ID {
			t.Errorf("got id %q, want %q", got.ID, created.ID)
		}
	})

	t.Run("wrong tenant", func(t *testing.T) {
		_, err := svc.Get(ctx, "other_tenant", created.ID)
		if err != errs.ErrNotFound {
			t.Errorf("expected ErrNotFound for wrong tenant, got %v", err)
		}
	})
}

func TestCustomerService_Update(t *testing.T) {
	store := newMemoryStore()
	svc := NewService(store)
	ctx := context.Background()

	created, _ := svc.Create(ctx, "tenant1", CreateInput{
		ExternalID:  "cus_789",
		DisplayName: "Original Name",
	})

	updated, err := svc.Update(ctx, "tenant1", created.ID, UpdateInput{
		DisplayName: "Updated Name",
		Email:       "new@example.com",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.DisplayName != "Updated Name" {
		t.Errorf("got display_name %q, want %q", updated.DisplayName, "Updated Name")
	}
	if updated.Email != "new@example.com" {
		t.Errorf("got email %q, want %q", updated.Email, "new@example.com")
	}
}

func TestCustomerService_RotateCostDashboardToken(t *testing.T) {
	store := newMemoryStore()
	svc := NewService(store)
	ctx := context.Background()

	cust, err := svc.Create(ctx, "tenant1", CreateInput{
		ExternalID: "cus_token", DisplayName: "Token Tester",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("mint + persist", func(t *testing.T) {
		token, err := svc.RotateCostDashboardToken(ctx, "tenant1", cust.ID)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if !strings.HasPrefix(token, "hc_pcd_") {
			t.Errorf("token prefix: got %q, want hc_pcd_…", token)
		}
		if len(token) != len("hc_pcd_")+64 {
			t.Errorf("token length: got %d, want %d (prefix + 64 hex)", len(token), len("hc_pcd_")+64)
		}
		// GetByCostDashboardToken resolves the same customer.
		got, err := svc.GetByCostDashboardToken(ctx, token)
		if err != nil {
			t.Fatalf("get by token: %v", err)
		}
		if got.ID != cust.ID {
			t.Errorf("resolved customer: got %q, want %q", got.ID, cust.ID)
		}
	})

	t.Run("rotation invalidates previous token", func(t *testing.T) {
		first, err := svc.RotateCostDashboardToken(ctx, "tenant1", cust.ID)
		if err != nil {
			t.Fatalf("rotate first: %v", err)
		}
		second, err := svc.RotateCostDashboardToken(ctx, "tenant1", cust.ID)
		if err != nil {
			t.Fatalf("rotate second: %v", err)
		}
		if first == second {
			t.Fatal("two rotations produced the same token")
		}
		if _, err := svc.GetByCostDashboardToken(ctx, first); err == nil {
			t.Error("first token still resolves after rotation — should be invalidated")
		}
		if _, err := svc.GetByCostDashboardToken(ctx, second); err != nil {
			t.Errorf("second token doesn't resolve: %v", err)
		}
	})

	t.Run("missing customer", func(t *testing.T) {
		_, err := svc.RotateCostDashboardToken(ctx, "tenant1", "hc_cus_nonexistent")
		if err == nil {
			t.Fatal("expected error for missing customer")
		}
	})
}

func TestCustomerService_BillingProfile(t *testing.T) {
	store := newMemoryStore()
	svc := NewService(store)
	// No subscriptions: the profile currency guard has nothing to refuse.
	svc.SetSubscriptionChecker(&fakeSubChecker{})
	ctx := context.Background()

	created, _ := svc.Create(ctx, "tenant1", CreateInput{
		ExternalID:  "cus_bp",
		DisplayName: "BP Test",
	})

	t.Run("upsert and get", func(t *testing.T) {
		bp, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{
			CustomerID: created.ID,
			LegalName:  "Acme Inc.",
			Country:    "US",
			Currency:   "USD",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if bp.LegalName != "Acme Inc." {
			t.Errorf("got legal_name %q, want %q", bp.LegalName, "Acme Inc.")
		}

		got, err := svc.GetBillingProfile(ctx, "tenant1", created.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Country != "US" {
			t.Errorf("got country %q, want %q", got.Country, "US")
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := svc.GetBillingProfile(ctx, "tenant1", "nonexistent")
		if err != errs.ErrNotFound {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("missing customer_id", func(t *testing.T) {
		_, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{})
		if err == nil {
			t.Fatal("expected error for missing customer_id")
		}
	})

	// Non-standard tax statuses must carry the data their invoice legend
	// legally requires — enforced server-side, not just in the dashboard.
	t.Run("exempt requires a reason", func(t *testing.T) {
		_, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{
			CustomerID: created.ID, Country: "US", TaxStatus: domain.TaxStatusExempt,
		})
		if err == nil {
			t.Fatal("expected error: exempt with no tax_exempt_reason")
		}
		bp, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{
			CustomerID: created.ID, Country: "US", TaxStatus: domain.TaxStatusExempt,
			TaxExemptReason: "Reseller certificate",
		})
		if err != nil {
			t.Fatalf("exempt + reason should save: %v", err)
		}
		if bp.TaxExemptReason != "Reseller certificate" {
			t.Errorf("tax_exempt_reason = %q, want preserved", bp.TaxExemptReason)
		}
	})

	t.Run("reverse_charge requires a buyer tax id", func(t *testing.T) {
		_, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{
			CustomerID: created.ID, Country: "DE", TaxStatus: domain.TaxStatusReverseCharge,
		})
		if err == nil {
			t.Fatal("expected error: reverse_charge with no tax_id")
		}
		bp, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{
			CustomerID: created.ID, Country: "DE", TaxStatus: domain.TaxStatusReverseCharge,
			TaxIDType: "eu_vat", TaxID: "DE123456789",
		})
		if err != nil {
			t.Fatalf("reverse_charge + tax_id should save: %v", err)
		}
		if bp.TaxStatus != domain.TaxStatusReverseCharge {
			t.Errorf("tax_status = %q, want reverse_charge", bp.TaxStatus)
		}
	})

	// Country must be ISO-3166 alpha-2 — a bad code is rejected at the API
	// boundary instead of detonating as an opaque Stripe rejection later, and a
	// lowercase/padded code normalizes rather than rejects.
	t.Run("country must be ISO alpha-2", func(t *testing.T) {
		_, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{
			CustomerID: created.ID, Country: "USA",
		})
		if err == nil {
			t.Fatal("expected error: non-alpha-2 country 'USA'")
		}
		bp, err := svc.UpsertBillingProfile(ctx, "tenant1", domain.CustomerBillingProfile{
			CustomerID: created.ID, Country: " us ",
		})
		if err != nil {
			t.Fatalf("lowercase/padded country should normalize, not error: %v", err)
		}
		if bp.Country != "US" {
			t.Errorf("country = %q, want normalized %q", bp.Country, "US")
		}
	})
}

// instantCapturingAudit records the instant the emission runs on.
type instantCapturingAudit struct{ at time.Time }

func (a *instantCapturingAudit) LogInTx(ctx context.Context, _ *sql.Tx, _ audit.Entry) error {
	a.at, _ = clock.EffectiveNow(ctx)
	return nil
}

// A create runs on the instant its ctx carries (the API's door bound it):
// the row and its audit row both carry it.
func TestCreate_RunsOnTheBoundInstant(t *testing.T) {
	at := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	svc := NewService(newMemoryStore())
	spy := &instantCapturingAudit{}
	svc.SetAuditLogger(spy)
	created, err := svc.Create(clock.WithEffectiveNow(context.Background(), at), "tenant1", CreateInput{ExternalID: "cus_one", DisplayName: "One"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !spy.at.Equal(at) || !created.CreatedAt.Equal(at) || !created.UpdatedAt.Equal(at) {
		t.Errorf("row created %v, updated %v, audit row %v; want all at %v", created.CreatedAt, created.UpdatedAt, spy.at, at)
	}
}
