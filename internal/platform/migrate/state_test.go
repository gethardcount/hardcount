package migrate

import "testing"

// decide runs goose only once goose has recorded 180, and refuses Hardcount
// tables goose has no record of. Mutations: decide on the goose table's
// presence instead (a rolled-back 180, row 0 only, then skips the
// baseline), or drop the refusal (Hardcount tables then get the baseline).
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
		{"goose has 180 and Hardcount tables", state{hardcountTables: true, gooseTable: true, applied: []int64{180, 181}}, actNormal},
		{"Hardcount tables, no goose table (v0.8.x, or restored without it)", state{hardcountTables: true}, actRefuse},
		{"Hardcount tables, goose table with only row 0", state{hardcountTables: true, gooseTable: true}, actRefuse},
		{"Hardcount tables, goose rows but not 180", state{hardcountTables: true, gooseTable: true, applied: []int64{181}}, actRefuse},
	} {
		if got := decide(tc.st); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
