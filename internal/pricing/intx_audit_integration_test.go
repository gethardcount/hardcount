package pricing_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// landThenFailEmitter writes the REAL audit row on the caller's tx and then
// errors. Satisfies pricing.AuditEmitter. Landing the row before failing is
// what makes the rollback assertion meaningful: it proves the audit INSERT is
// rolled back WITH the business write, not merely that nothing was attempted.
type landThenFailEmitter struct {
	logger *audit.Logger
	calls  int
}

func (e *landThenFailEmitter) LogInTx(ctx context.Context, tx *sql.Tx, entry audit.Entry) error {
	e.calls++
	if err := e.logger.LogInTx(ctx, tx, entry); err != nil {
		return err
	}
	return errors.New("injected audit failure")
}

// countingEmitter records how many times an emission was attempted without
// writing anything — the instrument for the "must emit NOTHING" gates.
type countingEmitter struct{ calls int }

func (e *countingEmitter) LogInTx(_ context.Context, _ *sql.Tx, _ audit.Entry) error {
	e.calls++
	return nil
}

type pricingFixture struct {
	db       *postgres.DB
	store    *pricing.PostgresStore
	logger   *audit.Logger
	tenantID string
	ctx      context.Context
}

func newPricingFixture(t *testing.T, name string) pricingFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	t.Cleanup(cancel)
	return pricingFixture{
		db:       db,
		store:    pricing.NewPostgresStore(db),
		logger:   audit.NewLogger(db),
		tenantID: testutil.CreateTestTenant(t, db, name),
		ctx:      ctx,
	}
}

// auditedSvc returns a service wired to the real in-tx logger.
func (f pricingFixture) auditedSvc() *pricing.Service {
	svc := pricing.NewService(f.store)
	svc.SetAuditLogger(f.logger)
	return svc
}

func (f pricingFixture) rows(t *testing.T, resourceType, resourceID string) []domain.AuditEntry {
	t.Helper()
	rows, _, err := f.logger.Query(f.ctx, f.tenantID, audit.QueryFilter{
		ResourceType: resourceType, ResourceID: resourceID,
	})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return rows
}

// seedPlan is a plan with one per_unit price on a fresh meter.
func (f pricingFixture) seedPlan(t *testing.T, code string) (domain.Plan, domain.PlanPrice) {
	t.Helper()
	m := f.seedMeter(t, code+"_meter")
	plan, prices, err := pricing.NewService(f.store).CreatePlan(f.ctx, f.tenantID, pricing.CreatePlanInput{
		Code: code, Name: code, Currency: "USD", BillingInterval: domain.BillingMonthly,
		Prices: []pricing.PriceInput{{MeterID: m.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(2)}}},
	})
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	return plan, prices[0]
}

func (f pricingFixture) seedMeter(t *testing.T, key string) domain.Meter {
	t.Helper()
	m, err := pricing.NewService(f.store).CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
		EventName: key, Name: "Before Rename", Unit: "unit", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("seed meter: %v", err)
	}
	return m
}

