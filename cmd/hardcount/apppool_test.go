package main

import (
	"bytes"
	"database/sql"
	"log/slog"
	neturl "net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/gethardcount/hardcount/internal/testutil"
)

// P7 / ADR-073: APP_DATABASE_URL fail-closed matrix. resolveAppURL is
// pure so every branch is locked here; openAppPool's os.Exit sites are
// thin wrappers over these decisions.
func TestResolveAppURL_Matrix(t *testing.T) {
	admin := "postgres://hardcount:hardcount@db:5432/hardcount?sslmode=disable"
	strong := "postgres://hardcount_app:s3cure-Str0ng-pw@db:5432/hardcount?sslmode=disable"

	cases := []struct {
		name         string
		env          string
		appURL       string
		wantURL      string
		wantFallback bool
		wantErr      string // substring; "" = no error
	}{
		{"prod explicit strong", "production", strong, strong, false, ""},
		{"staging explicit strong", "staging", strong, strong, false, ""},
		{"prod unset", "production", "", "", false, "APP_DATABASE_URL is required"},
		{"staging unset", "staging", "", "", false, "APP_DATABASE_URL is required"},
		{"prod default password", "production", "postgres://hardcount_app:hardcount_app@db:5432/hardcount", "", false, "default/guessable password"},
		{"prod password equals username", "production", "postgres://app:app@db:5432/hardcount", "", false, "default/guessable password"},
		{"prod empty password (trust auth)", "production", "postgres://hardcount_app@db:5432/hardcount", "", false, "no password"},
		{"prod unparseable", "production", "postgres://hardcount_app:pw@db:5432/hardcount\x7f??", "", false, "not a parseable"},
		{"local unset derives", "local", "", "postgres://hardcount_app:hardcount_app@db:5432/hardcount?sslmode=disable", false, ""},
		{"local explicit default password allowed", "local", "postgres://hardcount_app:hardcount_app@db:5432/hardcount", "postgres://hardcount_app:hardcount_app@db:5432/hardcount", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, fallbackWarn, err := resolveAppURL(tc.env, admin, tc.appURL)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (fallbackWarn != "") != tc.wantFallback {
				t.Fatalf("fallback = %q, want fallback=%v", fallbackWarn, tc.wantFallback)
			}
			if url != tc.wantURL {
				t.Fatalf("url = %q, want %q", url, tc.wantURL)
			}
		})
	}

	// Local with an underivable admin URL falls back to the admin pool
	// with a warning (single-operator dev convenience, unchanged).
	url, warn, err := resolveAppURL("local", "not-a-url-at-all%%%\x7f", "")
	if err != nil || url != "" || warn == "" {
		t.Fatalf("local underivable: url=%q warn=%q err=%v — want admin-pool fallback warning", url, warn, err)
	}
}

// TestCheckRLSCapability_FlagsBypassRoles: the capability check is what
// catches APP_DATABASE_URL pointed at the admin/superuser role — the
// post-fix path of least resistance no string check can see.
func TestCheckRLSCapability_FlagsBypassRoles(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"
	}
	adminPool, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer func() { _ = adminPool.Close() }()

	// The admin role itself (superuser in the dev compose) must be
	// flagged as bypass-capable.
	role, canBypass, err := checkRLSCapability(adminPool)
	if err != nil {
		t.Fatalf("capability(admin): %v", err)
	}
	if !canBypass {
		t.Fatalf("admin role %q not flagged bypass-capable — the copied-DATABASE_URL misconfig would boot", role)
	}

	// A BYPASSRLS (non-superuser) role must also be flagged.
	bypassRole := testutil.CreateRole(t, "hardcount_p7_bypass", "p7-test-pw", "BYPASSRLS")
	bypassURL := rewriteUser(t, adminURL, bypassRole, "p7-test-pw")
	bypassPool, err := sql.Open("pgx", bypassURL)
	if err != nil {
		t.Fatalf("open bypass: %v", err)
	}
	defer func() { _ = bypassPool.Close() }()
	role, canBypass, err = checkRLSCapability(bypassPool)
	if err != nil {
		t.Fatalf("capability(bypass): %v", err)
	}
	if role != bypassRole || !canBypass {
		t.Fatalf("role=%q canBypass=%v — BYPASSRLS role must be flagged", role, canBypass)
	}
}

// TestWarnIfNoIdleTxTimeout_WarnsOnlyAtZero: a role whose
// idle_in_transaction_session_timeout is 0 gets one boot warning that names
// the setting and the page that says how to set it. A role with a value gets
// none.
// Mutation-verify: drop the warning → the "at 0" case fails; warn whatever the
// value → the "30s" case fails.
func TestWarnIfNoIdleTxTimeout_WarnsOnlyAtZero(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	adminURL := testDBURL("TEST_ADMIN_DATABASE_URL",
		"postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable")
	admin := testutil.AdminPool(t)

	cases := []struct {
		name, prefix, value string
		wantWarn            bool
	}{
		// Set to 0 on the role itself, so the case does not depend on the
		// server's or the database's default.
		{"at 0", "hardcount_idle_tx_off", "0", true},
		{"30s", "hardcount_idle_tx_on", "'30s'", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role := testutil.CreateRole(t, tc.prefix, "idle-tx-test-pw", "")
			q := "ALTER ROLE " + role + " SET idle_in_transaction_session_timeout = " + tc.value
			if _, err := admin.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			pool, err := sql.Open("pgx", rewriteUser(t, adminURL, role, "idle-tx-test-pw"))
			if err != nil {
				t.Fatalf("open role pool: %v", err)
			}
			defer func() { _ = pool.Close() }()

			var out bytes.Buffer
			warnIfNoIdleTxTimeout(slog.New(slog.NewTextHandler(&out, nil)), pool, role)

			got := out.String()
			if !tc.wantWarn {
				if got != "" {
					t.Fatalf("role with the timeout at %s logged %q, want nothing", tc.value, got)
				}
				return
			}
			if n := strings.Count(got, "level=WARN"); n != 1 {
				t.Fatalf("logged %q, want exactly one WARN line", got)
			}
			for _, want := range []string{
				"idle_in_transaction_session_timeout",
				"docs/ops/postgres-requirements.md#timeouts-on-the-limited-user",
				"role=" + role,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("warning %q does not contain %q", got, want)
				}
			}
		})
	}
}

// TestWarnIfNoIdleTxTimeout_ReadFailureIsLogged: when the setting cannot be
// read, the boot says the check did not run, and does not pass as if the
// timeout were set.
func TestWarnIfNoIdleTxTimeout_ReadFailureIsLogged(t *testing.T) {
	pool, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A closed pool fails the read without dialling anything.
	if err := pool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var out bytes.Buffer
	warnIfNoIdleTxTimeout(slog.New(slog.NewTextHandler(&out, nil)), pool, "hardcount_app")
	got := out.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "idle_in_transaction_session_timeout") ||
		!strings.Contains(got, "database is closed") {
		t.Fatalf("logged %q, want a WARN naming the setting and the read error", got)
	}
}

func rewriteUser(t *testing.T, dsn, user, pw string) string {
	t.Helper()
	u, err := neturl.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = neturl.UserPassword(user, pw)
	return u.String()
}
