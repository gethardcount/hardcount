package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_LockWaitEnded_AnswersConflict_SameKeyRunsAgain: a request whose
// lock wait Postgres ended (SQLSTATE 55P03, lock_timeout) answers 409
// concurrent_change, the idempotency layer does not store it, and the same
// request with the same Idempotency-Key then runs once (ADR-136, amended
// 2026-11-10).
//
// The five new routes are the request-path takers of the customer-rules
// lock: subscription create and activate, item add (both its branches), and
// customer update and billing profile. A second session holds that lock. The
// cancel, a route that already converted a deadlock, waits for the
// subscription row instead.
//
// The server runs on a pool whose role has lock_timeout = 300ms, as an
// operator's ALTER ROLE hardcount_app SET lock_timeout does today. So the
// blocked request ends by itself, and the test releases the lock after it.
//
// Mutation-verify: drop the 55P03 branch of respond.ConcurrentChangeAsConflict,
// or remove its call at one handler: that route's first answer is a 500,
// and the same key replays it.
func TestE2E_LockWaitEnded_AnswersConflict_SameKeyRunsAgain(t *testing.T) {
	addAt := lagP0.AddDate(0, 0, 10) // mid-period: the add prorates

	// activeRig is a subscription on plan_a from lagP0, its first invoice
	// paid: an add that prorates is refused against an unpaid one (ADR-050).
	activeRig := func(t *testing.T) *lagRig {
		r := newLagRig(t, "in_advance", false)
		r.must(http.MethodPost, "/v1/invoices/"+r.firstInvoiceID()+"/record-payment", lagP0.Add(time.Hour),
			map[string]any{"note": "fixture"}, http.StatusOK)
		return r
	}
	// createdRig is the rig's customer with one subscription made by body.
	createdRig := func(extra map[string]any) func(t *testing.T) *lagRig {
		return func(t *testing.T) *lagRig {
			r := newLagRigBare(t, "in_advance", 0)
			body := map[string]any{
				"code": "lock-sub", "display_name": "Lock Sub", "customer_id": r.custID,
				"items": []map[string]any{{"plan_id": r.planA, "quantity": 1}},
			}
			for k, v := range extra {
				body[k] = v
			}
			r.subID = r.must(http.MethodPost, "/v1/subscriptions", lagP0, body, http.StatusCreated)["id"].(string)
			return r
		}
	}
	holdCustomerRules := func(t *testing.T, tx *sql.Tx, r *lagRig) {
		if _, err := tx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"customer_rules:"+r.tenantID+":"+r.custID); err != nil {
			t.Fatalf("hold the customer-rules lock: %v", err)
		}
	}
	holdSubscription := func(t *testing.T, tx *sql.Tx, r *lagRig) {
		if _, err := tx.ExecContext(t.Context(), `SELECT 1 FROM subscriptions WHERE id = $1 FOR UPDATE`, r.subID); err != nil {
			t.Fatalf("hold the subscription row: %v", err)
		}
	}
	items := func(t *testing.T, r *lagRig) int {
		return flowCount(t, r.db, `SELECT count(*) FROM subscription_items WHERE subscription_id = $1 AND deleted_at IS NULL`, r.subID)
	}
	customer := func(r *lagRig) map[string]any {
		return r.must(http.MethodGet, "/v1/customers/"+r.custID, addAt, nil, http.StatusOK)
	}

	for _, c := range []struct {
		name   string
		rig    func(t *testing.T) *lagRig
		hold   func(t *testing.T, tx *sql.Tx, r *lagRig)
		method string
		path   func(r *lagRig) string
		body   func(r *lagRig) any
		at     time.Time
		want   int
		// unchanged checks the 409 changed nothing; once, that the resend
		// made the change once.
		unchanged, once func(t *testing.T, r *lagRig)
	}{
		{
			name: "cancel", rig: activeRig, hold: holdSubscription,
			method: http.MethodPost, path: func(r *lagRig) string { return "/v1/subscriptions/" + r.subID + "/cancel" },
			at: addAt, want: http.StatusOK,
			unchanged: func(t *testing.T, r *lagRig) {
				if s := r.sub(); s.Status != domain.SubscriptionActive {
					t.Fatalf("subscription %s, want active", s.Status)
				}
			},
			once: func(t *testing.T, r *lagRig) {
				if s := r.sub(); s.Status != domain.SubscriptionCanceled {
					t.Fatalf("subscription %s, want canceled", s.Status)
				}
			},
		},
		{
			name: "subscription_create", rig: func(t *testing.T) *lagRig { return newLagRigBare(t, "in_advance", 0) },
			hold:   holdCustomerRules,
			method: http.MethodPost, path: func(*lagRig) string { return "/v1/subscriptions" },
			body: func(r *lagRig) any {
				return map[string]any{
					"code": "lock-sub", "display_name": "Lock Sub", "customer_id": r.custID,
					"items": []map[string]any{{"plan_id": r.planA, "quantity": 1}},
				}
			},
			at: lagP0, want: http.StatusCreated,
			unchanged: func(t *testing.T, r *lagRig) {
				if subs, invs := r.customerCounts(); subs != 0 || invs != 0 {
					t.Fatalf("%d subscriptions and %d invoices, want none", subs, invs)
				}
			},
			once: func(t *testing.T, r *lagRig) {
				if subs, invs := r.customerCounts(); subs != 1 || invs != 1 {
					t.Fatalf("%d subscriptions and %d invoices, want one of each", subs, invs)
				}
			},
		},
		{
			name: "subscription_activate", rig: createdRig(map[string]any{"status": "draft"}), hold: holdCustomerRules,
			method: http.MethodPost, path: func(r *lagRig) string { return "/v1/subscriptions/" + r.subID + "/activate" },
			at: lagP0, want: http.StatusOK,
			unchanged: func(t *testing.T, r *lagRig) {
				if s := r.sub(); s.Status != domain.SubscriptionDraft {
					t.Fatalf("subscription %s, want draft", s.Status)
				}
				if _, invs := r.customerCounts(); invs != 0 {
					t.Fatalf("%d invoices, want none", invs)
				}
			},
			once: func(t *testing.T, r *lagRig) {
				if s := r.sub(); s.Status != domain.SubscriptionActive {
					t.Fatalf("subscription %s, want active", s.Status)
				}
				if _, invs := r.customerCounts(); invs != 1 {
					t.Fatalf("%d invoices, want the one first invoice", invs)
				}
			},
		},
		{
			name: "item_add_prorated", rig: activeRig, hold: holdCustomerRules,
			method: http.MethodPost, path: func(r *lagRig) string { return "/v1/subscriptions/" + r.subID + "/items" },
			body: func(r *lagRig) any { return map[string]any{"plan_id": r.planC, "quantity": 1} },
			at:   addAt, want: http.StatusCreated,
			unchanged: func(t *testing.T, r *lagRig) {
				if n := items(t, r); n != 1 {
					t.Fatalf("%d items, want 1", n)
				}
				if _, invs := r.customerCounts(); invs != 1 {
					t.Fatalf("%d invoices, want only the first", invs)
				}
			},
			once: func(t *testing.T, r *lagRig) {
				if n := items(t, r); n != 2 {
					t.Fatalf("%d items, want 2", n)
				}
				if _, invs := r.customerCounts(); invs != 2 {
					t.Fatalf("%d invoices, want the first and one proration invoice", invs)
				}
			},
		},
		{
			// A trialing subscription prorates nothing, so the add runs in
			// Service.AddItem's transaction of its own.
			name: "item_add_unprorated", rig: createdRig(map[string]any{"trial_days": 14}), hold: holdCustomerRules,
			method: http.MethodPost, path: func(r *lagRig) string { return "/v1/subscriptions/" + r.subID + "/items" },
			body: func(r *lagRig) any { return map[string]any{"plan_id": r.planC, "quantity": 1} },
			at:   lagP0.AddDate(0, 0, 2), want: http.StatusCreated,
			unchanged: func(t *testing.T, r *lagRig) {
				if n := items(t, r); n != 1 {
					t.Fatalf("%d items, want 1", n)
				}
			},
			once: func(t *testing.T, r *lagRig) {
				if n := items(t, r); n != 2 {
					t.Fatalf("%d items, want 2", n)
				}
			},
		},
		{
			name: "customer_update", rig: activeRig, hold: holdCustomerRules,
			method: http.MethodPatch, path: func(r *lagRig) string { return "/v1/customers/" + r.custID },
			body: func(*lagRig) any { return map[string]any{"display_name": "Renamed Customer"} },
			at:   addAt, want: http.StatusOK,
			unchanged: func(t *testing.T, r *lagRig) {
				if got := customer(r)["display_name"]; got != "Lag Customer" {
					t.Fatalf("display_name %v, want Lag Customer", got)
				}
			},
			once: func(t *testing.T, r *lagRig) {
				if got := customer(r)["display_name"]; got != "Renamed Customer" {
					t.Fatalf("display_name %v, want Renamed Customer", got)
				}
			},
		},
		{
			name: "billing_profile", rig: activeRig, hold: holdCustomerRules,
			method: http.MethodPut, path: func(r *lagRig) string { return "/v1/customers/" + r.custID + "/billing-profile" },
			body: func(*lagRig) any {
				return map[string]any{
					"legal_name": "Lag Customer LLC", "address_line1": "1 Main St", "city": "Austin", "country": "US",
				}
			},
			at: addAt, want: http.StatusOK,
			unchanged: func(t *testing.T, r *lagRig) {
				if code, raw := r.do(http.MethodGet, "/v1/customers/"+r.custID+"/billing-profile", addAt, nil); code != http.StatusNotFound {
					t.Fatalf("billing profile: %d %s, want 404 (none saved)", code, raw)
				}
			},
			once: func(t *testing.T, r *lagRig) {
				bp := r.must(http.MethodGet, "/v1/customers/"+r.custID+"/billing-profile", addAt, nil, http.StatusOK)
				if bp["legal_name"] != "Lag Customer LLC" {
					t.Fatalf("billing profile legal_name %v, want Lag Customer LLC", bp["legal_name"])
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := c.rig(t)
			r.srv = lockLimitedServer(t, r.clk)
			const key = "idem-e2e-lock-wait-ended"
			var body any
			if c.body != nil {
				body = c.body(r)
			}

			admin := testutil.AdminPool(t)
			holder, err := admin.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = holder.Rollback() }()
			c.hold(t, holder, r)

			first := r.doKey(c.method, c.path(r), key, c.at, body)
			if err := holder.Rollback(); err != nil {
				t.Fatalf("release: %v", err)
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(first.Body.Bytes(), &env)
			if first.Code != http.StatusConflict || env.Error.Code != "concurrent_change" || first.Header().Get("Idempotent-Replayed") != "" {
				t.Fatalf("request whose lock wait ended: %d %s, want 409 concurrent_change from the handler", first.Code, first.Body.String())
			}
			c.unchanged(t, r)

			again := r.doKey(c.method, c.path(r), key, c.at, body)
			if again.Code != c.want || again.Header().Get("Idempotent-Replayed") != "" {
				t.Fatalf("resend with the same key: %d (replayed=%q) %s, want %d from the handler",
					again.Code, again.Header().Get("Idempotent-Replayed"), again.Body.String(), c.want)
			}
			c.once(t, r)
		})
	}
}