// TestMeterUpdateAudit_SharedFate pins the ADR-090 emission on
// PATCH /v1/meters/{id} (pricing.Service.UpdateMeter →
// PostgresStore.UpdateMeterAudited). The route had NO emission of its own —
// only the deleted HTTP catch-all — yet it changes how the meter counts its
// usage.
//
// Directions pinned: (1) success commits patch + exactly one update row with
// the changed fields; (2) a zero-row UPDATE (meter that doesn't exist) emits
// NOTHING; (3) a failed emission rolls the patch back.
func TestMeterUpdateAudit_SharedFate(t *testing.T) {
	f := newPricingFixture(t, "Meter Update InTx Audit")

	t.Run("patch commits with exactly one update row carrying the changed fields", func(t *testing.T) {
		m := f.seedMeter(t, "tokens_ok")
		newName := "Renamed Meter"
		agg := "max"

		out, err := f.auditedSvc().UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{
			Name:        &newName,
			Aggregation: &agg,
		})
		if err != nil {
			t.Fatalf("update meter: %v", err)
		}

		rows := f.rows(t, "meter", m.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one meter audit row; got %d: %+v", len(rows), rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionUpdate || row.ResourceType != "meter" {
			t.Errorf("vocabulary: got %s/%s, want update/meter", row.Action, row.ResourceType)
		}
		if row.ResourceLabel != newName {
			t.Errorf("resource_label = %q, want %q", row.ResourceLabel, newName)
		}
		if row.Metadata["name"] != newName {
			t.Errorf("metadata name = %v, want %q", row.Metadata["name"], newName)
		}
		if row.Metadata["aggregation"] != "max" {
			t.Errorf("metadata aggregation = %v, want max", row.Metadata["aggregation"])
		}
		if row.Metadata["event_name"] != "tokens_ok" {
			t.Errorf("metadata event_name = %v, want tokens_ok", row.Metadata["event_name"])
		}
		// Untouched field must NOT be claimed as changed.
		if _, ok := row.Metadata["unit"]; ok {
			t.Errorf("metadata claims a unit change the request never made: %+v", row.Metadata)
		}

		// The patch really landed (shared fate, success side).
		got, err := f.store.GetMeter(f.ctx, f.tenantID, m.ID)
		if err != nil {
			t.Fatalf("get meter: %v", err)
		}
		if got.Name != newName || got.Aggregation != "max" {
			t.Errorf("meter did not commit with its audit row: %+v", got)
		}
		_ = out
	})

	t.Run("nonexistent meter emits nothing", func(t *testing.T) {
		ghostID := postgres.NewID("hc_mtr") // never inserted
		emitter := &countingEmitter{}

		// Straight at the store: the service's pre-flight GetMeter would 404
		// first, so this is the only way to exercise the zero-row UPDATE gate
		// that keeps the log from asserting a change to a meter that was
		// never touched (RLS/livemode-invisible rows reach exactly here).
		_, err := f.store.UpdateMeterAudited(f.ctx, f.tenantID, domain.Meter{
			ID: ghostID, Name: "Ghost", Unit: "unit", Aggregation: "sum",
		}, nil, func(tx *sql.Tx, out domain.Meter) error {
			return emitter.LogInTx(f.ctx, tx, audit.Entry{})
		})
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("zero-row UPDATE must return ErrNotFound; got %v", err)
		}
		if emitter.calls != 0 {
			t.Fatalf("emit fired %d time(s) for a meter that does not exist — FABRICATED EVIDENCE", emitter.calls)
		}
		if rows := f.rows(t, "meter", ghostID); len(rows) != 0 {
			t.Errorf("audit rows recorded against a nonexistent meter: %+v", rows)
		}
	})

	t.Run("audit failure rolls the patch back", func(t *testing.T) {
		m := f.seedMeter(t, "tokens_rollback")
		emitter := &landThenFailEmitter{logger: f.logger}
		svc := pricing.NewService(f.store)
		svc.SetAuditLogger(emitter)

		newName := "Should Not Stick"
		if _, err := svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Name: &newName}); err == nil {
			t.Fatal("patch must fail when its audit emission fails (shared fate)")
		}
		if emitter.calls != 1 {
			t.Fatalf("emit calls = %d, want 1", emitter.calls)
		}
		got, err := f.store.GetMeter(f.ctx, f.tenantID, m.ID)
		if err != nil {
			t.Fatalf("get meter: %v", err)
		}
		if got.Name != "Before Rename" {
			t.Errorf("meter name = %q — the patch committed despite a failed audit emission", got.Name)
		}
		if rows := f.rows(t, "meter", m.ID); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back patch: %+v", rows)
		}
	})
}

