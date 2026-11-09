package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gethardcount/hardcount/internal/api"
	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/migrate"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
)

// A restored database holds no record of what Stripe did after the backup:
// payments, refunds, disputes and saved cards. Started on it, Hardcount could
// charge or refund the same money again. So a restore marks the database,
// serve refuses to start while the mark is there, and `hardcount restore
// catch-up` applies Stripe's events since the backup and then removes it
// (ADR-135).

// The statements that mark a database as restored. They create the marker
// table when the backup predates it (migration 00223), write the marker
// with its anchor, and pause the two jobs that charge cards: billing, and
// dunning, which retries failed payments. Both stay paused after the
// catch-up, until the operator has redone the changes made after the
// backup, such as an invoice marked paid outside Stripe. The anchor is read
// before the pauses, which clear the two leases' heartbeats. A database
// that is already marked keeps its marker and anchor: it may be a backup of
// a restore that never caught up.
//
// `hardcount restore begin` runs them in one transaction, with $1 naming
// what marked the database. scripts/restore.sh runs the same statements in
// the restore's own transaction, with 'restore.sh' in place of $1, so a
// restore that rolls back leaves no marker and no pause.
// TestRestoreScriptMarksLikeBegin checks that the two texts agree.
const (
	restoreTableSQL = `CREATE TABLE IF NOT EXISTS public.restore_catchup (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    set_at timestamptz NOT NULL DEFAULT now(),
    set_by text NOT NULL,
    anchor timestamptz
)`
	restoreMarkSQL = `INSERT INTO public.restore_catchup (set_by, anchor)
SELECT $1::text, GREATEST(
    (SELECT max(GREATEST(heartbeat_at, last_tick_ended_at)) FROM public.leader_leases),
    (SELECT max(received_at) FROM public.stripe_webhook_events))
ON CONFLICT (id) DO NOTHING`
	restorePauseBillingSQL = `SELECT public.leader_pause('billing', 'restore', 'database restored: run hardcount restore catch-up, then resume billing')`
	restorePauseDunningSQL = `SELECT public.leader_pause('dunning', 'restore', 'database restored: run hardcount restore catch-up, then resume dunning after billing')`
)

var restoreMarkStatements = []string{restoreTableSQL, restoreMarkSQL, restorePauseBillingSQL, restorePauseDunningSQL}

// restorePausedRoles are the jobs the marking statements pause.
var restorePausedRoles = []string{"billing", "dunning"}

const (
	// stripeRedeliveryWindow is how long Stripe keeps resending a webhook
	// that was not acknowledged. The catch-up starts this long before the
	// anchor, so it also reads an event that was still being resent when
	// the backup was taken.
	stripeRedeliveryWindow = 72 * time.Hour
	// stripeEventsReach is how far back the catch-up may start: Stripe's
	// List Events reaches back 30 days, less a day of margin.
	stripeEventsReach = 29 * 24 * time.Hour
)

