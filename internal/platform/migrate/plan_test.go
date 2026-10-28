package migrate

import (
	"reflect"
	"testing"
	"time"
)

var planFiles = []PlanFile{
	{Version: 180, Name: baselineName},
	{Version: 181, Name: "00181_usage_rollup_index.sql", NoTx: true},
	{Version: 182, Name: "00182_add_invoice_memo.sql"},
}

func TestPlanFor(t *testing.T) {
	cases := []struct {
		name     string
		st       state
		state    PlanState
		database int64
		pending  []int64
		ahead    []int64
	}{
		{"empty", state{}, PlanEmpty, 0, []int64{180, 181, 182}, nil},
		{"behind", state{gooseTable: true, applied: []int64{180}}, PlanBehind, 180, []int64{181, 182}, nil},
		{"behind by one", state{gooseTable: true, applied: []int64{180, 181}}, PlanBehind, 181, []int64{182}, nil},
		{"up to date", state{gooseTable: true, applied: []int64{180, 181, 182}}, PlanUpToDate, 182, nil, nil},
		{"ahead", state{gooseTable: true, applied: []int64{180, 181, 182, 183, 184}}, PlanAhead, 184, nil, []int64{183, 184}},
		{"rolled-back 180: row 0 only", state{gooseTable: true}, PlanEmpty, 0, []int64{180, 181, 182}, nil},
		{"tables, no record", state{tenantsTable: true}, PlanUnrecorded, 0, nil, nil},
		{"tables, goose row 0 only", state{tenantsTable: true, gooseTable: true}, PlanUnrecorded, 0, nil, nil},
		{"tables, goose rows but not 180", state{tenantsTable: true, gooseTable: true, applied: []int64{183}}, PlanUnrecorded, 183, nil, nil},
		{"made by Velox, up to date", state{tenantsTable: true, veloxSchema: true, gooseTable: true, applied: []int64{180, 181, 182}}, PlanVelox, 182, nil, nil},
		{"made by Velox, behind", state{tenantsTable: true, veloxSchema: true, gooseTable: true, applied: []int64{180}}, PlanVelox, 180, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := planFor(tc.st, planFiles)
			if p.State != tc.state || p.Database != tc.database || p.Latest != 182 {
				t.Fatalf("got state %q database %d latest %d, want %q %d 182", p.State, p.Database, p.Latest, tc.state, tc.database)
			}
			var pending, ahead []int64
			for _, f := range p.Pending {
				pending = append(pending, f.Version)
			}
			for _, a := range p.Ahead {
				ahead = append(ahead, a.Version)
			}
			if !reflect.DeepEqual(pending, tc.pending) || !reflect.DeepEqual(ahead, tc.ahead) {
				t.Fatalf("pending %v ahead %v, want %v %v", pending, ahead, tc.pending, tc.ahead)
			}
		})
	}
}

func TestFormatPlan(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{
			name: "behind, with an index build",
			plan: Plan{State: PlanBehind, Database: 180, Latest: 182, Pending: planFiles[1:]},
			want: `database:     180 (goose_db_version)
this release: 182 (hardcount v0.9.1)
state:        behind, 2 migrations to apply
  00181_usage_rollup_index.sql             no transaction (index build)
  00182_add_invoice_memo.sql
running now:  no
next: take a backup, then run "hardcount migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "empty",
			plan: Plan{State: PlanEmpty, Latest: 180, Pending: planFiles[:1]},
			want: `database:     empty
this release: 180 (hardcount v0.9.1)
state:        empty, 1 migration to apply
  00180 baseline
running now:  no
next: take a backup, then run "hardcount migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.
`,
		},
		{
			name: "no migration record",
			plan: Plan{State: PlanUnrecorded, Latest: 182},
			want: `database:     tables, no migration record
this release: 182 (hardcount v0.9.1)
state:        no migration record, hardcount migrate will not run
running now:  no
next: restore the backup together with its migration table, or use an empty database. Upgrading from Velox 0.8.x is not supported; install fresh. Steps: docs/ops/runbook/migration-failed-midway.md
`,
		},
		{
			name: "made by Velox",
			plan: Plan{State: PlanVelox, Database: 182, Latest: 182},
			want: `database:     made by Velox (new rows get vlx_ ids)
this release: 182 (hardcount v0.9.1)
state:        made by Velox, hardcount migrate will not run
running now:  no
next: install fresh on an empty database; Hardcount does not take over a Velox database. Steps: docs/ops/runbook/migration-failed-midway.md#the-database-was-made-by-velox
`,
		},
		{
			name: "up to date, a migration running",
			plan: Plan{State: PlanUpToDate, Database: 182, Latest: 182, Running: "yes, pid 4242 (hardcount-migrate)"},
			want: `database:     182 (goose_db_version)
this release: 182 (hardcount v0.9.1)
state:        up to date, nothing to apply
running now:  yes, pid 4242 (hardcount-migrate)
`,
		},
		{
			name: "ahead",
			plan: Plan{State: PlanAhead, Database: 183, Latest: 182, Ahead: []PlanVersion{{183, time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)}}},
			want: `database:     183 (goose_db_version)
this release: 182 (hardcount v0.9.1)
state:        ahead, the database has migrations this release does not ship
  00183   applied 2026-11-02 09:30:00 UTC
running now:  no
next: this binary is older than the database; to go back, restore the backup taken before the upgrade
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatPlan(tc.plan, "v0.9.1"); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}
