package migrate

import (
	"reflect"
	"testing"
)

// TestPlanFor covers every state against a small shipped set with a gap
// (3 is not shipped), the shape of the real set's 0143 and 0166.
func TestPlanFor(t *testing.T) {
	files := []upFile{
		{1, "0001_init.up.sql"},
		{2, "0002_users.up.sql"},
		{4, "0004_invoices.up.sql"},
		{5, "0005_credits.up.sql"},
	}
	cases := []struct {
		name    string
		dbVer   uint
		dirty   bool
		state   PlanState
		pending []string
	}{
		{"new database", 0, false, PlanBehind, []string{"0001_init.up.sql", "0002_users.up.sql", "0004_invoices.up.sql", "0005_credits.up.sql"}},
		{"behind across the gap", 2, false, PlanBehind, []string{"0004_invoices.up.sql", "0005_credits.up.sql"}},
		{"behind by one", 4, false, PlanBehind, []string{"0005_credits.up.sql"}},
		{"up to date", 5, false, PlanUpToDate, nil},
		{"ahead", 7, false, PlanAhead, nil},
		{"a version this binary does not ship", 3, false, PlanUnknown, nil},
		{"dirty at the latest", 5, true, PlanDirty, nil},
		{"dirty wins over ahead", 7, true, PlanDirty, nil},
		{"dirty before anything applied", 0, true, PlanDirty, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planFor(c.dbVer, c.dirty, files)
			if p.State != c.state {
				t.Errorf("state: got %q, want %q", p.State, c.state)
			}
			if !reflect.DeepEqual(p.Pending, c.pending) {
				t.Errorf("pending: got %v, want %v", p.Pending, c.pending)
			}
			if p.Database != c.dbVer || p.Dirty != c.dirty || p.Latest != 5 {
				t.Errorf("got database %d dirty %v latest %d, want %d %v 5", p.Database, p.Dirty, p.Latest, c.dbVer, c.dirty)
			}
		})
	}
}

// TestEmbeddedUpFilesMatchesVersions ties the plan's file list to the
// versions the runner and CheckSchemaReady use: same order, same latest,
// and a database at the latest version plans nothing.
func TestEmbeddedUpFilesMatchesVersions(t *testing.T) {
	files, err := embeddedUpFiles()
	if err != nil {
		t.Fatal(err)
	}
	versions, err := embeddedUpVersions()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(versions) {
		t.Fatalf("%d files, %d versions", len(files), len(versions))
	}
	for i, f := range files {
		if f.version != versions[i] {
			t.Fatalf("file %d: version %d, want %d", i, f.version, versions[i])
		}
		if m := migrationFilenamePattern.FindStringSubmatch(f.name); len(m) != 2 {
			t.Fatalf("file %d: name %q is not an .up.sql migration", i, f.name)
		}
	}
	latest, err := EmbeddedLatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	p := planFor(latest, false, files)
	if p.Latest != latest || p.State != PlanUpToDate || len(p.Pending) != 0 {
		t.Fatalf("at the latest version: got %+v", p)
	}
	if p := planFor(0, false, files); len(p.Pending) != len(files) || p.Pending[0] != files[0].name {
		t.Fatalf("new database: %d pending, want all %d starting with %s", len(p.Pending), len(files), files[0].name)
	}
}
