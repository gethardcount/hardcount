// Package doctor is the money-invariant sweep: a read-only check of a
// Hardcount database for states that no legal writer can produce.
//
// It exists because no other gate sees the class of defect it catches.
// Manual walk-throughs prove flows and CI pins behaviours, but a row that
// breaks an invariant sits in the data and says nothing. ADR-111 records
// such a defect, found by one ad-hoc query of this kind months after the
// rows went bad. This package makes that kind of query permanent.
//
// The one rule (docs/dev/records/product-stability-plan.md): a check ships
// only as an exact predicate, with zero legal false positives, or it does not
// ship. Every check in checks.go passed a search for legal counterexamples:
// grandfathered rows, deliberate states (such as an invoice parked under
// ADR-107), simulated rows and best-effort stamps that lag. A doctor that
// raises false alarms gets ignored, and an ignored doctor is worse than none.
// When a fixing PR closes a new class of bug, it adds the class's invariant
// here in the same PR.
package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Check is one invariant: SQL whose result rows are violations. The query
// must select `id` first (the violating row's primary key), then any columns
// useful for triage. It runs with row-level security off and no tenant
// binding (see beginRead): the doctor sweeps across tenants by design.
type Check struct {
	// Name is the stable identifier, kebab-case, unique across domains.
	Name string
	// Domain groups checks for reporting (invoices, credit-ledger, …).
	Domain string
	// Why is one sentence: the writer that maintains this invariant and
	// what a violation means in money terms.
	Why string
	// SQL returns violating rows. The first column must be the row id.
	SQL string
}

// Violation is one row that a check flagged.
type Violation struct {
	Check  Check
	RowID  string
	Detail string // remaining selected columns, "col=val" joined
}

// Result is one full sweep.
type Result struct {
	Started    time.Time
	Duration   time.Duration
	Checks     int
	Violations []Violation
	// Errors are what keeps the sweep from vouching for the database: a
	// check that failed to run (schema drift, a connection error), or a read
	// that saw no customers and no invoices while tenants exist. Each is
	// itself a finding, and none is skipped without a word.
	Errors []error
}

// Run executes every check against db. It is read-only by construction:
// checks are SELECTs, and Run also opens each transaction read-only, so a
// mistyped check cannot write. Before the checks, it adds an error when
// tenants exist but it reads no customers and no invoices (see coverage).
func Run(ctx context.Context, db *sql.DB, checks []Check) Result {
	res := Result{Started: time.Now()}
	if err := coverage(ctx, db); err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("read coverage: %w", err))
	}
	for _, c := range checks {
		res.Checks++
		if err := runOne(ctx, db, c, &res); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("check %s: %w", c.Name, err))
		}
	}
	res.Duration = time.Since(res.Started)
	return res
}

// beginRead opens the read-only transaction that each check and the
// coverage probe run in, under postgres.TxBypass. Every tenant table has
// FORCE ROW LEVEL SECURITY, so without it any role that is neither a
// superuser nor BYPASSRLS, the tables' owner included, reads no tenant rows.
// The checks read every tenant on purpose and filter by none.
func beginRead(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := postgres.NewDB(db, 0).BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	// A transaction may turn read-only after its first statement, here the
	// bypass setting, and from then on it refuses every write.
	if _, err := tx.ExecContext(ctx, `SET TRANSACTION READ ONLY`); err != nil {
		postgres.Rollback(tx)
		return nil, err
	}
	return tx, nil
}

// coverage returns an error when tenants exist but the sweep's read sees no
// customers and no invoices. A sweep over such a read finds nothing whether
// or not the data is sound, so it must not pass. The tenants table has no
// row-level security, so its count does not depend on the role. Every
// account's billing rows hang off its customers, so an account with a
// customer and no invoice yet still gets a sweep.
func coverage(ctx context.Context, db *sql.DB) error {
	tx, err := beginRead(ctx, db)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	var tenants int64
	var customers, invoices bool
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM tenants),
		EXISTS (SELECT 1 FROM customers), EXISTS (SELECT 1 FROM invoices)`).Scan(&tenants, &customers, &invoices); err != nil {
		return err
	}
	if tenants > 0 && !customers && !invoices {
		return fmt.Errorf("%d tenant(s) exist, but the doctor read no customers and no invoices. "+
			"Either the DATABASE_URL user cannot read the rows, or no account has a customer yet. "+
			"The doctor reports OK only over rows it has read", tenants)
	}
	return nil
}

func runOne(ctx context.Context, db *sql.DB, c Check, res *Result) error {
	tx, err := beginRead(ctx, db)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, c.SQL)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		v := Violation{Check: c, RowID: str(vals[0])}
		for i := 1; i < len(cols); i++ {
			if v.Detail != "" {
				v.Detail += " "
			}
			v.Detail += cols[i] + "=" + str(vals[i])
		}
		res.Violations = append(res.Violations, v)
	}
	return rows.Err()
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}
