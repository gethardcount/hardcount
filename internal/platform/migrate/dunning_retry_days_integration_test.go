package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// retryDaysVersion is 00206_dunning_retry_days.sql; 00207 validates its
// CHECK.
const retryDaysVersion = 206

// retryDaysExec runs one statement past row-level security, in its own
// transaction, as the app's bypass transactions do.
func retryDaysExec(ctx context.Context, db *sql.DB, q string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', 'on', true)`); err != nil {
		return err
	}
	if _, err := tx.Exec(q); err != nil {
		return err
	}
	return tx.Commit()
}

// oldPolicySQL inserts a policy in the shape before 00206.
func oldPolicySQL(id string, grace, max int, schedule string) string {
	return `INSERT INTO dunning_policies (id, tenant_id, name, grace_period_days, max_retry_attempts, retry_schedule)
		VALUES ('` + id + `', 'ten_rd', '` + id + `', ` + strconv.Itoa(grace) + `, ` + strconv.Itoa(max) + `, '` + schedule + `'::jsonb)`
}

// TestRetryDays_ConvertsExactlyAndRollsBack: 00206 writes each schedule as
// one list of days, exactly: the first wait, then the waits the retries use,
// whatever Go duration spelling they had. Entries past the last retry are
// dropped. The CHECK then refuses any list that is not 1 to 15 whole days
// from 1 to 30, and the Down writes the three old columns back.
func TestRetryDays_ConvertsExactlyAndRollsBack(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_dunsched_b_migrate_convert")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, retryDaysVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", retryDaysVersion, err)
	}
	exec := func(q string) {
		t.Helper()
		if err := retryDaysExec(ctx, db, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('ten_rd', 'Retry days')`)
	// The policy writer before 00206 stored a schedule the caller left out
	// (allowed for one retry) as json.Marshal of a nil slice: JSON null.
	omitted, err := json.Marshal([]string(nil))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id         string
		grace, max int
		schedule   string
		want       string
	}{
		{"dpol_seed", 3, 3, `["96h","168h"]`, `[3, 4, 7]`},
		{"dpol_recipe", 3, 4, `["24h","72h","168h","336h"]`, `[3, 1, 3, 7]`},
		{"dpol_one", 1, 1, `[]`, `[1]`},
		{"dpol_one_omitted", 2, 1, string(omitted), `[2]`},
		// Entries past the last retry were never used, so they are dropped,
		// even one that is not whole days.
		{"dpol_unused", 2, 2, `["48h","1h30m","96h"]`, `[2, 2]`},
		// Other spellings of whole days.
		{"dpol_spellings", 5, 5, `["4320m","259200s","+48h","72h0m0s"]`, `[5, 3, 3, 2, 3]`},
		{"dpol_long", 30, 15, `["24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","720h"]`,
			`[30, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 30]`},
	}
	for _, c := range cases {
		exec(oldPolicySQL(c.id, c.grace, c.max, c.schedule))
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, c := range cases {
		var got string
		if err := db.QueryRow(`SELECT retry_days::text FROM dunning_policies WHERE id = $1`, c.id).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if got != c.want {
			t.Errorf("%s: retry_days %s, want %s", c.id, got, c.want)
		}
	}
	var validated bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = 'dunning_policies_retry_days_check'`).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Error("00207 left the retry_days CHECK not validated")
	}
	for _, gone := range []string{"grace_period_days", "retry_schedule", "max_retry_attempts"} {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name = 'dunning_policies' AND column_name = $1)`, gone).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("column %s is still there", gone)
		}
	}

	// The CHECK: 1 to 15 entries, each a whole number of days from 1 to 30.
	for _, bad := range []string{`[]`, `[0]`, `[31]`, `[1.5]`, `["3"]`, `[3, null]`, `[3.0]`, `[-1]`, `{"first": 3}`, `3`,
		`[1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1]`} {
		if err := retryDaysExec(ctx, db, `UPDATE dunning_policies SET retry_days = '`+bad+`'::jsonb WHERE id = 'dpol_seed'`); err == nil {
			t.Errorf("retry_days %s was accepted, want the CHECK to refuse it", bad)
		}
	}
	for _, ok := range []string{`[1]`, `[30]`, `[1,1,1,1,1,1,1,1,1,1,1,1,1,1,30]`} {
		if err := retryDaysExec(ctx, db, `UPDATE dunning_policies SET retry_days = '`+ok+`'::jsonb WHERE id = 'dpol_one'`); err != nil {
			t.Errorf("retry_days %s was refused: %v", ok, err)
		}
	}
	exec(`UPDATE dunning_policies SET retry_days = '[1]'::jsonb WHERE id = 'dpol_one'`)

	// Down: the three columns come back, holding the same schedule.
	if _, err := down(ctx, dsn, migrationFiles(), retryDaysVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", retryDaysVersion, err)
	}
	wantOld := map[string]string{
		"dpol_seed":        `3 3 ["96h", "168h"]`,
		"dpol_recipe":      `3 4 ["24h", "72h", "168h"]`,
		"dpol_one":         `1 1 []`,
		"dpol_one_omitted": `2 1 []`,
		"dpol_unused":      `2 2 ["48h"]`,
		"dpol_spellings":   `5 5 ["72h", "72h", "48h", "72h"]`,
	}
	for id, want := range wantOld {
		var grace, max int
		var schedule string
		if err := db.QueryRow(`SELECT grace_period_days, max_retry_attempts, retry_schedule::text FROM dunning_policies WHERE id = $1`, id).Scan(&grace, &max, &schedule); err != nil {
			t.Fatalf("%s after Down: %v", id, err)
		}
		if got := strconv.Itoa(grace) + " " + strconv.Itoa(max) + " " + schedule; got != want {
			t.Errorf("%s after Down: %s, want %s", id, got, want)
		}
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	var again string
	if err := db.QueryRow(`SELECT retry_days::text FROM dunning_policies WHERE id = 'dpol_recipe'`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if again != `[3, 1, 3, 7]` {
		t.Errorf("after Down and Up, retry_days %s, want [3, 1, 3, 7]", again)
	}
}

// TestRetryDays_RefusesWhatCannotConvertExactly: a policy whose retries use
// a wait that is not a whole number of days from 1 to 30, or that Velox's
// policy writer would have refused, stops 00206 before anything changes.
// The error names each policy and why. Once those rows are fixed, it runs.
func TestRetryDays_RefusesWhatCannotConvertExactly(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_dunsched_b_migrate_refuse")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, retryDaysVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", retryDaysVersion, err)
	}
	exec := func(q string) {
		t.Helper()
		if err := retryDaysExec(ctx, db, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('ten_rd', 'Retry days')`)
	exec(oldPolicySQL("dpol_fine", 3, 3, `["96h","168h"]`))
	exec(oldPolicySQL("dpol_hours", 3, 2, `["36h"]`))
	exec(oldPolicySQL("dpol_mixed", 3, 3, `["72h","1h30m"]`))
	exec(oldPolicySQL("dpol_long_wait", 3, 2, `["1440h"]`))
	exec(oldPolicySQL("dpol_grace", 0, 1, `[]`))
	exec(oldPolicySQL("dpol_count", 3, 16, `["24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h","24h"]`))
	exec(oldPolicySQL("dpol_short", 3, 3, `["72h"]`))
	exec(oldPolicySQL("dpol_garbage", 3, 2, `["soon"]`))
	// A schedule left out (JSON null) is an empty list: too short for 3.
	exec(oldPolicySQL("dpol_omitted_short", 3, 3, `null`))

	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("Up converted schedules that are not whole days")
	}
	for _, want := range []string{
		"dunning policy dpol_hours: retry_schedule entry 1 (36h) is not a whole number of days from 1 to 30",
		"dunning policy dpol_mixed: retry_schedule entry 2 (1h30m)",
		"dunning policy dpol_long_wait: retry_schedule entry 1 (1440h)",
		"dunning policy dpol_grace: grace_period_days 0 is outside 1 to 30",
		"dunning policy dpol_count: max_retry_attempts 16 is outside 1 to 15",
		"dunning policy dpol_short: retry_schedule has 1 entries; max_retry_attempts 3 needs 2",
		"dunning policy dpol_garbage: retry_schedule entry 1 (soon)",
		"dunning policy dpol_omitted_short: retry_schedule has 0 entries; max_retry_attempts 3 needs 2",
		"docs/ops/runbook/migration-failed-midway.md#00206-a-dunning-schedule-is-not-whole-days",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "dpol_fine") {
		t.Errorf("error names a policy that converts: %v", err)
	}
	// Nothing changed: the old columns are still there.
	var grace int
	if err := db.QueryRow(`SELECT grace_period_days FROM dunning_policies WHERE id = 'dpol_fine'`).Scan(&grace); err != nil {
		t.Fatalf("old columns gone after a refused Up: %v", err)
	}

	exec(`DELETE FROM dunning_policies WHERE id <> 'dpol_fine'`)
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up after fixing the rows: %v", err)
	}
}

