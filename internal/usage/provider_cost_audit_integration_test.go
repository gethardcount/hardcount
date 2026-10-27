package usage_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// failingProviderCostEmitter satisfies usage.AuditEmitter and always errors —
// the fault injection for the shared-fate direction: if the audit row cannot
// be written, the rate mutation must not commit.
type failingProviderCostEmitter struct{}

func (failingProviderCostEmitter) LogInTx(_ context.Context, _ *sql.Tx, _ audit.Entry) error {
	return errors.New("injected audit failure")
}

// providerCostAuditFixture wires the real store + handler exactly as the
// composition root does (store-owned tx, handler-built emission).
type providerCostAuditFixture struct {
	db       *postgres.DB
	store    *usage.PostgresStore
	logger   *audit.Logger
	tenantID string
	ctx      context.Context
}

func newProviderCostAuditFixture(t *testing.T) *providerCostAuditFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Provider Cost Audit")
	ctx, cancel := context.WithTimeout(
		auth.WithTenantID(postgres.WithLivemode(context.Background(), false), tenantID),
		60*time.Second,
	)
	t.Cleanup(cancel)
	return &providerCostAuditFixture{
		db:       db,
		store:    usage.NewPostgresStore(db),
		logger:   audit.NewLogger(db),
		tenantID: tenantID,
		ctx:      ctx,
	}
}

// serve drives the real chi sub-router (PUT / and DELETE /{id}) so path params
// are exercised as mounted. The request ctx carries the audit bookkeeping cell,
// so callers can assert the request is ACCOUNTED FOR — the property the
// root-mounted AuditCoverage detector reads (an unaccounted-for mutating 2xx is
// an uncovered mutation).
func (f *providerCostAuditFixture) serve(t *testing.T, emitter usage.AuditEmitter, method, target, body string) (*httptest.ResponseRecorder, context.Context) {
	t.Helper()
	h := usage.NewProviderCostHandler(f.store, nil)
	if emitter != nil {
		h.SetAuditLogger(emitter)
	}
	reqCtx := audit.WithRequestState(f.ctx)
	req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec, reqCtx
}

func (f *providerCostAuditFixture) auditRows(t *testing.T, filter audit.QueryFilter) []domain.AuditEntry {
	t.Helper()
	filter.ResourceType = "provider_cost"
	rows, _, err := f.logger.Query(f.ctx, f.tenantID, filter)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return rows
}

func (f *providerCostAuditFixture) rateCount(t *testing.T) int {
	t.Helper()
	rates, err := f.store.ListProviderCostRates(f.ctx, f.tenantID)
	if err != nil {
		t.Fatalf("list rates: %v", err)
	}
	return len(rates)
}

