package recipe

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// failingRecipeEmitter satisfies recipe.AuditEmitter and always errors — the
// fault injection for the shared-fate direction: if the audit row cannot be
// written, the whole installed object graph must roll back with it.
type failingRecipeEmitter struct{}

func (failingRecipeEmitter) LogInTx(_ context.Context, _ *sql.Tx, _ audit.Entry) error {
	return errors.New("injected audit failure")
}

func recipeAuditRows(t *testing.T, logger *audit.Logger, ctx context.Context, tenantID string) []domain.AuditEntry {
	t.Helper()
	rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceType: "recipe"})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return rows
}

// TestRecipeInstantiate_AuditSharedFate covers POST
// /v1/recipes/{key}/instantiate — the route that relied on the middleware
// catch-all, which recorded "created recipe {key}" from the URL and could say
// nothing about what the apply actually installed.
//
// Three properties: (a) the apply's row commits with the object graph and
// names what was installed; (b) an emit failure rolls the ENTIRE graph back
// (no plan, no meter, no instance) and leaves no row; (c) the idempotent
// re-apply — which installs nothing (ADR-085) — emits nothing.
func TestRecipeInstantiate_AuditSharedFate(t *testing.T) {
	f := newRecipeFixture(t)
	logger := audit.NewLogger(f.db)
	ctx := postgres.WithLivemode(context.Background(), false)

	t.Run("audit failure rolls the whole installed graph back", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, f.db, "Recipe Audit RollBack")
		registry, err := Load()
		if err != nil {
			t.Fatalf("load registry: %v", err)
		}
		svc := NewService(f.db, NewPostgresStore(f.db), registry, f.pricingSvc, f.dunningSvc, f.webhookSvc)
		svc.SetAuditLogger(failingRecipeEmitter{})

		if _, err := svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{}); err == nil {
			t.Fatal("instantiate must fail when its audit emission fails (shared fate)")
		}
		for _, table := range []string{"recipe_instances", "plans", "meters", "plan_prices", "webhook_endpoints"} {
			if n := countRows(t, f.db, tenantID, table); n != 0 {
				t.Errorf("%s: %d rows survived a failed audit emission — the un-audited install leaked", table, n)
			}
		}
		if rows := recipeAuditRows(t, logger, ctx, tenantID); len(rows) != 0 {
			t.Errorf("phantom audit row for a rolled-back install: %+v", rows)
		}
	})

	t.Run("successful apply commits the graph and one audit row", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, f.db, "Recipe Audit Apply")
		f.svc.SetAuditLogger(logger)

		inst, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{
			CreatedBy: "operator@example.com",
		})
		if err != nil {
			t.Fatalf("instantiate: %v", err)
		}

		rows := recipeAuditRows(t, logger, ctx, tenantID)
		if len(rows) != 1 {
			t.Fatalf("want exactly one recipe audit row; got %+v", rows)
		}
		got := rows[0]
		if got.Action != domain.AuditActionCreate {
			t.Errorf("action: got %q, want %q", got.Action, domain.AuditActionCreate)
		}
		// resource_id is the recipe KEY — the id the operator surface resolves.
		if got.ResourceID != "anthropic_style" {
			t.Errorf("resource_id: got %q, want %q", got.ResourceID, "anthropic_style")
		}
		if got.ResourceLabel == "" {
			t.Error("resource_label must carry the recipe's name")
		}
		if got.Metadata["instance_id"] != inst.ID {
			t.Errorf("metadata instance_id: got %v, want %s", got.Metadata["instance_id"], inst.ID)
		}
		if got.Metadata["recipe_version"] != inst.RecipeVersion {
			t.Errorf("metadata recipe_version: got %v, want %s", got.Metadata["recipe_version"], inst.RecipeVersion)
		}
		// "What did this recipe install?" must be answerable from the row.
		assertIDList(t, got.Metadata, "plan_ids", inst.CreatedObjects.PlanIDs)
		assertIDList(t, got.Metadata, "meter_ids", inst.CreatedObjects.MeterIDs)
		assertIDList(t, got.Metadata, "price_ids", inst.CreatedObjects.PriceIDs)
		if got.Metadata["dunning_policy_id"] != inst.CreatedObjects.DunningPolicyID {
			t.Errorf("metadata dunning_policy_id: got %v, want %s", got.Metadata["dunning_policy_id"], inst.CreatedObjects.DunningPolicyID)
		}
		if got.Metadata["webhook_endpoint_id"] != inst.CreatedObjects.WebhookEndpointID {
			t.Errorf("metadata webhook_endpoint_id: got %v, want %s", got.Metadata["webhook_endpoint_id"], inst.CreatedObjects.WebhookEndpointID)
		}

		t.Run("idempotent re-apply installs nothing and emits nothing", func(t *testing.T) {
			second, err := f.svc.Instantiate(ctx, tenantID, "anthropic_style", nil, InstantiateOptions{})
			if err != nil {
				t.Fatalf("re-apply: %v", err)
			}
			if second.ID != inst.ID {
				t.Fatalf("re-apply must return the existing instance: got %s, want %s", second.ID, inst.ID)
			}
			if rows := recipeAuditRows(t, logger, ctx, tenantID); len(rows) != 1 {
				t.Errorf("a no-op re-apply must not record a second install: got %d rows", len(rows))
			}
		})

		// Through the mounted route: the request must come back ACCOUNTED FOR on
		// BOTH arms — an install self-marks via its in-tx emission, and the no-op
		// re-apply DECLARES itself (Service.Instantiate → audit.MarkSkip). Without
		// the declaration the coverage detector would report every re-apply — a 201
		// that installed nothing — as an uncovered mutation, forever.
		t.Run("a no-op re-apply is declared, not reported", func(t *testing.T) {
			h := NewHandler(f.svc)
			reqCtx := audit.WithRequestState(auth.WithTenantID(ctx, tenantID))
			req := httptest.NewRequest(http.MethodPost, "/anthropic_style/instantiate", strings.NewReader(`{}`)).
				WithContext(reqCtx)
			rec := httptest.NewRecorder()
			h.Routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusCreated {
				t.Fatalf("instantiate: got %d, want 201; body=%s", rec.Code, rec.Body.String())
			}
			if !audit.WasHandled(reqCtx) {
				t.Error("a no-op re-apply left the request unaccounted-for — the coverage detector would report it as an uncovered mutation (Service.Instantiate must MarkSkip on the badge-exists branch)")
			}
			if rows := recipeAuditRows(t, logger, ctx, tenantID); len(rows) != 1 {
				t.Errorf("no-op re-apply over HTTP recorded an extra row: got %d", len(rows))
			}
		})
	})
}

// assertIDList pins that a metadata key carries exactly the ids the instance
// row recorded. JSONB round-trips string slices as []any.
func assertIDList(t *testing.T, meta map[string]any, key string, want []string) {
	t.Helper()
	raw, ok := meta[key].([]any)
	if !ok {
		t.Errorf("metadata %s: missing or not a list (%T)", key, meta[key])
		return
	}
	if len(raw) != len(want) {
		t.Errorf("metadata %s: got %d ids, want %d", key, len(raw), len(want))
		return
	}
	for i, v := range raw {
		if v != want[i] {
			t.Errorf("metadata %s[%d]: got %v, want %s", key, i, v, want[i])
		}
	}
}