// restoreMarker is the row of restore_catchup.
type restoreMarker struct {
	SetAt  time.Time
	SetBy  string
	Anchor sql.NullTime
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readRestoreMarker returns the marker, and false when the database is not
// marked. It reads with the owner role: the table has no row-level security.
func readRestoreMarker(ctx context.Context, db rowQuerier) (restoreMarker, bool, error) {
	var m restoreMarker
	err := db.QueryRowContext(ctx, `SELECT set_at, set_by, anchor FROM restore_catchup`).Scan(&m.SetAt, &m.SetBy, &m.Anchor)
	if errors.Is(err, sql.ErrNoRows) {
		return restoreMarker{}, false, nil
	}
	if err != nil {
		return restoreMarker{}, false, fmt.Errorf("read the restore marker: %w", err)
	}
	return m, true, nil
}

// servedWithin is how recently a lease write shows a copy of Hardcount
// running on the database. A running copy writes the lease of its outbox
// jobs every few seconds, so a minute leaves a wide margin.
const servedWithin = time.Minute

// errDatabaseServed is markRestored's refusal of a database a copy of
// Hardcount is running on.
var errDatabaseServed = errors.New("a copy of Hardcount is running on this database")

// markRestored runs restoreMarkStatements in one transaction on the owner
// role, and returns the marker. It is a TxBypass transaction because the
// anchor reads stripe_webhook_events across tenants, and that table's
// row-level security applies to the owner too.
//
// It refuses, unless force, when a lease write in the last servedWithin
// shows a copy of Hardcount running on the database. serve checks the
// marker only when it starts, so a copy already running keeps charging,
// and its lease writes and the webhooks it receives move the anchor past
// the backup.
func markRestored(ctx context.Context, db *postgres.DB, setBy string, force bool) (restoreMarker, error) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return restoreMarker{}, err
	}
	defer postgres.Rollback(tx)
	if !force {
		var newest sql.NullTime
		var now time.Time
		if err := tx.QueryRowContext(ctx,
			`SELECT max(GREATEST(acquired_at, heartbeat_at, last_tick_ended_at)), now() FROM public.leader_leases`,
		).Scan(&newest, &now); err != nil {
			return restoreMarker{}, fmt.Errorf("read the leases: %w", err)
		}
		if newest.Valid && now.Sub(newest.Time) < servedWithin {
			return restoreMarker{}, fmt.Errorf("%w: a background job took its turn %s ago, at %s. "+
				"Stop every copy first: a running copy keeps charging cards, and it moves the anchor past the backup. "+
				"If no copy runs (the restore finished under a minute ago), wait a minute and run it again, or pass --force. "+
				"If a copy ran on the restored database, pass the catch-up --since with a time three days before the backup",
				errDatabaseServed, now.Sub(newest.Time).Round(time.Second), newest.Time.UTC().Format(time.RFC3339))
		}
	}
	if _, err := tx.ExecContext(ctx, restoreTableSQL); err != nil {
		return restoreMarker{}, fmt.Errorf("create the restore marker table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, restoreMarkSQL, setBy); err != nil {
		return restoreMarker{}, fmt.Errorf("write the restore marker: %w", err)
	}
	if _, err := tx.ExecContext(ctx, restorePauseBillingSQL); err != nil {
		return restoreMarker{}, fmt.Errorf("pause billing: %w", err)
	}
	if _, err := tx.ExecContext(ctx, restorePauseDunningSQL); err != nil {
		return restoreMarker{}, fmt.Errorf("pause dunning: %w", err)
	}
	m, _, err := readRestoreMarker(ctx, tx)
	if err != nil {
		return restoreMarker{}, err
	}
	if err := tx.Commit(); err != nil {
		return restoreMarker{}, fmt.Errorf("commit the restore marker: %w", err)
	}
	return m, nil
}

