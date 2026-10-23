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

func (f pricingFixture) seedRatingRule(t *testing.T, key string) domain.RatingRuleVersion {
	t.Helper()
	rule, err := pricing.NewService(f.store).CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: key, Name: key, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(2),
	})
	if err != nil {
		t.Fatalf("seed rating rule: %v", err)
	}
	return rule
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
// only the deleted HTTP catch-all — yet it is the operator action that binds or
// UNBINDS a meter's default rating rule, i.e. the single change that decides
// whether unmatched usage bills at all.
//
// Directions pinned: (1) success commits patch + exactly one update row with
// the changed fields; (2) a zero-row UPDATE (meter that doesn't exist) emits
// NOTHING; (3) a failed emission rolls the patch back.
func TestMeterUpdateAudit_SharedFate(t *testing.T) {
	f := newPricingFixture(t, "Meter Update InTx Audit")
	rule := f.seedRatingRule(t, "tokens")

	t.Run("patch commits with exactly one update row carrying the changed fields", func(t *testing.T) {
		m := f.seedMeter(t, "tokens_ok")
		newName := "Renamed Meter"
		agg := "max"

		out, err := f.auditedSvc().UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{
			Name:                &newName,
			Aggregation:         &agg,
			RatingRuleVersionID: &rule.ID,
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
		if row.Metadata["rating_rule_version_id"] != rule.ID {
			t.Errorf("metadata rating_rule_version_id = %v, want %s", row.Metadata["rating_rule_version_id"], rule.ID)
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
		if got.Name != newName || got.Aggregation != "max" || got.RatingRuleVersionID != rule.ID {
			t.Errorf("meter did not commit with its audit row: %+v", got)
		}
		_ = out
	})

	t.Run("unbinding the default rate is recorded, not silently absent", func(t *testing.T) {
		m := f.seedMeter(t, "tokens_unbind")
		empty := ""
		if _, err := f.auditedSvc().UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{
			RatingRuleVersionID: &rule.ID,
		}); err != nil {
			t.Fatalf("bind: %v", err)
		}
		if _, err := f.auditedSvc().UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{
			RatingRuleVersionID: &empty,
		}); err != nil {
			t.Fatalf("unbind: %v", err)
		}
		rows := f.rows(t, "meter", m.ID)
		if len(rows) != 2 {
			t.Fatalf("want two meter audit rows (bind + unbind); got %d", len(rows))
		}
		// Query returns newest-first: rows[0] is the unbind.
		v, ok := rows[0].Metadata["rating_rule_version_id"]
		if !ok || v != "" {
			t.Errorf("unbind row must record rating_rule_version_id=\"\" (the change that stops usage being priced); got %v (present=%v)", v, ok)
		}
	})

	t.Run("nonexistent meter emits nothing", func(t *testing.T) {
		ghostID := postgres.NewID("vlx_mtr") // never inserted
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

// TestMeterPricingRuleDeleteAudit_SharedFate pins the ADR-090 emission on
// DELETE /v1/meters/{meter_id}/pricing-rules/{id} — the ADR's poster child.
// The catch-all recorded this as action=delete resource_type=meter
// resource_id={meter_id}: a permanent, un-editable row claiming the operator
// destroyed the whole meter when they removed ONE dimension rule. The truth
// is now emitted from the DELETE's own tx, sourced from the deleted row.
//
// Directions pinned: (1) success removes the rule AND writes exactly one
// delete row on resource_type=meter_pricing_rule, resource_id={rule id},
// metadata.meter_id = the rule's own meter — and writes NOTHING on the meter
// (the old lie); (2) deleting a nonexistent rule emits NOTHING; (3) a failed
// emission leaves the rule in place.
func TestMeterPricingRuleDeleteAudit_SharedFate(t *testing.T) {
	f := newPricingFixture(t, "Rule Delete InTx Audit")
	rule := f.seedRatingRule(t, "tokens")

	seedPricingRule := func(t *testing.T, meterKey string, priority int) (domain.Meter, domain.MeterPricingRule) {
		t.Helper()
		m := f.seedMeter(t, meterKey)
		pr, err := pricing.NewService(f.store).UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID:             m.ID,
			RatingRuleVersionID: rule.ID,
			DimensionMatch:      map[string]any{"model": meterKey},
			Priority:            priority,
		})
		if err != nil {
			t.Fatalf("seed pricing rule: %v", err)
		}
		return m, pr
	}

	t.Run("delete records the rule, not the meter", func(t *testing.T) {
		m, pr := seedPricingRule(t, "rule_ok", 10)

		if err := f.auditedSvc().DeleteMeterPricingRule(f.ctx, f.tenantID, pr.ID); err != nil {
			t.Fatalf("delete rule: %v", err)
		}

		rows := f.rows(t, "meter_pricing_rule", pr.ID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one meter_pricing_rule audit row; got %d: %+v", len(rows), rows)
		}
		row := rows[0]
		if row.Action != domain.AuditActionDelete || row.ResourceType != "meter_pricing_rule" {
			t.Errorf("vocabulary: got %s/%s, want delete/meter_pricing_rule", row.Action, row.ResourceType)
		}
		if row.ResourceID != pr.ID {
			t.Errorf("resource_id = %q, want the RULE id %q (the catch-all used to record the meter id here)", row.ResourceID, pr.ID)
		}
		if row.Metadata["meter_id"] != m.ID {
			t.Errorf("metadata meter_id = %v, want %s", row.Metadata["meter_id"], m.ID)
		}
		if row.Metadata["rating_rule_version_id"] != rule.ID {
			t.Errorf("metadata rating_rule_version_id = %v, want %s", row.Metadata["rating_rule_version_id"], rule.ID)
		}

		// The lie is gone: nothing claims the METER was deleted.
		for _, r := range f.rows(t, "meter", m.ID) {
			if r.Action == domain.AuditActionDelete {
				t.Errorf("a delete row still points at the meter — the fabricated record ADR-090 exists to kill: %+v", r)
			}
		}

		// And the rule really is gone (shared fate, success side).
		if _, err := f.store.GetMeterPricingRule(f.ctx, f.tenantID, pr.ID); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("rule survived its delete (err=%v)", err)
		}
	})

	t.Run("nonexistent rule emits nothing", func(t *testing.T) {
		ghostID := postgres.NewID("vlx_mpr") // never inserted
		emitter := &countingEmitter{}
		svc := pricing.NewService(f.store)
		svc.SetAuditLogger(emitter)

		err := svc.DeleteMeterPricingRule(f.ctx, f.tenantID, ghostID)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("deleting a nonexistent rule must return ErrNotFound; got %v", err)
		}
		if emitter.calls != 0 {
			t.Fatalf("emit fired %d time(s) for a rule that was never deleted — FABRICATED EVIDENCE", emitter.calls)
		}
		if rows := f.rows(t, "meter_pricing_rule", ghostID); len(rows) != 0 {
			t.Errorf("audit rows recorded against a nonexistent rule: %+v", rows)
		}
	})

	t.Run("audit failure rolls the delete back", func(t *testing.T) {
		_, pr := seedPricingRule(t, "rule_rollback", 20)

		emitter := &landThenFailEmitter{logger: f.logger}
		svc := pricing.NewService(f.store)
		svc.SetAuditLogger(emitter)

		if err := svc.DeleteMeterPricingRule(f.ctx, f.tenantID, pr.ID); err == nil {
			t.Fatal("delete must fail when its audit emission fails (shared fate)")
		}
		if emitter.calls != 1 {
			t.Fatalf("emit calls = %d, want 1", emitter.calls)
		}
		// The rule is STILL BILLING — a rule that could not be recorded as
		// deleted must not be deleted.
		if _, err := f.store.GetMeterPricingRule(f.ctx, f.tenantID, pr.ID); err != nil {
			t.Errorf("rule was deleted despite a failed audit emission: %v", err)
		}
		if rows := f.rows(t, "meter_pricing_rule", pr.ID); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back delete: %+v", rows)
		}

		// The delete is still available to a retry.
		if err := f.auditedSvc().DeleteMeterPricingRule(f.ctx, f.tenantID, pr.ID); err != nil {
			t.Fatalf("retry delete: %v", err)
		}
		if rows := f.rows(t, "meter_pricing_rule", pr.ID); len(rows) != 1 {
			t.Errorf("retry must land exactly one delete audit row; got %d", len(rows))
		}
	})
}

