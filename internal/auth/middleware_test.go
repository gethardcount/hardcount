package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

func TestMiddleware_NoAuth(t *testing.T) {
	svc := NewService(newMemStore())
	mw := Middleware(svc)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestMiddleware_InvalidKey(t *testing.T) {
	svc := NewService(newMemStore())
	mw := Middleware(svc)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer hc_secret_0000000000000000000000000000000000000000000000000000000000000000000000000000")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestMiddleware_ValidSecretKey(t *testing.T) {
	svc := NewService(newMemStore())
	result, _ := svc.CreateKey(postgres.WithLivemode(t.Context(), true), "tenant1", CreateKeyInput{Name: "Test", KeyType: KeyTypeSecret})

	mw := Middleware(svc)

	var gotTenantID string
	var gotKeyType KeyType
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenantID = TenantID(r.Context())
		gotKeyType = GetKeyType(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+result.RawKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if gotTenantID != "tenant1" {
		t.Errorf("tenant_id: got %q", gotTenantID)
	}
	if gotKeyType != KeyTypeSecret {
		t.Errorf("key_type: got %q, want secret", gotKeyType)
	}
}

func TestMiddleware_XAPIKeyFallback(t *testing.T) {
	svc := NewService(newMemStore())
	result, _ := svc.CreateKey(postgres.WithLivemode(t.Context(), true), "t1", CreateKeyInput{Name: "Test", KeyType: KeyTypeSecret})

	mw := Middleware(svc)
	var gotKeyType KeyType
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeyType = GetKeyType(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", result.RawKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if gotKeyType != KeyTypeSecret {
		t.Errorf("key_type: got %q, want secret", gotKeyType)
	}
}

func TestRequire_SecretKeyHasFullAccess(t *testing.T) {
	svc := NewService(newMemStore())
	result, _ := svc.CreateKey(postgres.WithLivemode(t.Context(), true), "t1", CreateKeyInput{Name: "Secret", KeyType: KeyTypeSecret})

	perms := []Permission{
		PermCustomerRead, PermCustomerWrite, PermPricingRead, PermPricingWrite,
		PermSubscriptionRead, PermSubscriptionWrite, PermUsageRead, PermUsageWrite,
		PermInvoiceRead, PermInvoiceWrite, PermDunningRead, PermDunningWrite,
		PermAPIKeyRead, PermAPIKeyWrite,
	}

	for _, perm := range perms {
		t.Run(string(perm), func(t *testing.T) {
			handler := Middleware(svc)(Require(perm)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})))

			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Authorization", "Bearer "+result.RawKey)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("secret key should have %s, got status %d", perm, rec.Code)
			}
		})
	}
}

// An ingest key passes Require(usage:ingest) and gets the ordinary
// missing-permission 403 on everything else, with the same message a
// secret key would get for a permission it lacked.
func TestRequire_IngestKeyOnlySendsUsage(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := WithKeyType(postgres.WithLivemode(t.Context(), true), KeyTypeSecret)
	result, err := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "LiteLLM", KeyType: KeyTypeIngest})
	if err != nil {
		t.Fatal(err)
	}

	for _, perm := range everyPermission {
		t.Run(string(perm), func(t *testing.T) {
			reached := false
			handler := Middleware(svc)(Require(perm)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})))
			req := httptest.NewRequest("POST", "/", nil)
			req.Header.Set("Authorization", "Bearer "+result.RawKey)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if perm == PermUsageIngest {
				if rec.Code != http.StatusOK || !reached {
					t.Errorf("ingest key on usage:ingest: %d, want 200", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusForbidden || reached {
				t.Errorf("ingest key on %s: %d (reached=%v), want 403", perm, rec.Code, reached)
			}
			want := "insufficient permissions: this key type does not have " + string(perm) + " access"
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("ingest key on %s: body %s, want the missing-permission message %q", perm, rec.Body.String(), want)
			}
		})
	}
}

func TestLivemodeFromRawKey(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		live bool
		ok   bool
	}{
		{"secret_live", "hc_secret_live_" + "ff", true, true},
		{"secret_test", "hc_secret_test_" + "aa", false, true},
		{"ingest_live", "hc_ingest_live_xyz", true, true},
		{"empty", "", false, false},
		{"no_mode_infix", "hc_cps_customer_portal_token", false, false},
		{"random", "not-a-key", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			live, ok := LivemodeFromRawKey(c.raw)
			if live != c.live || ok != c.ok {
				t.Errorf("got (live=%v, ok=%v), want (live=%v, ok=%v)", live, ok, c.live, c.ok)
			}
		})
	}
}

func TestLivemodeFromRequest(t *testing.T) {
	// Bearer header with live key
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer hc_secret_live_"+"abc")
	if live, ok := LivemodeFromRequest(r); !ok || !live {
		t.Errorf("Bearer live: got (live=%v, ok=%v), want (true, true)", live, ok)
	}

	// X-API-Key fallback with test key
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "hc_secret_test_xyz")
	if live, ok := LivemodeFromRequest(r); !ok || live {
		t.Errorf("X-API-Key test: got (live=%v, ok=%v), want (false, true)", live, ok)
	}

	// No auth header → not ok
	r = httptest.NewRequest("GET", "/", nil)
	if _, ok := LivemodeFromRequest(r); ok {
		t.Error("no auth header should return ok=false")
	}

	// Unknown bearer token → ok=false (not rejected here, just not our concern)
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer some-opaque-session-token")
	if _, ok := LivemodeFromRequest(r); ok {
		t.Error("unparseable bearer should return ok=false")
	}
}

func TestRequireMethod_GETUsesReadPOSTUsesWrite(t *testing.T) {
	svc := NewService(newMemStore())
	// An ingest key holds usage:ingest but NOT customer:write, so
	// RequireMethod(usage:ingest, customer:write) passes read methods (arg0
	// held) and 403s write methods (arg1 absent) — isolating
	// GET/HEAD/OPTIONS→arg0 and POST/PUT/PATCH/DELETE→arg1.
	sec, _ := svc.CreateKey(postgres.WithLivemode(t.Context(), true), "t1", CreateKeyInput{Name: "Ingest", KeyType: KeyTypeIngest})

	mw := Middleware(svc)(RequireMethod(PermUsageIngest, PermCustomerWrite)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
	))

	cases := []struct {
		method string
		want   int
		why    string
	}{
		{"GET", http.StatusOK, "GET → arg0 (usage:ingest) held"},
		{"HEAD", http.StatusOK, "HEAD → arg0 held"},
		{"OPTIONS", http.StatusOK, "OPTIONS → arg0 held"},
		{"POST", http.StatusForbidden, "POST → arg1 (customer:write) absent"},
		{"PUT", http.StatusForbidden, "PUT → arg1 absent"},
		{"PATCH", http.StatusForbidden, "PATCH → arg1 absent"},
		{"DELETE", http.StatusForbidden, "DELETE → arg1 absent"},
	}

	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.why, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/", nil)
			req.Header.Set("Authorization", "Bearer "+sec.RawKey)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s: got %d, want %d", tc.why, rec.Code, tc.want)
			}
		})
	}
}
