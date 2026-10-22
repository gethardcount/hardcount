package migrate

import (
	"context"
	"database/sql"
	"testing"
)

// The block usage cap is cut (#973): migration 191 drops
// subscriptions.usage_cap_units and overage_action, and its down puts them
// back, so a rollback and a second up both work. The down goes to 190, not
// one step, so a later migration does not take 191's place. Mutation: an up
// that keeps either column turns this red.
func TestUsageCapColumnsDropped(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_usage_cap")
	if err := Up(dsn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := capColumns(t, db); len(got) != 0 {
		t.Fatalf("after up, subscriptions still has %v", got)
	}

	if _, err := down(context.Background(), dsn, migrationFiles(), 190); err != nil {
		t.Fatalf("down to 190: %v", err)
	}
	if got := capColumns(t, db); len(got) != 2 {
		t.Fatalf("after down, cap columns = %v, want both back", got)
	}

	if err := Up(dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := capColumns(t, db); len(got) != 0 {
		t.Fatalf("after the second up, subscriptions still has %v", got)
	}
}

func capColumns(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'subscriptions'
		  AND column_name IN ('usage_cap_units', 'overage_action') ORDER BY column_name`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns: %v", err)
	}
	return out
}
