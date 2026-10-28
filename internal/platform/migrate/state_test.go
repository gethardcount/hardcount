package migrate

import "testing"

// decide runs goose only once goose has recorded 180, refuses tables goose
// has no record of, and refuses a database a build of Velox made.
// Mutations: decide on the goose table's presence instead (a rolled-back
// 180, row 0 only, then skips the baseline), drop the refusal (tables then
// get the baseline), or drop the Velox refusal (a Velox database then runs
// goose).
func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   state
		want action
	}{
		{"new database", state{}, actBaseline},
		{"goose table with only row 0 (a rolled-back 180)", state{gooseTable: true}, actBaseline},
		{"goose has 180", state{gooseTable: true, applied: []int64{180}}, actNormal},
		{"goose has 180 and later files", state{gooseTable: true, applied: []int64{180, 181, 182}}, actNormal},
		{"goose has 180 and tables", state{tenantsTable: true, gooseTable: true, applied: []int64{180, 181}}, actNormal},
		{"tables, no goose table (Velox 0.8.x, or restored without it)", state{tenantsTable: true}, actRefuse},
		{"tables, goose table with only row 0", state{tenantsTable: true, gooseTable: true}, actRefuse},
		{"tables, goose rows but not 180", state{tenantsTable: true, gooseTable: true, applied: []int64{181}}, actRefuse},
		{"goose has 180, made by Velox", state{tenantsTable: true, veloxSchema: true, gooseTable: true, applied: []int64{180, 181}}, actRefuseVelox},
	} {
		if got := decide(tc.st); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
