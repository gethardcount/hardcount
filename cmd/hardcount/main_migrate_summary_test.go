package main

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/migrate"
)

// `hardcount migrate` says what it did; it printed "migrations applied
// successfully" when nothing ran (#1245).
func TestMigrateSummaryLine(t *testing.T) {
	for _, tc := range []struct {
		sum  migrate.UpSummary
		want string
	}{
		{migrate.UpSummary{Applied: 0, Version: 214}, "already up to date: the database is at version 214"},
		{migrate.UpSummary{Applied: 1, Version: 181}, "applied 1 migration: the database is at version 181"},
		{migrate.UpSummary{Applied: 2, Version: 181}, "applied 2 migrations: the database is at version 181"},
	} {
		if got := migrateSummaryLine(tc.sum); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.sum, got, tc.want)
		}
	}
}
