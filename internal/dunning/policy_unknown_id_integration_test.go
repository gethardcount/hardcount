package dunning_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestPolicyRoutes_UnknownIDIsNotFound: every dunning-policy route that takes
// an {id} answers 404 not_found for an id that does not exist. PATCH used to
// answer 500: the UPDATE ... RETURNING matched no row, and the store passed
// sql.ErrNoRows up unmapped, so respond.FromError fell through to
// internal_error. Real Postgres because the miss is the store's own query.
func TestPolicyRoutes_UnknownIDIsNotFound(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Dunning Policy Unknown ID")
	svc := dunning.NewService(dunning.NewPostgresStore(db), nil, clock.Real())
	routes := dunning.NewHandler(svc).Routes()

	const missing = "vlx_dpol_doesnotexist"
	validBody := `{"name":"x","enabled":true,"max_retry_attempts":3,"grace_period_days":3,"retry_schedule":["72h","120h"]}`

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/policies/" + missing, ""},
		{http.MethodPatch, "/policies/" + missing, validBody},
		{http.MethodDelete, "/policies/" + missing, ""},
		{http.MethodPost, "/policies/" + missing + "/set-default", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			ctx := postgres.WithLivemode(auth.WithTenantID(context.Background(), tenantID), false)
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
			}
			var env struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v; body: %s", err, rec.Body.String())
			}
			if env.Error.Type != "invalid_request_error" || env.Error.Code != "not_found" {
				t.Errorf("error = %+v, want invalid_request_error/not_found", env.Error)
			}
		})
	}
}
