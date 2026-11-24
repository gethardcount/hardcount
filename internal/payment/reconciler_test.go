package payment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type mockReconcileStore struct {
	unknowns []domain.Invoice
	byID     map[string]*domain.Invoice
	// getResult, when set for an id, overrides Get's return — used to simulate
	// a webhook winning the race during the GetPaymentIntent round-trip (the
	// sweep listed a stale snapshot; Get re-reads the now-settled invoice).
	getResult map[string]domain.Invoice
}

// The stale-hold sweep's store methods: these unit tests hold no stale
// holds (the sweep is pinned on real Postgres, stale_hold_integration_test.go).
func (m *mockReconcileStore) ListStaleChargeHolds(context.Context, int, []string) ([]domain.StaleChargeHold, error) {
	return nil, nil
}

func (m *mockReconcileStore) ClearStaleChargeHold(context.Context, string, string, time.Time, int64, bool) (bool, error) {
	return false, nil
}

func (m *mockReconcileStore) MarkStaleHoldChecked(context.Context, string, string) error { return nil }

func (m *mockReconcileStore) EndAttemptWithoutPayment(context.Context, string, string, string, string) (domain.InvoicePaymentStatus, bool, error) {
	return "", false, nil
}

func (m *mockReconcileStore) StampChargeOutcome(_ context.Context, _, id string, _ int64, _ domain.InvoicePaymentStatus, _, _ string) (domain.Invoice, bool, error) {
	if inv, ok := m.byID[id]; ok {
		return *inv, false, nil
	}
	return domain.Invoice{}, false, errs.ErrNotFound
}

func newMockReconcileStore(unknowns ...domain.Invoice) *mockReconcileStore {
	byID := make(map[string]*domain.Invoice, len(unknowns))
	for i := range unknowns {
		byID[unknowns[i].ID] = &unknowns[i]
	}
	return &mockReconcileStore{unknowns: unknowns, byID: byID}
}

func (m *mockReconcileStore) ListUnknownPayments(_ context.Context, _ time.Time, _ int) ([]domain.Invoice, error) {
	var out []domain.Invoice
	for _, inv := range m.unknowns {
		if inv.PaymentStatus == domain.PaymentUnknown {
			out = append(out, inv)
		}
	}
	return out, nil
}

// RecordProviderSync MUTATES the stored invoice, matching the real store's
// narrow UPDATE rather than swallowing the call. A no-op fake here would let a
// test assert "the reconciler recorded the observation" against a double that
// records nothing — the drift class that gave an earlier fix zero executing
// coverage. It deliberately leaves ChargeAttemptSeq and UpdatedAt alone,
// exactly as the SQL does, so a regression that starts bumping either is
// visible here too.
func (m *mockReconcileStore) RecordProviderSync(_ context.Context, _, id, providerStatus string) error {
	inv, ok := m.byID[id]
	if !ok {
		return errs.ErrNotFound
	}
	inv.ProviderPaymentStatus = providerStatus
	now := time.Now().UTC()
	inv.ProviderSyncedAt = &now
	return nil
}

func (m *mockReconcileStore) ListProcessingPayments(_ context.Context, _ time.Time, _ int) ([]domain.Invoice, error) {
	var out []domain.Invoice
	for _, inv := range m.unknowns {
		if inv.PaymentStatus == domain.PaymentProcessing {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (m *mockReconcileStore) Get(_ context.Context, _, id string) (domain.Invoice, error) {
	if m.getResult != nil {
		if inv, ok := m.getResult[id]; ok {
			return inv, nil
		}
	}
	inv, ok := m.byID[id]
	if !ok {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return *inv, nil
}

func TestReconciler_SucceededResolvesUnknown(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_1",
	})
	client := &mockStripeClient{
		piStates: map[string]PaymentIntentResult{
			"pi_ambig_1": {ID: "pi_ambig_1", Status: "succeeded"},
		},
	}
	settler := &recordingSettler{}
	r := NewReconciler(client, store, settler, time.Second)

	resolved, errs := r.Run(postgres.WithLivemode(context.Background(), true), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1", resolved)
	}
	if len(settler.succeeded) != 1 || settler.succeeded[0] != (settledCall{invoiceID: "inv_1", piID: "pi_ambig_1"}) || len(settler.failed) != 0 {
		t.Errorf("settled %+v paid, %+v failed; want inv_1 paid by pi_ambig_1 through the settler", settler.succeeded, settler.failed)
	}
}

func TestReconciler_CanceledMarksFailed(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_2", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_2",
	})
	client := &mockStripeClient{
		piStates: map[string]PaymentIntentResult{
			"pi_ambig_2": {ID: "pi_ambig_2", Status: "canceled", Charged: true},
		},
	}
	settler := &recordingSettler{}
	r := NewReconciler(client, store, settler, time.Second)

	resolved, errs := r.Run(postgres.WithLivemode(context.Background(), true), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 1 {
		t.Errorf("resolved: got %d, want 1", resolved)
	}
	if len(settler.failed) != 1 || settler.failed[0].invoiceID != "inv_2" || settler.failed[0].piID != "pi_ambig_2" || len(settler.succeeded) != 0 {
		t.Errorf("settled %+v failed, %+v paid; want inv_2 failed on pi_ambig_2 through the settler", settler.failed, settler.succeeded)
	}
}

func TestReconciler_StillInFlightSkipped(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_3", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_3",
	})
	client := &mockStripeClient{
		piStates: map[string]PaymentIntentResult{
			"pi_ambig_3": {ID: "pi_ambig_3", Status: "processing"},
		},
	}
	r := NewReconciler(client, store, &recordingSettler{}, time.Second)

	resolved, errs := r.Run(postgres.WithLivemode(context.Background(), true), 10)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if resolved != 0 {
		t.Errorf("resolved: got %d, want 0 (still in flight)", resolved)
	}
	if got := store.byID["inv_3"].PaymentStatus; got != domain.PaymentUnknown {
		t.Errorf("payment_status: got %q, want unknown (unchanged)", got)
	}
}

type errStripeClient struct{ mockStripeClient }

func (e *errStripeClient) GetPaymentIntent(_ context.Context, _ string) (PaymentIntentResult, error) {
	return PaymentIntentResult{}, fmt.Errorf("stripe 502 on reconcile")
}

func TestReconciler_StripeErrorLeavesUnknown(t *testing.T) {
	// Stripe 5xx on the reconcile query itself — we must not flip state.
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_5", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_ambig_5",
	})
	r := NewReconciler(&errStripeClient{}, store, &recordingSettler{}, time.Second)

	resolved, errs := r.Run(context.Background(), 10)
	if resolved != 0 {
		t.Errorf("resolved: got %d, want 0", resolved)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 per-invoice error, got %d", len(errs))
	}
	if got := store.byID["inv_5"].PaymentStatus; got != domain.PaymentUnknown {
		t.Errorf("payment_status: got %q, want unknown (unchanged on Stripe error)", got)
	}
}