// TestRetryDays_RunbookFixConverts: the runbook's fix for a refused 00206,
// run as written on a three-retry policy whose entry 1 is "36h", lets the
// upgrade run and keeps the policy's other waits. Neither the runbook nor
// the error offers deleting the policy: a policy with runs cannot be
// deleted, and deleting the account's default stops dunning for every
// customer without a policy of their own.
func TestRetryDays_RunbookFixConverts(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_dunsched_f2_migrate_runbook")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, retryDaysVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", retryDaysVersion, err)
	}
	for _, q := range []string{
		`INSERT INTO tenants (id, name) VALUES ('ten_rd', 'Retry days')`,
		oldPolicySQL("dpol_36h", 3, 3, `["36h","120h"]`),
	} {
		if err := retryDaysExec(ctx, db, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("Up converted a 36h wait")
	}
	offersDelete := regexp.MustCompile(`(?i)or delete`)
	if offersDelete.MatchString(err.Error()) {
		t.Errorf("the error offers deleting the policy:\n%v", err)
	}

	doc, rerr := os.ReadFile("../../../docs/ops/runbook/migration-failed-midway.md")
	if rerr != nil {
		t.Fatal(rerr)
	}
	_, section, ok := strings.Cut(string(doc), "\n## 00206: a dunning schedule is not whole days\n")
	if !ok {
		t.Fatal("the runbook has no 00206 section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	if offersDelete.MatchString(section) {
		t.Errorf("the runbook offers deleting the policy:\n%s", section)
	}
	_, fix, ok := strings.Cut(section, "```sql\n")
	if !ok {
		t.Fatal("the runbook's 00206 section has no sql block")
	}
	fix, _, _ = strings.Cut(fix, "```")
	fix = strings.ReplaceAll(fix, "<policy id>", "dpol_36h")
	conn, cerr := db.Conn(ctx)
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, fix); err != nil {
		t.Fatalf("the runbook's fix:\n%s\n%v", fix, err)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up after the runbook's fix: %v", err)
	}
	var got string
	if err := db.QueryRow(`SELECT retry_days::text FROM dunning_policies WHERE id = 'dpol_36h'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != `[3, 3, 5]` {
		t.Errorf("retry_days %s, want [3, 3, 5]: the fix changes entry 1 to 72h and keeps entry 2 (120h)", got)
	}
}
