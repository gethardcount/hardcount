package dunning_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// countingStopper is the Stripe leg of a void, counted per invoice.
type countingStopper struct{ stopped []string }

func (c *countingStopper) StopCollection(_ context.Context, _ string, inv domain.Invoice) {
	c.stopped = append(c.stopped, inv.ID)
}

// recordingEvents records the outbound webhook events dunning sends.
type recordingEvents struct{ types []string }

func (r *recordingEvents) Dispatch(_ context.Context, _, eventType string, _ map[string]any) error {
	r.types = append(r.types, eventType)
	return nil
}

func (r *recordingEvents) count(eventType string) int {
	n := 0
	for _, t := range r.types {
		if t == eventType {
			n++
		}
	}
	return n
}

// An operator resolves a dunning run as invoice_voided. The dunning handler
// voids through the real invoice service, whose post-commit legs stop
// collection at Stripe and close the run (one void path, ADR-118 amendment
// 2026-10-22). The handler's own ResolveRun then loses the resolve CAS and
// answers with the run the void resolved: one resolved event, one
// dunning.resolved, one StopCollection.
//
// A void the invoice service refuses (a charge holds the invoice) stops
// nothing and leaves the run open: the invoice is still collectible.
func TestResolveRun_InvoiceVoided_OneVoidPathStopsAndResolvesOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Resolve Void One Path")

	dstore := dunning.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)

	type rig struct {
		inv     domain.Invoice
		run     domain.InvoiceDunningRun
		stopper *countingStopper
		events  *recordingEvents
		router  chi.Router
	}
	newRig := func(t *testing.T, tag string) rig {
		t.Helper()
		cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
			ExternalID: "cus_resolve_void_" + tag, DisplayName: "Resolve Void " + tag,
		})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		events := &recordingEvents{}
		dsvc := dunning.NewService(dstore, nil, clock.Real())
		dsvc.SetEventDispatcher(events)
		policy, err := dsvc.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
			Name: "resolve-void-" + tag, Enabled: true, RetrySchedule: []string{"72h", "72h"}, MaxRetryAttempts: 3,
			FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
		})
		if err != nil {
			t.Fatalf("upsert policy: %v", err)
		}
		if err := dsvc.SetDefaultPolicy(ctx, tenantID, policy.ID); err != nil {
			t.Fatalf("set default policy: %v", err)
		}

		stopper := &countingStopper{}
		invSvc := invoice.NewService(invStore, clock.Real(), tenant.NewSettingsStore(db))
		invSvc.SetCollectionStopper(stopper)
		invSvc.SetDunningResolver(dsvc)

		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: "INV-RESOLVE-VOID-" + tag,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour), BillingPeriodEnd: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		run, err := dsvc.StartDunning(ctx, tenantID, inv.ID, cust.ID, time.Time{}, domain.DunningCausePaymentFailed)
		if err != nil || run.State != domain.DunningActive {
			t.Fatalf("start dunning: state %s, %v", run.State, err)
		}
		events.types = nil // StartDunning's own event is not under test

		h := dunning.NewHandler(dsvc)
		h.SetInvoiceVoider(invSvc)
		router := chi.NewRouter()
		router.Mount("/v1/dunning", h.Routes())
		return rig{inv: inv, run: run, stopper: stopper, events: events, router: router}
	}
	resolve := func(r rig) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/dunning/runs/"+r.run.ID+"/resolve",
			strings.NewReader(`{"resolution":"invoice_voided"}`))
		req = req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID))
		rec := httptest.NewRecorder()
		r.router.ServeHTTP(rec, req)
		return rec
	}
	resolvedEvents := func(t *testing.T, runID string) int {
		t.Helper()
		evs, err := dstore.ListEvents(ctx, tenantID, runID)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		n := 0
		for _, e := range evs {
			if e.EventType == domain.DunningEventResolved {
				n++
			}
		}
		return n
	}

	t.Run("void succeeds: one stop, one resolve, 200 with the resolved run", func(t *testing.T) {
		r := newRig(t, "OK")
		rec := resolve(r)
		if rec.Code != http.StatusOK {
			t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
		}
		var body domain.InvoiceDunningRun
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.State != domain.DunningResolved || body.Resolution != domain.ResolutionInvoiceVoided {
			t.Errorf("response run state=%s resolution=%q, want resolved/invoice_voided", body.State, body.Resolution)
		}
		if got, err := invStore.Get(ctx, tenantID, r.inv.ID); err != nil || got.Status != domain.InvoiceVoided {
			t.Fatalf("invoice %s (%v), want voided", got.Status, err)
		}
		if len(r.stopper.stopped) != 1 || r.stopper.stopped[0] != r.inv.ID {
			t.Errorf("StopCollection calls = %v, want exactly one for %s", r.stopper.stopped, r.inv.ID)
		}
		run, err := dstore.GetRun(ctx, tenantID, r.run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionInvoiceVoided {
			t.Errorf("run state=%s resolution=%q, want resolved/invoice_voided", run.State, run.Resolution)
		}
		if n := resolvedEvents(t, r.run.ID); n != 1 {
			t.Errorf("resolved timeline events = %d, want 1 (the void's resolve and the handler's collide on the CAS)", n)
		}
		if n := r.events.count(domain.EventDunningResolved); n != 1 {
			t.Errorf("dunning.resolved events = %d, want 1", n)
		}
	})

	t.Run("void refused: nothing stopped, run stays open", func(t *testing.T) {
		r := newRig(t, "HELD")
		// A dunning retry's charge between its claim and its outcome.
		if h, err := invStore.ClaimChargeForManualCollect(ctx, tenantID, r.inv.ID); err != nil || !h.Held() {
			t.Fatalf("claim: held=%v %v", h.Held(), err)
		}
		if rec := resolve(r); rec.Code != http.StatusConflict {
			t.Fatalf("resolve while a charge holds the invoice: %d %s, want 409", rec.Code, rec.Body.String())
		}
		if len(r.stopper.stopped) != 0 {
			t.Errorf("StopCollection ran on a refused void: %v", r.stopper.stopped)
		}
		run, err := dstore.GetRun(ctx, tenantID, r.run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.State != domain.DunningActive {
			t.Errorf("run state=%s, want active: the invoice is still collectible", run.State)
		}
		if n := r.events.count(domain.EventDunningResolved); n != 0 {
			t.Errorf("dunning.resolved events = %d, want 0", n)
		}
	})
}
