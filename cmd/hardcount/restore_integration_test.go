package main

import (
	"context"
	"database/sql"
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

// `hardcount restore begin` marks the database with its anchor, the newest
// instant the restored data shows Hardcount running, and pauses billing. The
// anchor is read before the pause, which clears billing's heartbeat. A
// second begin keeps the first marker, even after newer rows arrive, and the
// catch-up's clear lets serve start.
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
	m, err := markRestored(ctx, owner, "hardcount restore begin")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Anchor.Valid || !m.Anchor.Time.Equal(base.Add(-2*time.Second)) || m.SetBy != "hardcount restore begin" {
		t.Fatalf("marker %+v, want anchor %s (billing's heartbeat) set by hardcount restore begin", m, base.Add(-2*time.Second))
	}
	var pausedBy string
	if err := admin.QueryRow(`SELECT paused_by FROM leader_leases WHERE role = 'billing' AND paused_at IS NOT NULL`).Scan(&pausedBy); err != nil {
		t.Fatalf("billing is not paused: %v", err)
	}

	// A newer webhook, then begin again: the first marker stays.
	insertWebhookEvent(t, admin, tenantID, "evt_new", base.Add(time.Hour))
	again, err := markRestored(ctx, owner, "restore.sh")
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
