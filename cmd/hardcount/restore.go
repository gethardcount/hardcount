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
// with its anchor, and pause billing. The anchor is read before the pause,
// which clears billing's lease heartbeat. A database that is already marked
// keeps its marker and anchor: it may be a backup of a restore that never
// caught up.
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
	restorePauseSQL = `SELECT public.leader_pause('billing', 'restore', 'database restored: run hardcount restore catch-up, then resume billing')`
)

var restoreMarkStatements = []string{restoreTableSQL, restoreMarkSQL, restorePauseSQL}

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

// markRestored runs restoreMarkStatements in one transaction on the owner
// role, and returns the marker. It is a TxBypass transaction because the
// anchor reads stripe_webhook_events across tenants, and that table's
// row-level security applies to the owner too.
func markRestored(ctx context.Context, db *postgres.DB, setBy string) (restoreMarker, error) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return restoreMarker{}, err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, restoreTableSQL); err != nil {
		return restoreMarker{}, fmt.Errorf("create the restore marker table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, restoreMarkSQL, setBy); err != nil {
		return restoreMarker{}, fmt.Errorf("write the restore marker: %w", err)
	}
	if _, err := tx.ExecContext(ctx, restorePauseSQL); err != nil {
		return restoreMarker{}, fmt.Errorf("pause billing: %w", err)
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
			"Follow \"A backup older than 29 days\" in docs/ops/restore.md", start.UTC().Format(time.RFC3339))
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
	catchUpUnmatched   = 3 // payments the operator must handle and acknowledge
	catchUpKeyRejected = 4 // Stripe refused a live key
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
	if !strings.HasPrefix(key, "sk_") && !strings.HasPrefix(key, "rk_") {
		return fmt.Errorf("%s does not hold a Stripe secret key (sk_… or rk_…)", file)
	}
	f[tenantstripe.Account{TenantID: tenantID, Livemode: mode == "live"}] = key
	return nil
}

// ackFlag collects --acknowledge <PaymentIntent id>.
type ackFlag map[string]bool

func (f ackFlag) String() string { return fmt.Sprintf("%d payments", len(f)) }

func (f ackFlag) Set(v string) error {
	if !strings.HasPrefix(v, "pi_") {
		return fmt.Errorf("%q is not a Stripe PaymentIntent id (pi_…)", v)
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
		os.Exit(restoreBegin())
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
// time, as scripts/restore.sh marks one it restores from a dump.
func restoreBegin() int {
	pool, timeout, err := openOwnerPool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore begin failed: %v\n", err)
		return 1
	}
	defer func() { _ = pool.Close() }() // the process exits next
	m, err := markRestored(context.Background(), postgres.NewDB(pool, timeout), "hardcount restore begin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore begin failed: %v\n", err)
		return 1
	}
	var out strings.Builder
	printMarker(&out, m, true)
	out.WriteString("Billing is paused. Hardcount will not start until you run 'hardcount restore catch-up'.\n")
	fmt.Print(out.String())
	return 0
}

// restoreStatus prints the marker and whether billing is paused. It reads
// only.
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
	var paused bool
	var by, reason sql.NullString
	err = pool.QueryRowContext(ctx, `SELECT paused_at IS NOT NULL, paused_by, pause_reason FROM leader_leases WHERE role = 'billing'`).Scan(&paused, &by, &reason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		fmt.Println("Billing: no lease row yet; it runs when Hardcount starts.")
	case err != nil:
		fmt.Fprintf(os.Stderr, "restore status failed: read the billing pause: %v\n", err)
		return 1
	case paused:
		fmt.Printf("Billing: paused by %s (%s). Resume it with SELECT leader_unpause('billing'); after the steps in docs/ops/restore.md.\n", by.String, reason.String)
	default:
		fmt.Println("Billing: not paused.")
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
	fs.Var(acks, "acknowledge", "a listed payment (pi_…) you have handled; repeat for each")
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
	fmt.Println("catch-up complete; billing is still paused")
	return catchUpDone
}

func modeName(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// printCatchUpReport writes what the catch-up did per account, the payments
// it could not apply, and what to do next.
func printCatchUpReport(w *strings.Builder, r payment.CatchUpReport, acks ackFlag, code int) {
	listed := map[string]bool{}
	var lost []string
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
		fmt.Fprintf(w, "%s: payment pages closed %d; events %d%s: applied %d, already recorded %d, not this account's %d, held with a listed payment %d\n",
			name, a.SessionsExpired, a.Events, byType, a.Applied, a.AlreadyRecorded, a.Skipped, a.Held)
		if a.KeyRefused != "" {
			fmt.Fprintf(w, "  Stripe refused the key: %s\n", a.KeyRefused)
		}
		if a.Err != nil {
			fmt.Fprintf(w, "  stopped: %v\n", a.Err)
		}
		for _, p := range a.Lost {
			listed[p.PaymentIntentID] = true
			state := "not in the database"
			if p.InvoiceDraft {
				state = "still a draft"
			}
			ack := "not acknowledged"
			if p.Acknowledged {
				ack = "acknowledged"
			}
			lost = append(lost, fmt.Sprintf("  %s %s  %d %s  paid %s  customer %s  invoice %s (%s)  %d refund or dispute events  %s",
				name, p.PaymentIntentID, p.AmountCents, p.Currency, p.PaidAt.UTC().Format(time.RFC3339),
				p.CustomerID, p.InvoiceID, state, p.Held, ack))
		}
	}
	if len(lost) > 0 {
		fmt.Fprintln(w, "\nPayments whose invoice the restore lost or left as a draft (amounts in the currency's smallest unit):")
		for _, l := range lost {
			fmt.Fprintln(w, l)
		}
	}
	for pi := range acks {
		if !listed[pi] {
			fmt.Fprintf(w, "\n--acknowledge %s: not a listed payment, ignored.\n", pi)
		}
	}
	switch code {
	case catchUpRetry:
		fmt.Fprintln(w, "\nA temporary error stopped the catch-up. Run it again: an event already applied is not applied twice.")
	case catchUpKeyRejected:
		fmt.Fprintln(w, "\nStripe refused a live key. Run it again with --stripe-key <tenant>:live=@<file> holding the account's current secret key.")
	case catchUpUnmatched:
		fmt.Fprintln(w, "\nHandle each payment not acknowledged as docs/ops/restore.md says, then run the catch-up again with --acknowledge <pi_…> for each.")
	}
}