// TestPriceWriteAudit_SharedFate pins the ADR-090 emission of a price
// write: the audit row is written on the write's own transaction, with the
// content before and after and when it applies. A never-used plan's write
// replaces the earlier content, so the row is the only record of it.
//
// Directions pinned: (1) an edit and a removal each write exactly one row on
// resource_type=plan_price, resource_id={price id}; (2) a write on a price
// that does not exist emits nothing; (3) a failed emission leaves the price
// as it was.
func TestPriceWriteAudit_SharedFate(t *testing.T) {
	f := newPricingFixture(t, "Price Write InTx Audit")

	t.Run("an edit and a removal are each recorded", func(t *testing.T) {
		plan, price := f.seedPlan(t, "audited")
		svc := f.auditedSvc()
		if _, err := svc.EditPrice(f.ctx, f.tenantID, plan.ID, price.ID, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(5)}); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if _, err := svc.RemovePrice(f.ctx, f.tenantID, plan.ID, price.ID); err != nil {
			t.Fatalf("remove: %v", err)
		}
		rows := f.rows(t, "plan_price", price.ID)
		if len(rows) != 2 {
			t.Fatalf("want two plan_price audit rows; got %d: %+v", len(rows), rows)
		}
		var edit, removal domain.AuditEntry
		for _, r := range rows {
			switch r.Action {
			case domain.AuditActionUpdate:
				edit = r
			case domain.AuditActionDelete:
				removal = r
			}
		}
		if edit.Metadata["effective"] != "now" || edit.Metadata["plan_id"] != plan.ID || edit.Metadata["before"] == nil || edit.Metadata["after"] == nil {
			t.Errorf("edit row metadata = %v, want plan_id, effective now, before and after", edit.Metadata)
		}
		if removal.Metadata["effective"] != "now" || removal.Metadata["after"] != nil {
			t.Errorf("removal row metadata = %v, want effective now and no after", removal.Metadata)
		}
	})

	t.Run("a price that does not exist emits nothing", func(t *testing.T) {
		plan, _ := f.seedPlan(t, "ghost")
		emitter := &countingEmitter{}
		svc := pricing.NewService(f.store)
		svc.SetAuditLogger(emitter)
		ghost := postgres.NewID("hc_prc")
		if _, err := svc.RemovePrice(f.ctx, f.tenantID, plan.ID, ghost); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("removing a price that does not exist must be a 404; got %v", err)
		}
		if emitter.calls != 0 {
			t.Fatalf("emit fired %d time(s) for a write that never happened", emitter.calls)
		}
	})

	t.Run("audit failure rolls the write back", func(t *testing.T) {
		plan, price := f.seedPlan(t, "rollback")
		emitter := &landThenFailEmitter{logger: f.logger}
		svc := pricing.NewService(f.store)
		svc.SetAuditLogger(emitter)
		if _, err := svc.RemovePrice(f.ctx, f.tenantID, plan.ID, price.ID); err == nil {
			t.Fatal("a removal must fail when its audit emission fails (shared fate)")
		}
		if emitter.calls != 1 {
			t.Fatalf("emit calls = %d, want 1", emitter.calls)
		}
		listed, err := svc.ListPlanPrices(f.ctx, f.tenantID, plan.ID)
		if err != nil || len(listed) != 1 || !listed[0].UnitAmountCents.Equal(decimal.NewFromInt(2)) {
			t.Errorf("the price must stand after a failed removal: %+v, %v", listed, err)
		}
		if rows := f.rows(t, "plan_price", price.ID); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back write: %+v", rows)
		}
	})
}

// TestMeterRoutes_AreAccountedFor: the meter PATCH and the price writes emit
// their own in-tx row, whose self-mark accounts for the request. If a route
// ever stops emitting, the coverage detector reports it rather than a
// fabricated row papering over it.
func TestMeterRoutes_AreAccountedFor(t *testing.T) {
	f := newPricingFixture(t, "Meter Routes Accounted")
	m := f.seedMeter(t, "markhandled")
	plan, price := f.seedPlan(t, "routes")
	h := pricing.NewHandler(f.auditedSvc())

	for _, tc := range []struct {
		name, method, path, body string
		srv                      http.Handler
		want                     int
	}{
		{"PATCH /meters/{id}", http.MethodPatch, "/" + m.ID, `{"name":"Renamed"}`, h.MeterRoutes(), http.StatusOK},
		{"PATCH /plans/{id}/prices/{price_id}", http.MethodPatch, "/" + plan.ID + "/prices/" + price.ID, `{"mode":"per_unit","unit_amount_cents":"3"}`, h.PlanRoutes(), http.StatusOK},
		{"DELETE /plans/{id}/prices/{price_id}", http.MethodDelete, "/" + plan.ID + "/prices/" + price.ID, "", h.PlanRoutes(), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqCtx := audit.WithRequestState(auth.WithTenantID(f.ctx, f.tenantID))
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)).WithContext(reqCtx)
			rec := httptest.NewRecorder()
			tc.srv.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if !audit.WasHandled(reqCtx) {
				t.Error("the write emitted no audit row — the coverage detector will report it as an uncovered mutation")
			}
		})
	}
}
