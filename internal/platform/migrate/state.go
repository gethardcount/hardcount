package migrate

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/pressly/goose/v3/database"
)

// The version record is public.goose_db_version, which goose writes: one
// row per applied migration, plus row 0.
//
// readState reads it, and whether public.tenants exists, read-only: no
// lock, and it creates nothing (goose's own Status, HasPending and
// GetVersions all create goose_db_version). decide turns what it read into
// an action. Up, Down, CheckSchemaReady and `velox migrate status` all use
// the same pair.
//
// Nothing reads public.schema_migrations, the record of Velox 0.8.x. A
// database that moved to goose may still have it; it is left alone.

// state is what readState found.
type state struct {
	// veloxTables: public.tenants exists. The baseline creates it, so a
	// database without it has none of Velox's tables.
	veloxTables bool
	gooseTable  bool
	// applied lists the versions in goose_db_version, ascending, without
	// row 0.
	applied []int64
}

func (s state) has(version int64) bool {
	for _, v := range s.applied {
		if v == version {
			return true
		}
	}
	return false
}

// gooseVersion is the highest version goose recorded, 0 when none.
func (s state) gooseVersion() int64 {
	if len(s.applied) == 0 {
		return 0
	}
	return s.applied[len(s.applied)-1]
}

// versionStore reads public.goose_db_version the way goose does.
func versionStore() (database.StoreExtender, error) {
	s, err := database.NewStore(database.DialectPostgres, versionTable)
	if err != nil {
		return nil, err
	}
	ext, ok := s.(database.StoreExtender)
	if !ok {
		return nil, errors.New("goose's postgres store cannot check whether its table exists")
	}
	return ext, nil
}

// readState reads in one read-only REPEATABLE READ transaction, so all
// three reads see the same snapshot. Read one by one, a check could miss
// row 180 and then see the tenants table that another copy's baseline
// committed in between, and refuse a database that is fine.
func readState(ctx context.Context, db *sql.DB) (state, error) {
	var st state
	store, err := versionStore()
	if err != nil {
		return st, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }()
	// pg_tables, not to_regclass: to_regclass reads the catalog as of now,
	// not as of the transaction's snapshot.
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'tenants')`,
	).Scan(&st.veloxTables); err != nil {
		return st, err
	}
	st.gooseTable, err = store.TableExists(ctx, tx)
	if err != nil {
		return st, err
	}
	if !st.gooseTable {
		return st, nil
	}
	rows, err := store.ListMigrations(ctx, tx)
	if err != nil {
		return st, err
	}
	for _, r := range rows {
		if r.Version != 0 {
			st.applied = append(st.applied, r.Version)
		}
	}
	sort.Slice(st.applied, func(i, j int) bool { return st.applied[i] < st.applied[j] })
	return st, nil
}

// action is what a command does with a state.
type action int

const (
	actNormal   action = iota + 1 // goose has 180: run goose as usual
	actBaseline                   // no Velox tables: migration 180 runs the baseline
	actRefuse                     // Velox tables, no 180: no command migrates it
)

// decide is pure:
//
//	goose has 180               ->  run goose
//	Velox tables, no 180        ->  refuse (ErrNoMigrationRecord)
//	otherwise                   ->  migration 180 runs the baseline
//
// "Refuse" covers a Velox 0.8.x database and a backup restored without its
// migration table: Velox cannot tell which version the tables are at.
// "Otherwise" covers no goose table and one holding only row 0, which a
// rolled-back migration 180 leaves behind.
func decide(s state) action {
	switch {
	case s.has(baselineVersion):
		return actNormal
	case s.veloxTables:
		return actRefuse
	}
	return actBaseline
}