// TestProviderCostAudit_SharedFate covers both ADR-090 directions for
// PUT /v1/provider-costs and DELETE /v1/provider-costs/{id} — the two routes
// that relied on the middleware catch-all (which recorded them from the URL,
// with no idea what rate changed).
func TestProviderCostAudit_SharedFate(t *testing.T) {
	f := newProviderCostAuditFixture(t)

	const rateBody = `{"provider":"anthropic","model":"claude-sonnet-4","token_type":"input","unit_cost_cents":"0.0003","currency":"usd"}`

	var rateID string

	t.Run("upsert commits the rate and its audit row together", func(t *testing.T) {
		rec, reqCtx := f.serve(t, f.logger, http.MethodPut, "/", rateBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("upsert: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		// The in-tx emission is the record, and its self-mark is what tells the
		// coverage detector this mutation left evidence.
		if !audit.WasHandled(reqCtx) {
			t.Error("upsert emitted no audit row — the coverage detector will report it as an uncovered mutation")
		}
		rates, err := f.store.ListProviderCostRates(f.ctx, f.tenantID)
		if err != nil {
			t.Fatalf("list rates: %v", err)
		}
		if len(rates) != 1 {
			t.Fatalf("want 1 persisted rate; got %d", len(rates))
		}
		rateID = rates[0].ID

		rows := f.auditRows(t, audit.QueryFilter{ResourceID: rateID})
		if len(rows) != 1 {
			t.Fatalf("want exactly one audit row for %s; got %+v", rateID, rows)
		}
		got := rows[0]
		if got.Action != "update" {
			t.Errorf("action: got %q, want %q", got.Action, "update")
		}
		if got.ResourceType != "provider_cost" {
			t.Errorf("resource_type: got %q", got.ResourceType)
		}
		if got.Metadata["provider"] != "anthropic" || got.Metadata["model"] != "claude-sonnet-4" ||
			got.Metadata["token_type"] != "input" {
			t.Errorf("metadata must identify the rate's key: %+v", got.Metadata)
		}
		if got.Metadata["unit_cost_cents"] != "0.0003" {
			t.Errorf("metadata unit_cost_cents: got %v, want 0.0003", got.Metadata["unit_cost_cents"])
		}
		// Currency is normalized by the handler before the write — the audit
		// row must show the value that actually landed, not the request's.
		if got.Metadata["currency"] != "USD" {
			t.Errorf("metadata currency: got %v, want USD (as persisted)", got.Metadata["currency"])
		}
	})

	t.Run("upsert audit failure rolls the rate edit back", func(t *testing.T) {
		before, err := f.store.ListProviderCostRates(f.ctx, f.tenantID)
		if err != nil {
			t.Fatalf("list rates: %v", err)
		}
		rec, _ := f.serve(t, failingProviderCostEmitter{}, http.MethodPut, "/",
			`{"provider":"anthropic","model":"claude-sonnet-4","token_type":"input","unit_cost_cents":"999.9999","currency":"usd"}`)
		if rec.Code < 500 {
			t.Fatalf("upsert must fail when its audit emission fails; got %d", rec.Code)
		}
		after, err := f.store.ListProviderCostRates(f.ctx, f.tenantID)
		if err != nil {
			t.Fatalf("list rates: %v", err)
		}
		if len(after) != len(before) || !after[0].UnitCostCents.Equal(before[0].UnitCostCents) {
			t.Errorf("rate moved despite the failed audit emission: before=%+v after=%+v", before, after)
		}
		if rows := f.auditRows(t, audit.QueryFilter{}); len(rows) != 1 {
			t.Errorf("want the single successful row only; got %d rows", len(rows))
		}
	})

	t.Run("a brand-new rate rolls back entirely when its audit fails", func(t *testing.T) {
		rec, _ := f.serve(t, failingProviderCostEmitter{}, http.MethodPut, "/",
			`{"provider":"openai","model":"gpt-5","token_type":"output","unit_cost_cents":"0.001","currency":"usd"}`)
		if rec.Code < 500 {
			t.Fatalf("upsert must fail when its audit emission fails; got %d", rec.Code)
		}
		if n := f.rateCount(t); n != 1 {
			t.Errorf("the un-audited INSERT leaked: want 1 rate, got %d", n)
		}
	})

	t.Run("deleting a nonexistent rate emits nothing", func(t *testing.T) {
		rec, reqCtx := f.serve(t, f.logger, http.MethodDelete, "/vlx_pcr_does_not_exist", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("delete missing rate: got %d, want 404", rec.Code)
		}
		if audit.WasHandled(reqCtx) {
			t.Error("a 404 must not mark the request handled")
		}
		rows := f.auditRows(t, audit.QueryFilter{Action: "delete"})
		if len(rows) != 0 {
			t.Errorf("a zero-row DELETE must not fabricate a 'deleted' record: %+v", rows)
		}
	})

	t.Run("delete audit failure rolls the delete back", func(t *testing.T) {
		rec, _ := f.serve(t, failingProviderCostEmitter{}, http.MethodDelete, "/"+rateID, "")
		if rec.Code < 500 {
			t.Fatalf("delete must fail when its audit emission fails; got %d", rec.Code)
		}
		if n := f.rateCount(t); n != 1 {
			t.Errorf("rate vanished despite the failed audit emission: %d rates left", n)
		}
		if rows := f.auditRows(t, audit.QueryFilter{Action: "delete"}); len(rows) != 0 {
			t.Errorf("phantom delete row for a rolled-back delete: %+v", rows)
		}
	})

	t.Run("delete commits the removal and its audit row together", func(t *testing.T) {
		rec, reqCtx := f.serve(t, f.logger, http.MethodDelete, "/"+rateID, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("delete: got %d, want 204; body=%s", rec.Code, rec.Body.String())
		}
		if !audit.WasHandled(reqCtx) {
			t.Error("delete emitted no audit row — the coverage detector will report it as an uncovered mutation")
		}
		if n := f.rateCount(t); n != 0 {
			t.Errorf("rate not deleted: %d rates left", n)
		}
		rows := f.auditRows(t, audit.QueryFilter{Action: "delete"})
		if len(rows) != 1 {
			t.Fatalf("want exactly one 'delete' audit row; got %+v", rows)
		}
		got := rows[0]
		if got.ResourceID != rateID {
			t.Errorf("resource_id: got %q, want %q", got.ResourceID, rateID)
		}
		// The row is gone from provider_cost_rates — the audit metadata is the
		// only surviving description of what was removed.
		if got.Metadata["provider"] != "anthropic" || got.Metadata["model"] != "claude-sonnet-4" ||
			got.Metadata["token_type"] != "input" || got.Metadata["unit_cost_cents"] != "0.0003" {
			t.Errorf("delete metadata must describe the deleted rate: %+v", got.Metadata)
		}
	})
}