// TestMeterRoutes_AreAccountedFor: both routes emit their own in-tx row, whose
// self-mark accounts for the request. The pricing-rule DELETE is the headline
// case of the whole redesign — the deleted catch-all recorded it as "deleted
// meter {meter_id}" (its classifier read parts[1] of the path as the resource
// id), a permanent row asserting the operator destroyed a meter that still
// exists. If this route ever stops emitting, the coverage detector reports it
// rather than a fabricated row papering over it.
func TestMeterRoutes_AreAccountedFor(t *testing.T) {
	f := newPricingFixture(t, "Meter Routes Accounted")
	rule := f.seedRatingRule(t, "tokens")
	m := f.seedMeter(t, "markhandled")

	pr, err := f.auditedSvc().UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: m.ID, RatingRuleVersionID: rule.ID,
		DimensionMatch: map[string]any{"model": "x"}, Priority: 5,
	})
	if err != nil {
		t.Fatalf("seed pricing rule: %v", err)
	}

	h := pricing.NewHandler(f.auditedSvc())
	passthrough := func(next http.Handler) http.Handler { return next }

	t.Run("PATCH /meters/{id} is accounted for", func(t *testing.T) {
		reqCtx := audit.WithRequestState(auth.WithTenantID(f.ctx, f.tenantID))
		req := httptest.NewRequest(http.MethodPatch, "/"+m.ID,
			strings.NewReader(`{"name":"Renamed"}`)).WithContext(reqCtx)
		rec := httptest.NewRecorder()
		h.MeterRoutes().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("patch meter: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !audit.WasHandled(reqCtx) {
			t.Error("meter PATCH emitted no audit row — the coverage detector will report it as an uncovered mutation")
		}
	})

	t.Run("DELETE /meters/{meter_id}/pricing-rules/{id} is accounted for", func(t *testing.T) {
		reqCtx := audit.WithRequestState(auth.WithTenantID(f.ctx, f.tenantID))
		req := httptest.NewRequest(http.MethodDelete, "/"+pr.ID, nil).WithContext(reqCtx)
		rec := httptest.NewRecorder()
		h.MeterPricingRuleRoutes(passthrough, passthrough).ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete rule: got %d, want 204; body=%s", rec.Code, rec.Body.String())
		}
		if !audit.WasHandled(reqCtx) {
			t.Error("pricing-rule DELETE emitted no audit row — this is the route whose fabricated 'deleted meter {meter_id}' row ADR-090 exists to kill; it must carry its own truthful evidence")
		}
	})
}
