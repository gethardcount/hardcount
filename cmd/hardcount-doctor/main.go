// hardcount-doctor sweeps a Hardcount database for money-invariant
// violations: states that no legal writer can produce. It only reads. It
// exits 1 if it finds a violation, and 2 if a check failed to run.
//
//	DATABASE_URL="postgres://…" go run ./cmd/hardcount-doctor
//
// DATABASE_URL must be a role that sees every row, such as the table owner
// or an admin. An RLS-scoped app role (hardcount_app) sees nothing, so the
// doctor would report a clean result over rows it cannot see.
//
// See internal/doctor for the invariant catalog and the exactness rule, and
// docs/dev/records/product-stability-plan.md for why the doctor exists.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/gethardcount/hardcount/internal/doctor"
)

func main() {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "hardcount-doctor: DATABASE_URL is required")
		os.Exit(2)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hardcount-doctor: open: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res := doctor.Run(ctx, db, doctor.Checks)

	fmt.Printf("hardcount-doctor: %d checks in %s\n", res.Checks, res.Duration.Round(time.Millisecond))
	for _, e := range res.Errors {
		fmt.Printf("  CHECK-ERROR %v\n", e)
	}
	if len(res.Violations) == 0 && len(res.Errors) == 0 {
		fmt.Println("  OK — no invariant violations")
		return
	}
	byCheck := map[string]int{}
	for _, v := range res.Violations {
		byCheck[v.Check.Name]++
		fmt.Printf("  VIOLATION [%s/%s] %s  %s\n      why: %s\n",
			v.Check.Domain, v.Check.Name, v.RowID, v.Detail, v.Check.Why)
	}
	fmt.Printf("hardcount-doctor: %d violation(s) across %d check(s)\n", len(res.Violations), len(byCheck))
	if len(res.Errors) > 0 {
		os.Exit(2)
	}
	os.Exit(1)
}
