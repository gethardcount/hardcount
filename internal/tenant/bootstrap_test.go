package tenant

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// testDeps returns BootstrapDeps that behave like the real user-package
// wiring without the peer import (tenant must not import user): the
// hash func enforces the same 12-char minimum, and CreateUserTx inserts
// the same rows user.PostgresStore.CreateInTx does. The REAL wiring
// (user.HashPassword + CreateInTx via the router) is exercised by the
// api-level e2e test (bootstrap → dashboard login → live-key call).
func testDeps(db *postgres.DB) BootstrapDeps {
	return BootstrapDeps{
		HashPassword: func(plaintext string) (string, error) {
			if len(plaintext) < 12 {
				return "", errs.Invalid("password", "must be at least 12 characters")
			}
			return "test-hash:" + plaintext, nil
		},
		CreateUserTx: func(ctx context.Context, tx *sql.Tx, email, passwordHash, tenantID, role string) (domain.User, error) {
			var u domain.User
			err := tx.QueryRowContext(ctx, `
				INSERT INTO users (email, password_hash) VALUES ($1, $2)
				RETURNING id, email::text, password_hash`, email, passwordHash,
			).Scan(&u.ID, &u.Email, &u.PasswordHash)
			if err != nil {
				return domain.User{}, err
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO user_tenants (user_id, tenant_id, role) VALUES ($1, $2, $3)`,
				u.ID, tenantID, role)
			return u, err
		},
		// The real seed writer: the tenant package may not import dunning,
		// but its tests may (the arch boundary skips _test files).
		SeedDunningTx: dunning.NewService(dunning.NewPostgresStore(db), nil, nil).SeedDefaultPoliciesTx,
	}
}

func newTestHandler(db *postgres.DB) *BootstrapHandler {
	return &BootstrapHandler{db: db, deps: testDeps(db), token: "test-token"}
}

// countRows counts via a bypass tx — SetupTestDB returns the
// RLS-enforced app connection, which sees zero rows in tenant-scoped
// tables without a tenant context.
func countRows(t *testing.T, db *postgres.DB, table string) int {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestBootstrap_Success(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newTestHandler(db)

	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"tenant_name":"Test Corp","token":"test-token"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control: got %q, want no-store (credentials transit once)", cc)
	}

	var resp bootstrapResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	if resp.Tenant.Name != "Test Corp" {
		t.Errorf("tenant name: got %q", resp.Tenant.Name)
	}
	// Mode-marked prefixes (ADR-073): the old unmarked vlx_secret_
	// parsed as LIVE in auth.ValidateKey — must not come back.
	if !strings.HasPrefix(resp.SecretKeyTest, "vlx_secret_test_") {
		t.Errorf("test secret key prefix: got %q", resp.SecretKeyTest)
	}
	if !strings.HasPrefix(resp.SecretKeyLive, "vlx_secret_live_") {
		t.Errorf("live secret key prefix: got %q", resp.SecretKeyLive)
	}
	if !strings.HasPrefix(resp.PublishableKeyTest, "vlx_pub_test_") {
		t.Errorf("publishable key prefix: got %q", resp.PublishableKeyTest)
	}
	if resp.OwnerEmail != "admin@velox.local" || !resp.PasswordGenerated || len(resp.OwnerPassword) < 12 {
		t.Errorf("owner defaults: email=%q generated=%v pwlen=%d", resp.OwnerEmail, resp.PasswordGenerated, len(resp.OwnerPassword))
	}

	// The self-host dead-end trio: owner user, live key, settings row.
	if n := countRows(t, db, "users"); n != 1 {
		t.Errorf("users: got %d, want 1", n)
	}
	if n := countRows(t, db, "user_tenants"); n != 1 {
		t.Errorf("user_tenants: got %d, want 1", n)
	}
	if n := countRows(t, db, "tenant_settings"); n != 1 {
		t.Errorf("tenant_settings: got %d, want 1", n)
	}
	btx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(btx)
	var liveKeys, testKeys int
	if err := btx.QueryRow(`SELECT count(*) FILTER (WHERE livemode), count(*) FILTER (WHERE NOT livemode) FROM api_keys WHERE key_type = 'secret'`).Scan(&liveKeys, &testKeys); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if liveKeys != 1 || testKeys != 1 {
		t.Errorf("secret keys live=%d test=%d, want 1/1 — livemode must be pinned per insert against the 0021 trigger", liveKeys, testKeys)
	}
}

// TestBootstrap_ValidationWritesNothing is THE half-bootstrap lock
// (panel HIGH): a password under MinPasswordLength must 422 with ZERO
// rows written — the old shape could commit tenant+keys, then fail
// user-create, and every retry 409'd "already bootstrapped" with no
// dashboard login possible, forever.
//
// Mutation-verify: move the HashPassword call after the tenant INSERT —
// the zero-rows assertion fails.
func TestBootstrap_ValidationWritesNothing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newTestHandler(db)

	req := httptest.NewRequest("POST", "/", strings.NewReader(
		`{"token":"test-token","owner_email":"me@x.test","owner_password":"short"}`))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("short password: got %d, want 422. body: %s", rec.Code, rec.Body.String())
	}
	for _, table := range []string{"tenants", "tenant_settings", "api_keys", "users", "user_tenants"} {
		if n := countRows(t, db, table); n != 0 {
			t.Errorf("%s after failed bootstrap: %d rows, want 0 (half-bootstrap!)", table, n)
		}
	}

	// The retry with a valid password succeeds — the install is NOT bricked.
	req = httptest.NewRequest("POST", "/", strings.NewReader(
		`{"token":"test-token","owner_email":"me@x.test","owner_password":"long-enough-password"}`))
	rec = httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry after validation failure: got %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
}

// TestBootstrap_ConcurrentPosts: the old INSERT ... WHERE NOT EXISTS
// was not race-safe under READ COMMITTED (two statement snapshots each
// miss the other's uncommitted row → two tenants, two credential
// sets). The advisory xact lock serializes.
//
// Mutation-verify: drop the pg_advisory_xact_lock — this flakes to two
// 201s / two tenants.
func TestBootstrap_ConcurrentPosts(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newTestHandler(db)

	const n = 4
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/", strings.NewReader(
				fmt.Sprintf(`{"tenant_name":"Racer %d","token":"test-token","owner_email":"r%d@x.test"}`, i, i)))
			rec := httptest.NewRecorder()
			h.Routes().ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	created, conflicted := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicted++
		}
	}
	if created != 1 || conflicted != n-1 {
		t.Errorf("codes: %v — want exactly one 201 and %d 409s", codes, n-1)
	}
	if got := countRows(t, db, "tenants"); got != 1 {
		t.Errorf("tenants after race: %d, want exactly 1", got)
	}
}

// TestBootstrap_GuardOrder pins ADR-073's oracle fix: a bootstrapped
// install answers a uniform 409 to ANY probe — wrong token, no token,
// even a disabled endpoint — so responses no longer disclose token
// validity or token configuration.
func TestBootstrap_GuardOrder(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newTestHandler(db)

	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"tenant_name":"First","token":"test-token"}`))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first bootstrap: got %d", rec.Code)
	}

	probes := []struct {
		name string
		h    *BootstrapHandler
		body string
	}{
		{"valid token", h, `{"token":"test-token"}`},
		{"wrong token", h, `{"token":"wrong"}`},
		{"no token", h, `{}`},
		{"endpoint disabled", &BootstrapHandler{db: db, deps: testDeps(db), token: ""}, `{"token":"anything"}`},
	}
	for _, p := range probes {
		req := httptest.NewRequest("POST", "/", strings.NewReader(p.body))
		rec := httptest.NewRecorder()
		p.h.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s on bootstrapped install: got %d, want uniform 409", p.name, rec.Code)
		}
	}
}

func TestBootstrap_TokenRequired(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := &BootstrapHandler{db: db, deps: testDeps(db), token: "my-secret-token"}

	// Without token
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"tenant_name":"Test"}`))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no token: got %d, want 403", rec.Code)
	}

	// With wrong token
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"tenant_name":"Test","token":"wrong"}`))
	rec = httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("wrong token: got %d, want 403", rec.Code)
	}

	// With correct token
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"tenant_name":"Test","token":"my-secret-token"}`))
	rec = httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("correct token: got %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_DefaultTenantName(t *testing.T) {
	db := testutil.SetupTestDB(t)
	h := newTestHandler(db)

	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"token":"test-token"}`))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	var resp bootstrapResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Tenant.Name != "Demo Tenant" {
		t.Errorf("default name: got %q, want 'Demo Tenant' (unified with the CLI default — one writer, one default)", resp.Tenant.Name)
	}
}
