package migrate

import "testing"

// decide runs goose only once goose has recorded 180. Mutation: decide on
// the goose table's presence instead; a rolled-back 180 (row 0 only) then
// skips the baseline.
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
	} {
		if got := decide(tc.st); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
