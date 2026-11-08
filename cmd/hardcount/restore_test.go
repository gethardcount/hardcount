package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
)

// scripts/restore.sh marks the database in the restore's own transaction,
// with the statements `hardcount restore begin` runs. A change to one must
// reach the other.
func TestRestoreScriptMarksLikeBegin(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "restore.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, stmt := range restoreMarkStatements {
		want := strings.ReplaceAll(stmt, "$1::text", "'restore.sh'::text") + ";"
		if !strings.Contains(script, want) {
			t.Errorf("scripts/restore.sh does not run:\n%s", want)
		}
	}
	// pg_restore's SQL empties search_path, and the anchor reads a table
	// with row-level security, which binds an owner that is not a
	// superuser.
	for _, want := range []string{
		"SELECT pg_catalog.set_config('search_path', 'public', true);",
		"SELECT pg_catalog.set_config('app.bypass_rls', 'on', true);",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scripts/restore.sh does not run %s", want)
		}
	}
}

func TestCatchUpStart(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	anchor := sql.NullTime{Time: now.Add(-5 * 24 * time.Hour), Valid: true}
	fromAnchor := anchor.Time.Add(-72 * time.Hour)
	cases := []struct {
		name    string
		anchor  sql.NullTime
		since   time.Time
		want    time.Time
		wantErr string
	}{
		{"72 hours before the anchor", anchor, time.Time{}, fromAnchor, ""},
		{"--since earlier moves the start", anchor, fromAnchor.Add(-time.Hour), fromAnchor.Add(-time.Hour), ""},
		{"--since later does not", anchor, anchor.Time, fromAnchor, ""},
		{"no anchor takes --since", sql.NullTime{}, now.Add(-48 * time.Hour), now.Add(-48 * time.Hour), ""},
		{"no anchor and no --since", sql.NullTime{}, time.Time{}, time.Time{}, "--since"},
		{"older than 29 days", sql.NullTime{Time: now.Add(-27 * 24 * time.Hour), Valid: true}, time.Time{}, time.Time{}, "29 days"},
		{"--since older than 29 days", anchor, now.Add(-30 * 24 * time.Hour), time.Time{}, "29 days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := catchUpStart(tc.anchor, tc.since, now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("start = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestCatchUpExitCode(t *testing.T) {
	live := tenantstripe.Account{TenantID: "hc_ten_a", Livemode: true}
	test := tenantstripe.Account{TenantID: "hc_ten_a", Livemode: false}
	lost := []payment.LostPayment{{PaymentIntentID: "pi_1"}}
	acked := []payment.LostPayment{{PaymentIntentID: "pi_1", Acknowledged: true}}
	cases := []struct {
		name string
		accs []payment.AccountCatchUp
		want int
	}{
		{"clean", []payment.AccountCatchUp{{Account: live}}, catchUpDone},
		{"acknowledged payments", []payment.AccountCatchUp{{Account: live, Lost: acked}}, catchUpDone},
		{"a payment to handle", []payment.AccountCatchUp{{Account: live, Lost: lost}}, catchUpUnmatched},
		{"a refused live key", []payment.AccountCatchUp{{Account: live, KeyRefused: "Invalid API Key"}, {Account: test, Lost: lost}}, catchUpKeyRejected},
		{"a temporary error first", []payment.AccountCatchUp{{Account: live, KeyRefused: "x", Lost: lost}, {Account: tenantstripe.Account{TenantID: "hc_ten_b", Livemode: true}, Err: errors.New("timeout")}}, catchUpRetry},
		{"test mode never holds the database", []payment.AccountCatchUp{{Account: live}, {Account: test, Err: errors.New("x"), KeyRefused: "y", Lost: lost}}, catchUpDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := catchUpExitCode(payment.CatchUpReport{Accounts: tc.accs}); got != tc.want {
				t.Fatalf("exit code %d, want %d", got, tc.want)
			}
		})
	}
}

func TestStripeKeyFlag(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "key")
	if err := os.WriteFile(good, []byte("sk_live_current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "not-a-key")
	if err := os.WriteFile(bad, []byte("whsec_x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := stripeKeyFlag{}
	if err := f.Set("hc_ten_a:live=@" + good); err != nil {
		t.Fatal(err)
	}
	if got := f[tenantstripe.Account{TenantID: "hc_ten_a", Livemode: true}]; got != "sk_live_current" {
		t.Fatalf("key %q, want sk_live_current", got)
	}
	for _, v := range []string{"hc_ten_a:live=" + good, "hc_ten_a=@" + good, "hc_ten_a:prod=@" + good, ":live=@" + good, "hc_ten_a:test=@" + bad, "hc_ten_a:test=@" + filepath.Join(dir, "missing")} {
		if err := f.Set(v); err == nil {
			t.Errorf("--stripe-key %s was accepted", v)
		}
	}
}
