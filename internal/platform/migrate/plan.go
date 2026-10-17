package migrate

// PlanState says how the database's recorded version compares with the
// migrations this binary ships.
type PlanState string

const (
	// PlanUpToDate: the database is at this binary's latest version.
	PlanUpToDate PlanState = "up to date"
	// PlanBehind: `velox migrate` would apply Plan.Pending.
	PlanBehind PlanState = "behind"
	// PlanAhead: the database is at a version newer than any this binary
	// ships, so a newer release migrated it.
	PlanAhead PlanState = "ahead"
	// PlanDirty: a migration started and did not finish. Nothing can be
	// planned until it is repaired (DirtyError says how).
	PlanDirty PlanState = "dirty"
	// PlanUnknown: the database records a version below this binary's
	// latest that the binary does not ship. golang-migrate cannot step on
	// from a version it has no file for, so `velox migrate` fails.
	PlanUnknown PlanState = "unknown"
)

// Plan is what `velox migrate` would do to a database, read without
// changing anything. `velox migrate status` prints it before an upgrade.
type Plan struct {
	Database uint // version in schema_migrations; 0 when none is applied
	Dirty    bool
	Latest   uint      // highest version this binary ships
	Pending  []string  // .up.sql files `velox migrate` would apply, in order; set only when State is PlanBehind
	State    PlanState // how Database compares with the shipped files
}

// ReadPlan reads the database's recorded version and compares it with the
// migrations embedded in this binary. It writes nothing and takes no lock:
// it does not create schema_migrations on a new database.
func ReadPlan(dsn string) (Plan, error) {
	v, dirty, err := Status(dsn)
	if err != nil {
		return Plan{}, err
	}
	files, err := embeddedUpFiles()
	if err != nil {
		return Plan{}, err
	}
	return planFor(v, dirty, files), nil
}

// planFor compares a recorded version with the shipped files, which are in
// ascending version order. Pending follows golang-migrate's Up: every file
// above the recorded version.
func planFor(dbVer uint, dirty bool, files []upFile) Plan {
	p := Plan{Database: dbVer, Dirty: dirty}
	shipped := dbVer == 0 // 0 means nothing applied, not a file
	for _, f := range files {
		p.Latest = f.version
		if f.version == dbVer {
			shipped = true
		}
	}
	switch {
	case dirty:
		p.State = PlanDirty
	case dbVer > p.Latest:
		p.State = PlanAhead
	case !shipped:
		p.State = PlanUnknown
	default:
		for _, f := range files {
			if f.version > dbVer {
				p.Pending = append(p.Pending, f.name)
			}
		}
		p.State = PlanUpToDate
		if len(p.Pending) > 0 {
			p.State = PlanBehind
		}
	}
	return p
}
