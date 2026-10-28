package main

import (
	"database/sql"
	neturl "net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
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
	if _, err := adminPool.Exec(`DROP ROLE IF EXISTS hardcount_p7_bypass`); err != nil {
		t.Fatalf("drop role: %v", err)
	}
	if _, err := adminPool.Exec(`CREATE ROLE hardcount_p7_bypass WITH LOGIN BYPASSRLS PASSWORD 'p7-test-pw'`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(`DROP ROLE IF EXISTS hardcount_p7_bypass`) })

	bypassURL := rewriteUser(t, adminURL, "hardcount_p7_bypass", "p7-test-pw")
	bypassPool, err := sql.Open("pgx", bypassURL)
	if err != nil {
		t.Fatalf("open bypass: %v", err)
	}
	defer func() { _ = bypassPool.Close() }()
	role, canBypass, err = checkRLSCapability(bypassPool)
	if err != nil {
		t.Fatalf("capability(bypass): %v", err)
	}
	if role != "hardcount_p7_bypass" || !canBypass {
		t.Fatalf("role=%q canBypass=%v — BYPASSRLS role must be flagged", role, canBypass)
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