// clearRestoreMarker deletes the marker, so serve starts again.
func clearRestoreMarker(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM restore_catchup`); err != nil {
		return fmt.Errorf("delete the restore marker: %w", err)
	}
	return nil
}

// catchUpStart is the creation time of the oldest Stripe event the catch-up
// reads: stripeRedeliveryWindow before the anchor. since moves it earlier,
// never later. Without an anchor, since is required. A start more than
// stripeEventsReach ago is refused, because Stripe no longer lists the
// events before it.
func catchUpStart(anchor sql.NullTime, since, now time.Time) (time.Time, error) {
	var start time.Time
	switch {
	case anchor.Valid:
		start = anchor.Time.Add(-stripeRedeliveryWindow)
		if !since.IsZero() && since.Before(start) {
			start = since
		}
	case since.IsZero():
		return time.Time{}, errors.New("the restored database shows no time Hardcount last ran (no job ticked and no Stripe webhook arrived), so the catch-up does not know where to start. " +
			"Pass --since with a time three days before the backup, such as --since 2026-10-16T03:00:00Z")
	default:
		start = since
	}
	if oldest := now.Add(-stripeEventsReach); start.Before(oldest) {
		return time.Time{}, fmt.Errorf("the catch-up would read Stripe events from %s, more than 29 days ago, and Stripe lists events for 30 days only. "+
			"Follow docs/ops/runbook/restore-old-backup.md", start.UTC().Format(time.RFC3339))
	}
	return start.UTC(), nil
}

// The exit codes of `hardcount restore catch-up`. Only live-mode accounts
// decide them: test mode moves no real money, so its problems are reported
// and do not hold the database.
const (
	catchUpDone        = 0
	catchUpRetry       = 1 // a temporary error: running it again is safe
	catchUpRefused     = 2 // nothing was read from Stripe: the message says why
	catchUpUnmatched   = 3 // payments, refunds or events the operator must handle and acknowledge
	catchUpKeyRejected = 4 // a live key does not work: refused, or of the other mode
)

// catchUpExitCode decides the exit code from the report. A retryable error
// comes first, because a rerun may clear the rest. The marker is removed
// only on catchUpDone.
func catchUpExitCode(r payment.CatchUpReport) int {
	var failed, refused, unmatched bool
	for _, a := range r.Accounts {
		if !a.Account.Livemode {
			continue
		}
		failed = failed || a.Err != nil
		refused = refused || a.KeyRefused != ""
		for _, p := range a.Lost {
			unmatched = unmatched || !p.Acknowledged
		}
		for _, rf := range a.Refunds {
			unmatched = unmatched || !rf.Acknowledged
		}
		for _, u := range a.Unapplied {
			unmatched = unmatched || !u.Acknowledged
		}
	}
	switch {
	case failed:
		return catchUpRetry
	case refused:
		return catchUpKeyRejected
	case unmatched:
		return catchUpUnmatched
	}
	return catchUpDone
}

// stripeKeyFlag collects --stripe-key <tenant>:<live|test>=@<file>: a
// Stripe secret key for one account, read from a file so it stays out of
// the shell history. The keys are used for this run only and never saved.
type stripeKeyFlag map[tenantstripe.Account]string

func (f stripeKeyFlag) String() string { return fmt.Sprintf("%d keys", len(f)) }

func (f stripeKeyFlag) Set(v string) error {
	account, file, ok := strings.Cut(v, "=@")
	tenantID, mode, ok2 := strings.Cut(account, ":")
	if !ok || !ok2 || tenantID == "" || file == "" || (mode != "live" && mode != "test") {
		return errors.New("want <tenant>:<live|test>=@<file>, such as hc_ten_abc:live=@/root/stripe-key")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read the key file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	// A key of the other mode lists only the other mode's events, which the
	// catch-up would not apply: a live account would end with nothing read.
	if !strings.HasPrefix(key, "sk_"+mode+"_") && !strings.HasPrefix(key, "rk_"+mode+"_") {
		return fmt.Errorf("%s does not hold a %s-mode Stripe secret key (sk_%s_… or rk_%s_…)", file, mode, mode, mode)
	}
	f[tenantstripe.Account{TenantID: tenantID, Livemode: mode == "live"}] = key
	return nil
}

// unknownTenants returns the tenants of keys the database has no row for,
// sorted. A key for a tenant the restore lost, or a mistyped one, would
// skip every event as another tenant's and end with nothing applied.
func unknownTenants(ctx context.Context, db rowQuerier, keys stripeKeyFlag) ([]string, error) {
	var out []string
	for acct := range keys {
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, acct.TenantID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("look up tenant %s: %w", acct.TenantID, err)
		}
		if !exists && !slices.Contains(out, acct.TenantID) {
			out = append(out, acct.TenantID)
		}
	}
	slices.Sort(out)
	return out, nil
}

// ackFlag collects --acknowledge: a listed payment (pi_…), refund (re_…)
// or event (evt_…).
type ackFlag map[string]bool

func (f ackFlag) String() string { return fmt.Sprintf("%d acknowledged", len(f)) }

func (f ackFlag) Set(v string) error {
	if !strings.HasPrefix(v, "pi_") && !strings.HasPrefix(v, "re_") && !strings.HasPrefix(v, "evt_") {
		return fmt.Errorf("%q is not the id of a listed payment (pi_…), refund (re_…) or event (evt_…)", v)
	}
	f[v] = true
	return nil
}

// timeFlag is an RFC 3339 time, such as 2026-10-16T03:00:00Z.
type timeFlag struct{ t time.Time }

func (f *timeFlag) String() string { return f.t.Format(time.RFC3339) }

func (f *timeFlag) Set(v string) error {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return errors.New("want a time such as 2026-10-16T03:00:00Z")
	}
	f.t = t
	return nil
}

func runRestore(sub string, args []string) {
	switch sub {
	case "begin":
		os.Exit(restoreBegin(args))
	case "status":
		os.Exit(restoreStatus())
	case "catch-up":
		os.Exit(restoreCatchUp(args))
	default:
		fmt.Fprintf(os.Stderr, "unknown restore subcommand: %q\navailable: begin, catch-up, status\n", sub)
		os.Exit(1)
	}
}

// openOwnerPool opens DATABASE_URL, the owner role, for the restore
// commands that read or write only the marker and the leases.
func openOwnerPool() (*sql.DB, time.Duration, error) {
	cfg, err := config.LoadDBOnly()
	if err != nil {
		return nil, 0, err
	}
	pool, err := config.OpenPostgres(cfg)
	if err != nil {
		return nil, 0, err
	}
	return pool, cfg.QueryTimeout, nil
}

// restoreBegin marks a database restored from a snapshot or to a point in
// time, as scripts/restore.sh marks one it restores from a dump. It exits 2
// when a copy of Hardcount runs on the database, unless --force.
func restoreBegin(args []string) int {
	fs := flag.NewFlagSet("hardcount restore begin", flag.ContinueOnError)
	force := fs.Bool("force", false, "mark the database even though a lease was written in the last minute")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	pool, timeout, err := openOwnerPool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore begin failed: %v\n", err)
		return 1
	}
	defer func() { _ = pool.Close() }() // the process exits next
	m, err := markRestored(context.Background(), postgres.NewDB(pool, timeout), "hardcount restore begin", *force)
	if errors.Is(err, errDatabaseServed) {
		fmt.Fprintf(os.Stderr, "restore begin refused: %v\n", err)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore begin failed: %v\n", err)
		return 1
	}
	var out strings.Builder
	printMarker(&out, m, true)
	out.WriteString("Billing and dunning are paused. Hardcount will not start until you run 'hardcount restore catch-up'.\n")
	fmt.Print(out.String())
	return 0
}

// restoreStatus prints the marker and whether billing and dunning are
// paused. It reads only.
func restoreStatus() int {
	pool, _, err := openOwnerPool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore status failed: %v\n", err)
		return 1
	}
	defer func() { _ = pool.Close() }() // the process exits next
	ctx := context.Background()
	m, restored, err := readRestoreMarker(ctx, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore status failed: %v\n", err)
		return 1
	}
	if !restored {
		fmt.Println("Not marked as restored: Hardcount starts normally.")
	} else {
		var out strings.Builder
		printMarker(&out, m, true)
		out.WriteString("Hardcount will not start until 'hardcount restore catch-up' finishes.\n")
		fmt.Print(out.String())
	}
	for _, role := range restorePausedRoles {
		name := strings.ToUpper(role[:1]) + role[1:]
		var paused bool
		var by, reason sql.NullString
		err = pool.QueryRowContext(ctx, `SELECT paused_at IS NOT NULL, paused_by, pause_reason FROM leader_leases WHERE role = $1`, role).Scan(&paused, &by, &reason)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			fmt.Printf("%s: no lease row yet; it runs when Hardcount starts.\n", name)
		case err != nil:
			fmt.Fprintf(os.Stderr, "restore status failed: read the %s pause: %v\n", role, err)
			return 1
		case paused:
			fmt.Printf("%s: paused by %s (%s). Resume it with SELECT leader_unpause('%s'); after the steps in docs/ops/restore.md.\n", name, by.String, reason.String, role)
		default:
			fmt.Printf("%s: not paused.\n", name)
		}
	}
	return 0
}

// printMarker writes the marker. withStart adds where the catch-up will
// start reading, for begin and status; the catch-up prints its own start.
func printMarker(w *strings.Builder, m restoreMarker, withStart bool) {
	fmt.Fprintf(w, "Marked as restored at %s by %s.\n", m.SetAt.UTC().Format(time.RFC3339), m.SetBy)
	if !m.Anchor.Valid {
		fmt.Fprintln(w, "Anchor: none. The restored data shows no time Hardcount ran, so the catch-up needs --since.")
		return
	}
	fmt.Fprintf(w, "Anchor: %s, the last time the restored data shows Hardcount running.\n", m.Anchor.Time.UTC().Format(time.RFC3339))
	if withStart {
		fmt.Fprintf(w, "The catch-up reads Stripe events from %s.\n", m.Anchor.Time.Add(-stripeRedeliveryWindow).UTC().Format(time.RFC3339))
	}
}

// restoreCatchUp applies Stripe's events since the anchor to a restored
// database, and removes the marker when nothing is left to do.
func restoreCatchUp(args []string) int {
	keys := stripeKeyFlag{}
	acks := ackFlag{}
	var since timeFlag
	fs := flag.NewFlagSet("hardcount restore catch-up", flag.ContinueOnError)
	fs.Var(&since, "since", "read Stripe events from this time when it is earlier than 72 hours before the anchor (RFC 3339)")
	fs.Var(keys, "stripe-key", "`<tenant>:<live|test>=@<file>`, a file holding the account's current Stripe secret key, used for this run only; repeat for each account")
	fs.Var(acks, "acknowledge", "a listed payment (pi_…), refund (re_…) or event (evt_…) you have handled; repeat for each")
	if err := fs.Parse(args); err != nil {
		return catchUpRefused
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return catchUpRefused
	}

	logger, err := newLogger(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return catchUpRefused
	}
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return catchUpRefused
	}
	pool, err := config.OpenPostgres(cfg.DB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		return catchUpRetry
	}
	defer func() { _ = pool.Close() }() // the process exits next
	if err := migrate.CheckSchemaReady(pool); err != nil {
		fmt.Fprintf(os.Stderr, "the schema is not ready: %v\nRun 'hardcount migrate status', and 'hardcount migrate' if it says behind.\n", err)
		return catchUpRefused
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m, restored, err := readRestoreMarker(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return catchUpRetry
	}
	if !restored {
		fmt.Fprintln(os.Stderr, "This database is not marked as restored, so there is nothing to catch up: Hardcount starts normally. "+
			"After a snapshot or point-in-time restore, run 'hardcount restore begin' first.")
		return catchUpRefused
	}
	start, err := catchUpStart(m.Anchor, since.t, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return catchUpRefused
	}
	missing, err := unknownTenants(ctx, pool, keys)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return catchUpRetry
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "--stripe-key names %s, which the restored database does not hold. "+
			"Check the tenant ID. A tenant created after the backup is lost with it, so its Stripe events cannot be applied.\n",
			strings.Join(missing, ", "))
		return catchUpRefused
	}

	appPool, closeAppPool := openAppPool(cfg, pool)
	defer closeAppPool()
	server := api.NewServer(postgres.NewDB(appPool, cfg.DB.QueryTimeout), nil, nil,
		api.WithInvoiceGrace(cfg.InvoiceGracePeriod),
		api.WithAppEnv(cfg.Env),
		api.WithRateLimits(cfg.RateLimits),
		api.WithStripeKeys(keys))

	extra := make([]tenantstripe.Account, 0, len(keys))
	for acct := range keys {
		extra = append(extra, acct)
	}
	report, err := server.RestoreCatchUp.Run(ctx, payment.CatchUpRun{Since: start, Extra: extra, Acknowledged: acks})
	if err != nil {
		fmt.Fprintf(os.Stderr, "catch-up failed: %v\n", err)
		return catchUpRetry
	}
	var out strings.Builder
	printMarker(&out, m, false)
	fmt.Fprintf(&out, "Read Stripe events created from %s.\n\n", start.Format(time.RFC3339))
	code := catchUpExitCode(report)
	printCatchUpReport(&out, report, acks, code)
	fmt.Print(out.String())
	if code != catchUpDone {
		return code
	}
	if err := clearRestoreMarker(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return catchUpRetry
	}
	fmt.Println("catch-up complete; billing and dunning are still paused")
	return catchUpDone
}

func modeName(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

func ackState(acknowledged bool) string {
	if acknowledged {
		return "acknowledged"
	}
	return "not acknowledged"
}

// printCatchUpReport writes what the catch-up did per account, what it
// listed instead of applying, and what to do next.
func printCatchUpReport(w *strings.Builder, r payment.CatchUpReport, acks ackFlag, code int) {
	listedIDs := map[string]bool{}
	var payments, refunds, events []string
	if len(r.Accounts) == 0 {
		fmt.Fprintln(w, "No account has Stripe keys, so there was nothing to read.")
	}
	for _, a := range r.Accounts {
		name := a.Account.TenantID + " " + modeName(a.Account.Livemode)
		var types []string
		for typ, n := range a.ByType {
			types = append(types, fmt.Sprintf("%s %d", typ, n))
		}
		sort.Strings(types)
		byType := ""
		if len(types) > 0 {
			byType = " (" + strings.Join(types, ", ") + ")"
		}
		fmt.Fprintf(w, "%s: payment pages closed %d; events %d%s: applied %d, already recorded %d, not this account's %d, held with a listed payment or refund %d\n",
			name, a.SessionsExpired, a.Events, byType, a.Applied, a.AlreadyRecorded, a.Skipped, a.Held)
		if a.KeyRefused != "" {
			fmt.Fprintf(w, "  the key does not work: %s\n", a.KeyRefused)
		}
		if a.Err != nil {
			fmt.Fprintf(w, "  stopped: %v\n", a.Err)
		}
		for _, p := range a.Lost {
			listedIDs[p.PaymentIntentID] = true
			state := "not in the database"
			if p.InvoiceDraft {
				state = "still a draft"
			}
			payments = append(payments, fmt.Sprintf("  %s %s  %d %s  paid %s  customer %s  invoice %s (%s)  %d events held with it  %s",
				name, p.PaymentIntentID, p.AmountCents, p.Currency, p.PaidAt.UTC().Format(time.RFC3339),
				p.CustomerID, p.InvoiceID, state, p.Held, ackState(p.Acknowledged)))
		}
		for _, rf := range a.Refunds {
			listedIDs[rf.RefundID] = true
			why := "made in Stripe; the invoice was reported to Stripe Tax"
			if rf.Reason == payment.RefundCreditNoteLost {
				why = "made by Hardcount for credit note " + rf.CreditNoteID + ", which no credit note in the database holds"
			}
			refunds = append(refunds, fmt.Sprintf("  %s %s  %d %s  %s  created %s  payment %s  invoice %s  %s  %s",
				name, rf.RefundID, rf.AmountCents, rf.Currency, rf.Status, rf.CreatedAt.UTC().Format(time.RFC3339),
				rf.PaymentIntentID, rf.InvoiceID, why, ackState(rf.Acknowledged)))
		}
		for _, u := range a.Unapplied {
			listedIDs[u.EventID] = true
			why := "customer " + u.CustomerID + " is not in the database"
			if u.Err != nil {
				why = "failed: " + u.Err.Error()
			}
			events = append(events, fmt.Sprintf("  %s %s  %s  created %s  %s  %s",
				name, u.EventID, u.Type, u.CreatedAt.UTC().Format(time.RFC3339), why, ackState(u.Acknowledged)))
		}
	}
	for _, section := range []struct {
		title string
		lines []string
	}{
		{"Payments whose invoice the restore lost or left as a draft (amounts in the currency's smallest unit):", payments},
		{"Refunds not applied, because applying them could refund or reverse tax a second time:", refunds},
		{"Events not applied:", events},
	} {
		if len(section.lines) == 0 {
			continue
		}
		fmt.Fprintln(w, "\n"+section.title)
		for _, l := range section.lines {
			fmt.Fprintln(w, l)
		}
	}
	var ignored []string
	for id := range acks {
		if !listedIDs[id] {
			ignored = append(ignored, id)
		}
	}
	sort.Strings(ignored)
	for _, id := range ignored {
		fmt.Fprintf(w, "\n--acknowledge %s: nothing listed in this run has this id, so nothing was recorded for it.\n", id)
	}
	switch code {
	case catchUpRetry:
		fmt.Fprintln(w, "\nA temporary error stopped the catch-up. Run it again: an event already applied is not applied twice.\n"+
			"If the same 'stopped:' line comes back, a rerun will not fix it. 'decrypt' means HARDCOUNT_ENCRYPTION_KEY is not the key the backup's install used.\n"+
			"For anything else, do not start Hardcount: get help, with the 'stopped:' line.")
	case catchUpKeyRejected:
		fmt.Fprintln(w, "\nA live key does not work. Run it again with --stripe-key <tenant>:live=@<file> holding the account's current live secret key.")
	case catchUpUnmatched:
		fmt.Fprintln(w, "\nNext, as docs/ops/restore.md says under \"Handle what the catch-up listed\":\n"+
			"  1. Refund in Stripe now each listed payment you will give back.\n"+
			"  2. Run the catch-up again with --acknowledge <id> for each listed payment, refund and event that is not acknowledged.\n"+
			"     An event that failed is tried once more, and recorded without being applied only if it fails again.\n"+
			"  3. Once Hardcount runs, while billing is still paused, grant credit for each payment you keep, and record each refund.")
	}
}