// lockLimitedServer is a server on a pool of its own, whose role has
// lock_timeout = 300ms: a member of the app role, so it has the app role's
// grants and the same row-level security. The pool connects to Postgres
// directly, at TEST_ADMIN_DATABASE_URL's address, because PgBouncer's test
// user list knows only the app role. The role and the pool end with the test.
func lockLimitedServer(t *testing.T, clk *clock.Fake) *Server {
	t.Helper()
	appURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if appURL == "" {
		appURL = "postgres://hardcount_test_app:hardcount_test_app@localhost:5432/hardcount_test?sslmode=disable"
	}
	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"
	}
	app, err := url.Parse(appURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	direct, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_ADMIN_DATABASE_URL: %v", err)
	}

	const password = "lock-wait-test-pw"
	role := testutil.CreateRole(t, "hardcount_lockwait", password, "IN ROLE "+app.User.Username())
	if _, err := testutil.AdminPool(t).ExecContext(t.Context(), `ALTER ROLE `+role+` SET lock_timeout = '300ms'`); err != nil {
		t.Fatalf("set the role's lock_timeout: %v", err)
	}
	direct.User = url.UserPassword(role, password)
	pool, err := sql.Open("pgx", direct.String())
	if err != nil {
		t.Fatalf("open the lock-limited pool: %v", err)
	}
	pool.SetMaxOpenConns(5)
	t.Cleanup(func() { _ = pool.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var limit string
	if err := pool.QueryRowContext(ctx, `SHOW lock_timeout`).Scan(&limit); err != nil || limit != "300ms" {
		t.Fatalf("lock-limited pool: lock_timeout %q (%v), want 300ms", limit, err)
	}
	return NewServer(postgres.NewDB(pool, 5*time.Second), clk, nil)
}
