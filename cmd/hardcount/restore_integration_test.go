package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// insertWebhookEvent records a live-mode Stripe event received at `at`.
func insertWebhookEvent(t *testing.T, admin *sql.DB, tenantID, eventID string, at time.Time) {
	t.Helper()
	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }() // after Commit it does nothing
	for _, q := range []string{`SELECT set_config('app.bypass_rls', 'on', true)`, `SELECT set_config('app.livemode', 'on', true)`} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO stripe_webhook_events (tenant_id, livemode, stripe_event_id, event_type, object_type, received_at)
		VALUES ($1, true, $2, 'payment_intent.succeeded', 'payment_intent', $3)`, tenantID, eventID, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// pausedBy returns who paused the role, and "" when it is not paused.
func pausedBy(t *testing.T, admin *sql.DB, role string) string {
	t.Helper()
	var by sql.NullString
	if err := admin.QueryRow(`SELECT paused_by FROM leader_leases WHERE role = $1`, role).Scan(&by); err != nil {
		t.Fatalf("read the %s lease: %v", role, err)
	}
	return by.String
}

// `hardcount restore begin` marks the database with its anchor, the newest
// instant the restored data shows Hardcount running, and pauses the two
// jobs that charge cards, billing and dunning. The anchor is read before
// the pauses, which clear the heartbeats. A second begin keeps the first
// marker, even after newer rows arrive, and the catch-up's clear lets
// serve start.
func TestRestoreBegin_AnchorPauseAndClear(t *testing.T) {
	testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 19, 3, 0, 0, 0, time.UTC)

	// Billing holds its lease with the newest heartbeat. Dunning's last tick
	// and the newest webhook are older.
	mustExec(t, admin, `INSERT INTO leader_leases (role, holder_token, holder_id, acquired_at, heartbeat_at, expires_at, last_tick_ended_at)
		VALUES ('billing', 1, 'copy-1', $1, $2, $3, $4)`, base.Add(-time.Minute), base.Add(-2*time.Second), base.Add(8*time.Second), base.Add(-time.Hour))
	mustExec(t, admin, `INSERT INTO leader_leases (role, holder_token, last_tick_ended_at) VALUES ('dunning', 1, $1)`, base.Add(-30*time.Second))
	tenantID := testutil.CreateTestTenant(t, postgres.NewDB(admin, 5*time.Second), "restore-begin")
	insertWebhookEvent(t, admin, tenantID, "evt_old", base.Add(-10*time.Minute))

	owner := postgres.NewDB(admin, 5*time.Second)
	m, err := markRestored(ctx, owner, "hardcount restore begin", false)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Anchor.Valid || !m.Anchor.Time.Equal(base.Add(-2*time.Second)) || m.SetBy != "hardcount restore begin" {
		t.Fatalf("marker %+v, want anchor %s (billing's heartbeat) set by hardcount restore begin", m, base.Add(-2*time.Second))
	}
	for _, role := range []string{"billing", "dunning"} {
		if by := pausedBy(t, admin, role); by != "restore" {
			t.Fatalf("%s paused by %q, want restore", role, by)
		}
	}

	// A newer webhook, then begin again: the first marker stays.
	insertWebhookEvent(t, admin, tenantID, "evt_new", base.Add(time.Hour))
	again, err := markRestored(ctx, owner, "restore.sh", false)
	if err != nil {
		t.Fatal(err)
	}
	if again.SetBy != m.SetBy || !again.Anchor.Time.Equal(m.Anchor.Time) {
		t.Fatalf("second begin changed the marker to %+v, want %+v", again, m)
	}

	read, restored, err := readRestoreMarker(ctx, admin)
	if err != nil || !restored || !read.Anchor.Time.Equal(m.Anchor.Time) {
		t.Fatalf("read %+v %v %v, want the marker", read, restored, err)
	}
	if err := clearRestoreMarker(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if _, restored, err := readRestoreMarker(ctx, admin); err != nil || restored {
		t.Fatalf("after the clear: restored=%v err=%v, want no marker", restored, err)
	}
}

// serve checks the marker only when it starts. So `restore begin` refuses a
// database a copy of Hardcount is running on: the copy would keep charging,
// and its lease writes would move the anchor past the backup. A lease
// written a few seconds ago shows the copy. --force marks it anyway.
func TestRestoreBegin_RefusesADatabaseACopyRunsOn(t *testing.T) {
	testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := context.Background()
	mustExec(t, admin, `INSERT INTO leader_leases (role, holder_token, last_tick_ended_at) VALUES ('webhook_outbox', 1, now() - interval '3 seconds')`)
	mustExec(t, admin, `INSERT INTO leader_leases (role, holder_token) VALUES ('billing', 1), ('dunning', 1)`)
	owner := postgres.NewDB(admin, 5*time.Second)

	if _, err := markRestored(ctx, owner, "hardcount restore begin", false); !errors.Is(err, errDatabaseServed) {
		t.Fatalf("begin on a database a copy runs on: %v, want errDatabaseServed", err)
	}
	if _, restored, err := readRestoreMarker(ctx, admin); err != nil || restored {
		t.Fatalf("after the refusal: restored=%v err=%v, want no marker", restored, err)
	}
	if by := pausedBy(t, admin, "billing"); by != "" {
		t.Fatalf("billing paused by %q after the refusal, want not paused", by)
	}

	m, err := markRestored(ctx, owner, "hardcount restore begin", true)
	if err != nil || m.SetBy != "hardcount restore begin" {
		t.Fatalf("begin --force: %+v, %v, want the database marked", m, err)
	}
}

// --stripe-key for a tenant the restored database does not hold is refused:
// every event names the real tenant, so each would be skipped and the run
// would end with nothing applied.
func TestUnknownTenants(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	known := testutil.CreateTestTenant(t, db, "restore-known")
	keys := stripeKeyFlag{
		{TenantID: known, Livemode: true}:                 "sk_live_a",
		{TenantID: known, Livemode: false}:                "sk_test_a",
		{TenantID: "hc_ten_typo", Livemode: true}:         "sk_live_b",
		{TenantID: "hc_ten_after_backup", Livemode: true}: "sk_live_c",
	}
	got, err := unknownTenants(context.Background(), admin, keys)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "hc_ten_after_backup,hc_ten_typo" {
		t.Fatalf("unknown tenants %v, want hc_ten_after_backup and hc_ten_typo", got)
	}
}

// The limited app role reads the marker and cannot change it, so no request
// can clear it (migration 00223). The boot's grant check asks for no more
// than that, or serve would refuse to start.
func TestRestoreMarker_AppRoleReadsOnly(t *testing.T) {
	testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	var sel, ins, upd, del bool
	if err := admin.QueryRow(`SELECT has_table_privilege('hardcount_app', 'public.restore_catchup', 'SELECT'),
		has_table_privilege('hardcount_app', 'public.restore_catchup', 'INSERT'),
		has_table_privilege('hardcount_app', 'public.restore_catchup', 'UPDATE'),
		has_table_privilege('hardcount_app', 'public.restore_catchup', 'DELETE')`).Scan(&sel, &ins, &upd, &del); err != nil {
		t.Fatal(err)
	}
	if !sel || ins || upd || del {
		t.Fatalf("hardcount_app on restore_catchup: select=%v insert=%v update=%v delete=%v, want read only", sel, ins, upd, del)
	}
	have := map[string]bool{"SELECT": sel, "INSERT": ins, "UPDATE": upd, "DELETE": del}
	needs, ok := appTableNeeds["restore_catchup"]
	if !ok {
		t.Fatal("appTableNeeds has no entry for restore_catchup, so the boot check asks for full read-write and refuses to start")
	}
	for _, p := range needs {
		if !have[p] {
			t.Errorf("the boot check asks for %s on restore_catchup, which the migration does not grant", p)
		}
	}
}
