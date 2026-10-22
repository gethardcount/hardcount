package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// PlanState says what `velox migrate` would do with the database.
type PlanState string

const (
	// PlanEmpty: goose has not recorded 180; migration 180 runs the
	// baseline, then the files after it.
	PlanEmpty PlanState = "empty"
	// PlanBehind: `velox migrate` would apply Plan.Pending.
	PlanBehind PlanState = "behind"
	// PlanUpToDate: nothing to apply.
	PlanUpToDate PlanState = "up to date"
	// PlanAhead: the database has migrations this binary does not ship.
	PlanAhead PlanState = "ahead"
	// PlanUnrecorded: Velox tables, but goose has not recorded 180.
	// `velox migrate` refuses it (ErrNoMigrationRecord).
	PlanUnrecorded PlanState = "no migration record"
)

// PlanFile is one migration `velox migrate` would apply.
type PlanFile struct {
	Version int64
	Name    string
	NoTx    bool // an index build, which runs without a transaction
}

// PlanVersion is one applied version this binary does not ship.
type PlanVersion struct {
	Version   int64
	AppliedAt time.Time
}

// Plan is what `velox migrate` would do, read without changing anything.
// `velox migrate status` prints it before an upgrade.
type Plan struct {
	State PlanState
	// Database is the highest version goose recorded, 0 when none.
	Database int64
	// Latest is the highest version this binary ships.
	Latest  int64
	Pending []PlanFile    // in order; set for PlanEmpty and PlanBehind
	Ahead   []PlanVersion // set for PlanAhead
	// Running describes the process holding the migration lock now, ""
	// when none does.
	Running string
}

// ReadPlan reads the plan with a pool of its own. It takes no lock and
// creates nothing: it never calls goose's Status, HasPending or
// GetVersions, which create goose_db_version.
func ReadPlan(dsn string) (Plan, error) {
	db, err := openPool(dsn)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = db.Close() }()
	return readPlan(context.Background(), db, migrationFiles())
}

func readPlan(ctx context.Context, db *sql.DB, fsys fs.FS) (Plan, error) {
	st, err := readState(ctx, db)
	if err != nil {
		return Plan{}, fmt.Errorf("read migration state: %w", err)
	}
	files, err := embeddedFiles(db, fsys)
	if err != nil {
		return Plan{}, err
	}
	p := planFor(st, files)
	if p.State == PlanAhead {
		store, err := versionStore()
		if err != nil {
			return Plan{}, err
		}
		for i, a := range p.Ahead {
			m, err := store.GetMigration(ctx, db, a.Version)
			if err != nil {
				return Plan{}, err
			}
			p.Ahead[i].AppliedAt = m.Timestamp
		}
	}
	p.Running, err = lockHolder(ctx, db)
	return p, err
}

// embeddedFiles lists migration 180 and the files after it, as goose
// collects them, in version order. It never touches the database.
func embeddedFiles(db *sql.DB, fsys fs.FS) ([]PlanFile, error) {
	p, err := newProvider(db, fsys, false)
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	var out []PlanFile
	for _, s := range p.ListSources() {
		out = append(out, PlanFile{
			Version: s.Version,
			Name:    sourceName(s),
			NoTx:    s.Type == goose.TypeSQL && isNoTxFile(fsys, s.Path),
		})
	}
	return out, nil
}

// planFor is pure: it combines the state with the shipped files, which are
// in ascending version order.
func planFor(st state, files []PlanFile) Plan {
	var p Plan
	for _, f := range files {
		p.Latest = f.Version
	}
	shipped := map[int64]bool{}
	for _, f := range files {
		shipped[f.Version] = true
		if !st.has(f.Version) {
			p.Pending = append(p.Pending, f)
		}
	}
	for _, v := range st.applied {
		if !shipped[v] {
			p.Ahead = append(p.Ahead, PlanVersion{Version: v})
		}
	}
	p.Database = st.gooseVersion()
	switch {
	case decide(st) == actRefuse:
		p.State, p.Pending, p.Ahead = PlanUnrecorded, nil, nil
	case decide(st) == actBaseline:
		p.State = PlanEmpty
	case len(p.Ahead) > 0:
		p.State, p.Pending = PlanAhead, nil
	case len(p.Pending) > 0:
		p.State = PlanBehind
	default:
		p.State = PlanUpToDate
	}
	return p
}

// lockHolder names the session holding goose's migration lock in this
// database, or "" when none does.
func lockHolder(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT l.pid, coalesce(a.application_name, '')
		FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND l.classid = 0 AND l.objid = $1::bigint::oid AND l.objsubid = 1`, lock.DefaultLockID)
	if err != nil {
		return "", fmt.Errorf("read the migration lock: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var holders []string
	for rows.Next() {
		var pid int
		var app string
		if err := rows.Scan(&pid, &app); err != nil {
			return "", err
		}
		h := fmt.Sprintf("yes, pid %d", pid)
		if app != "" {
			h += " (" + app + ")"
		}
		holders = append(holders, h)
	}
	return strings.Join(holders, ", "), rows.Err()
}

// FormatPlan renders `velox migrate status`. Docs quote its lines.
func FormatPlan(p Plan, release string) string {
	var b strings.Builder
	row := func(label, value string) { fmt.Fprintf(&b, "%-14s%s\n", label+":", value) }

	switch p.State {
	case PlanEmpty:
		row("database", "empty")
	case PlanUnrecorded:
		row("database", "Velox tables, no migration record")
	default:
		row("database", fmt.Sprintf("%d (goose_db_version)", p.Database))
	}
	row("this release", fmt.Sprintf("%d (velox %s)", p.Latest, release))

	switch p.State {
	case PlanEmpty, PlanBehind:
		label := "behind"
		if p.State == PlanEmpty {
			label = "empty"
		}
		row("state", fmt.Sprintf("%s, %s to apply", label, countMigrations(len(p.Pending))))
	case PlanUpToDate:
		row("state", "up to date, nothing to apply")
	case PlanUnrecorded:
		row("state", "no migration record, velox migrate will not run")
	case PlanAhead:
		row("state", "ahead, the database has migrations this release does not ship")
		for _, a := range p.Ahead {
			fmt.Fprintf(&b, "  %05d   applied %s\n", a.Version, a.AppliedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
		}
	}
	for _, f := range p.Pending {
		if f.NoTx {
			fmt.Fprintf(&b, "  %-40s no transaction (index build)\n", f.Name)
		} else {
			fmt.Fprintf(&b, "  %s\n", f.Name)
		}
	}

	running := p.Running
	if running == "" {
		running = "no"
	}
	row("running now", running)

	switch p.State {
	case PlanEmpty, PlanBehind:
		b.WriteString(`next: take a backup, then run "velox migrate". A server started with RUN_MIGRATIONS_ON_BOOT=true runs it as it starts.` + "\n")
	case PlanAhead:
		b.WriteString("next: this binary is older than the database; to go back, restore the backup taken before the upgrade\n")
	case PlanUnrecorded:
		b.WriteString("next: restore the backup together with its migration table, or use an empty database. " +
			"Upgrading from Velox 0.8.x is not supported; install fresh. Steps: " + RunbookPath + "\n")
	}
	return b.String()
}

func countMigrations(n int) string {
	if n == 1 {
		return "1 migration"
	}
	return fmt.Sprintf("%d migrations", n)
}
