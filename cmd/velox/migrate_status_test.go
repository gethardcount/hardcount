package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/migrate"
)

func TestFormatPlan(t *testing.T) {
	cases := []struct {
		name string
		plan migrate.Plan
		want string
	}{
		{
			name: "behind",
			plan: migrate.Plan{Database: 177, Latest: 180, State: migrate.PlanBehind,
				Pending: []string{"0178_a.up.sql", "0179_b.up.sql", "0180_c.up.sql"}},
			want: `database: 177
this release: 180 (velox v0.9.0)
state: behind, 3 migrations to apply
  0178_a.up.sql
  0179_b.up.sql
  0180_c.up.sql
next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "behind by one, new database",
			plan: migrate.Plan{Database: 0, Latest: 1, State: migrate.PlanBehind,
				Pending: []string{"0001_init.up.sql"}},
			want: `database: none applied
this release: 1 (velox v0.9.0)
state: behind, 1 migration to apply
  0001_init.up.sql
next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "up to date",
			plan: migrate.Plan{Database: 180, Latest: 180, State: migrate.PlanUpToDate},
			want: `database: 180
this release: 180 (velox v0.9.0)
state: up to date, nothing to apply
`,
		},
		{
			name: "ahead",
			plan: migrate.Plan{Database: 182, Latest: 180, State: migrate.PlanAhead},
			want: `database: 182
this release: 180 (velox v0.9.0)
state: ahead, the database has migrations this release does not ship
next: this release's code may not work with the newer schema. To go back to an older release, restore the backup you took before the upgrade.
`,
		},
		{
			name: "unknown",
			plan: migrate.Plan{Database: 143, Latest: 180, State: migrate.PlanUnknown},
			want: `database: 143
this release: 180 (velox v0.9.0)
state: unknown, this release does not ship migration 143, so "velox migrate" cannot continue from it
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatPlan(c.plan, "v0.9.0"); got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// A dirty database prints no plan: the repair comes first, so the output
// carries DirtyError's message with the file and the force commands.
func TestFormatPlanDirty(t *testing.T) {
	latest, err := migrate.EmbeddedLatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	got := formatPlan(migrate.Plan{Database: latest, Dirty: true, Latest: latest, State: migrate.PlanDirty}, "dev")
	head := fmt.Sprintf("database: %d (dirty)\nthis release: %d (velox dev)\nstate: dirty, a migration started and did not finish\n\n", latest, latest)
	if !strings.HasPrefix(got, head) {
		t.Fatalf("got:\n%s\nwant prefix:\n%s", got, head)
	}
	want := (&migrate.DirtyError{Version: latest}).Error()
	if strings.TrimPrefix(got, head) != want+"\n" {
		t.Fatalf("after the header, want the DirtyError message, got:\n%s", strings.TrimPrefix(got, head))
	}
	if !strings.Contains(want, "velox migrate force") || !strings.Contains(want, migrate.RunbookPath) {
		t.Fatalf("DirtyError message lacks the repair: %s", want)
	}
}
