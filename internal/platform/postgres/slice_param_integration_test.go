package postgres_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestSliceParam_PgxEncodesStringSlice pins what the stores rely on when they
// pass a plain []string as a query parameter: pgx sends it as a text[] whose
// elements arrive unchanged, an empty slice as '{}' and a nil slice as NULL.
// It runs in every exec mode an operator can choose, and covers the three
// shapes the stores use: an explicit ::text[] cast, an uncast ANY($n) against
// a text column, and the COALESCE guard on a nil exclude list.
func TestSliceParam_PgxEncodesStringSlice(t *testing.T) {
	modes := []pgx.QueryExecMode{
		pgx.QueryExecModeCacheStatement,
		pgx.QueryExecModeCacheDescribe,
		pgx.QueryExecModeDescribeExec,
		pgx.QueryExecModeExec,
		pgx.QueryExecModeSimpleProtocol,
	}
	// Elements that an array literal must quote or escape.
	tricky := []string{"a", "", "NULL", "x,y", `q"uote`, `back\slash`, "{brace}", " spaced ", "ünï", "two\nlines"}

	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			db := testutil.OpenExecModeDB(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			t.Run("elements arrive unchanged", func(t *testing.T) {
				if got := unnest(ctx, t, db, tricky); !slices.Equal(got, tricky) {
					t.Fatalf("round trip = %q, want %q", got, tricky)
				}
			})

			t.Run("empty slice is an empty array", func(t *testing.T) {
				var isNull bool
				var n int
				if err := db.QueryRowContext(ctx,
					`SELECT $1::text[] IS NULL, cardinality($1::text[])`, []string{}).Scan(&isNull, &n); err != nil {
					t.Fatal(err)
				}
				if isNull || n != 0 {
					t.Fatalf("empty slice: isNull=%v cardinality=%d, want false 0", isNull, n)
				}
			})

			t.Run("nil slice is NULL and the COALESCE guard keeps every row", func(t *testing.T) {
				var isNull bool
				var kept int
				if err := db.QueryRowContext(ctx, `
					SELECT $1::text[] IS NULL,
					       (SELECT count(*) FROM (VALUES ('a'), ('b')) v(id)
					         WHERE NOT (id = ANY(COALESCE($1::text[], '{}'))))`,
					[]string(nil)).Scan(&isNull, &kept); err != nil {
					t.Fatal(err)
				}
				if !isNull || kept != 2 {
					t.Fatalf("nil slice: isNull=%v kept=%d, want true 2", isNull, kept)
				}
			})

			t.Run("uncast ANY against a text column", func(t *testing.T) {
				var matched int
				if err := db.QueryRowContext(ctx, `
					SELECT count(*) FROM (VALUES ('x,y'::text), ('a'), ('zzz')) v(status)
					 WHERE status = ANY($1)`,
					[]string{"x,y", "a"}).Scan(&matched); err != nil {
					t.Fatal(err)
				}
				if matched != 2 {
					t.Fatalf("matched %d rows, want 2", matched)
				}
			})
		})
	}
}

// unnest sends vals as one text[] parameter and reads its elements back as
// rows, in order.
func unnest(ctx context.Context, t *testing.T, db *sql.DB, vals []string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT e FROM unnest($1::text[]) WITH ORDINALITY AS u(e, i) ORDER BY i`, vals)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}
