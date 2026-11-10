// Hardcount is the billing server. With no arguments it serves the API and
// runs the background workers. "hardcount migrate" applies the pending
// database migrations, and "hardcount help" lists the commands and the main
// settings. docs/settings.md lists every setting.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	neturl "net/url"

	"github.com/gethardcount/hardcount/internal/api"
	mw "github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/migrate"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/telemetry"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/version"
	"github.com/gethardcount/hardcount/internal/webhook"
)

func main() {
	// Pin the process to UTC (ADR-075). pgx decodes timestamptz with
	// time.Unix, which uses time.Local. On a host whose zone is not UTC, the
	// database read path then returns timestamps in that zone, and
	// time.Time.MarshalJSON writes its offset (for example "+05:30") to the
	// API instead of "Z". The app already creates instants in UTC
	// (clock.Now().UTC()). This assignment makes the read path agree, so
	// every serialized timestamp is UTC whatever the host's zone. It must be
	// the first statement, before any connection or goroutine, so nothing
	// reads time.Local while it changes. Billing date math uses explicit
	// zones, never time.Local, so it is not affected (ADR-058, ADR-074).
	time.Local = time.UTC

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	subcmd := ""
	if len(os.Args) > 2 {
		subcmd = os.Args[2]
	}

	switch cmd {
	case "serve":
		serve()
	case "migrate":
		switch subcmd {
		case "status":
			runMigrateStatus()
		case "rollback":
			runMigrateRollback()
		case "":
			runMigrate()
		default:
			fmt.Fprintf(os.Stderr, "unknown migrate subcommand: %s\navailable: status, rollback\n", subcmd)
			os.Exit(1)
		}
	case "restore":
		var args []string
		if len(os.Args) > 3 {
			args = os.Args[3:]
		}
		runRestore(subcmd, args)
	case "version":
		fmt.Println("hardcount " + version.Version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func serve() {
	logger, err := newLogger(os.Stdout)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	// OpenTelemetry tracing is a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT
	// is set.
	tracingShutdown, err := telemetry.Init(context.Background())
	if err != nil {
		slog.Error("init tracing", "error", err)
		os.Exit(1)
	}
	defer func() { _ = tracingShutdown(context.Background()) }()

	slog.Info("hardcount starting", "env", cfg.Env, "port", cfg.Port)

	pool, err := config.OpenPostgres(cfg.DB)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = pool.Close() }()

	if cfg.Migrate {
		// Logged as info, not a warning. The one-VM guide upgrades this way,
		// and with several copies the migration lock makes it safe
		// (docs/upgrades.md). A warning that operators are told to ignore
		// teaches them to ignore warnings (#1245).
		slog.Info("running migrations on start (RUN_MIGRATIONS_ON_BOOT=true)")
		if err := migrate.Up(cfg.DB.URL); err != nil {
			slog.Error("run migrations", "error", err)
			os.Exit(1)
		}
	}

	// Refuse to start if the schema is behind what this binary expects. The
	// check catches a missed migration Job, a race during a rolling deploy,
	// and a dirty migration.
	if err := migrate.CheckSchemaReady(pool); err != nil {
		slog.Error("schema check failed", "error", err)
		os.Exit(1)
	}

	// Refuse to start on a restored database until it has caught up with
	// Stripe (ADR-135). It holds none of the payments and refunds Stripe made
	// after the backup, so any request or job could charge or refund that
	// money again. This runs before anything listens or takes a lease, and
	// after RUN_MIGRATIONS_ON_BOOT, which keeps the marker.
	if m, restored, err := readRestoreMarker(context.Background(), pool); err != nil {
		slog.Error("refusing to start: could not read the restore marker", "error", err)
		os.Exit(1)
	} else if restored {
		slog.Error("refusing to start: this database was restored from a backup and has not caught up with Stripe. Run `hardcount restore catch-up` first (docs/ops/restore.md)",
			"restored_at", m.SetAt, "restored_by", m.SetBy)
		os.Exit(2)
	}

	appPool, closeAppPool := openAppPool(cfg, pool)
	defer closeAppPool()
	// appPool serves every request and background job, so its waits are the
	// ones an operator needs to see. pool, when it is a separate pool, runs
	// only the start-up checks above, so it is not exported.
	mw.RegisterDBPoolStats(appPool)

	db := postgres.NewDB(appPool, cfg.DB.QueryTimeout)

	// Leader gate (ADR-114). Each singleton loop (billing, dunning, the two
	// outbox dispatchers and webhook delivery retry) runs one tick at a time
	// under a lease in leader_leases. Each role's claim statement re-checks
	// the lease token in SQL. Every statement stands alone, so the gate is
	// safe behind a transaction-mode pooler.
	gate := leader.New(appPool, func(role leader.Role, reason string) {
		mw.RecordLeaderLeaseLost(string(role), reason)
	})
	roles := make([]string, 0, len(leader.Roles))
	for _, r := range leader.Roles {
		roles = append(roles, string(r))
	}
	mw.InitLeaderLeaseLost(roles...)
	mw.RegisterLeaderStatus(func(ctx context.Context) ([]mw.LeaderStatusRow, error) {
		rows, err := leader.Status(ctx, appPool)
		if err != nil {
			return nil, err
		}
		out := make([]mw.LeaderStatusRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, mw.LeaderStatusRow{Role: string(r.Role), Held: r.Held, HasTicked: r.HasTicked, LastTickAgeSeconds: r.LastTickAgeS, Paused: r.Paused})
		}
		return out, nil
	})

	// Production refuses to start without a reachable Redis. Without Redis
	// the general and hosted-invoice rate limiters fail closed, so a copy
	// with no Redis would pass /health/ready and still answer 429 to its
	// share of traffic, hosted invoice pay pages included.
	// config.validateFatal already rejects an unset REDIS_URL in production;
	// this check catches an invalid or unreachable one. Other environments
	// warn and fail open, except /v1/auth, whose limiter counts in this
	// process.
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	rdb, err := mw.OpenRedis(pingCtx, cfg.RedisURL)
	cancelPing()
	if redisBootFatal(cfg.Env, err) {
		slog.Error("refusing to start: REDIS_URL is set but Redis is invalid or unreachable — in production the general and hosted-invoice rate limiters fail CLOSED without it (every covered request 429); fix REDIS_URL or the network path", "error", err)
		os.Exit(1)
	}
	if err != nil {
		slog.Warn("redis unavailable — rate limiting fails open in this environment, except /v1/auth, which counts in this process", "env", cfg.Env, "error", err)
	} else if rdb != nil {
		slog.Info("redis connected for rate limiting")
	}

	server := api.NewServer(db, nil, rdb,
		api.WithInvoiceGrace(cfg.InvoiceGracePeriod),
		api.WithAppEnv(cfg.Env),
		api.WithRateLimits(cfg.RateLimits))

	billingInterval := 1 * time.Hour
	if cfg.Env == "local" {
		billingInterval = 5 * time.Minute
	}
	// Log the grace and the billing interval together. A period closes on
	// the first billing tick after its end plus the grace, so an operator
	// needs both to know when invoices are built.
	slog.Info("invoice grace period", "invoice_grace_period", cfg.InvoiceGracePeriod, "billing_interval", billingInterval)
	scheduler := billing.NewScheduler(server.BillingEngine, billingInterval, 50, server.DunningSvc, server.SettingsStore, nil, server.CreditSvc)
	// api.NewServer always builds these services, so wire them without nil
	// checks. The scheduler skips a worker whose service is nil, which tests
	// rely on. A nil check here would hide a wiring bug by turning off a
	// money-path worker without a word.
	scheduler.SetTokenCleaner(server.TokenSvc)
	scheduler.SetIdempotencyCleaner(mw.NewIdempotencyCleaner(db))
	scheduler.SetLoginThrottleCleaner(server.LoginThrottle)
	scheduler.SetTaxRetrier(server.InvoiceSvc)
	// Retry clawback credit-note drafts whose Issue() failed after commit. A
	// subscription downgrade or item removal writes the draft in the same
	// transaction as the change, and issues it after the commit. This retry
	// issues a draft whose first attempt failed. The retry is a follow-up to
	// ADR-056.
	scheduler.SetClawbackRetrier(server.CreditNoteSvc)
	// Trial expiry on the real clock, for subscriptions not on a test clock.
	// Each tick moves trialing subscriptions to active at trial_end_at.
	// Otherwise the dashboard could show an ended trial as trialing for up
	// to about 30 days.
	scheduler.SetTrialExpirer(server.SubscriptionSvc)
	// Pause resume on the real clock: clear pause_collection on subscriptions
	// whose resumes_at has passed. It runs after the billing run, and it skips
	// a subscription that still owes a period close before resumes_at, so the
	// period close reads the pause as it stood at the period's end. Without
	// this step, a paused subscription whose next_billing_at is in the future
	// stays paused indefinitely.
	scheduler.SetPauseResumer(server.SubscriptionSvc)
	scheduler.SetPaymentReconciler(server.PaymentReconciler)
	scheduler.SetGate(gate)

	// Track scheduler runs so /health/ready can report a stalled scheduler in
	// its body. It does not change the status code.
	api.SetSchedulerInterval(billingInterval)
	scheduler.SetOnRun(api.RecordSchedulerRun)

	// Test-clock catch-up runs off the request (ADR-015). Catch-up is the
	// billing for the time a test clock skipped. The Advance handler sets the
	// clock to status='advancing' and queues a job, and this worker runs it.
	// A buffer of 100 is plenty: at expected operator volumes, there are never
	// more than a few advances in flight.
	catchupQueue := testclock.NewCatchupQueue(100)
	server.TestClockSvc.SetCatchupQueue(catchupQueue)
	catchupWorker := testclock.NewCatchupWorker(catchupQueue, func(ctx context.Context, job testclock.CatchupJob) error {
		return server.TestClockSvc.RunCatchup(ctx, job)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", cfg.Port),
		Handler: server,
		// ReadHeaderTimeout bounds header parsing (a slowloris guard).
		// ReadTimeout gives the body a generous window, so a batch ingest of up
		// to 1,000 events over a slow link is not cut off. WriteTimeout must be
		// longer than the 30s request Timeout middleware, so the server writes
		// its JSON 504 before the socket closes. Equal values race and reset
		// the connection.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 13, // 8 KB
	}

	// Shutdown has a published time budget in three stages: at most 90s after
	// the SHUTDOWN_DRAIN_DELAY readiness drain, and usually under two seconds.
	// Each stage's limit abandons only work that is built to be resumed. Set
	// the orchestrator's grace period above the drain delay plus 90s: 120s
	// with the 5s default (docs/ops/high-availability.md, "What happens when a
	// copy stops"; compose sets stop_grace_period). Docker's 10s default sends
	// SIGKILL mid-drain.
	const (
		shutdownHTTPDrain   = 30 * time.Second // stage 1: in-flight requests
		shutdownCatchupWait = 30 * time.Second // stage 2: an in-flight test-clock advance
		shutdownWorkerDrain = 30 * time.Second // stage 3: scheduler + dispatcher ticks
	)

	// Stage 0: as soon as Shutdown is called, close every SSE subscriber, so
	// the dashboard's live tail does not hold the HTTP drain open for its full
	// 30s (see EventBus.CloseAll).
	srv.RegisterOnShutdown(server.WebhookOutSvc.EventBus().CloseAll)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Track the background workers, so shutdown can wait for each to finish
	// its current tick before the process exits. Otherwise a SIGTERM during a
	// billing run could leave partial work: half-built invoices, ledger
	// entries without their invoice update, or webhook deliveries in flight.
	var workers sync.WaitGroup

	workers.Add(2)
	go func() {
		defer workers.Done()
		scheduler.Start(ctx)
	}()
	go func() {
		defer workers.Done()
		server.WebhookOutSvc.StartRetryWorker(ctx, 30*time.Second, gate)
	}()

	// Start the catch-up worker after the queue is set on the service, so a
	// clock recovered at boot has a queue to land in. Recovery is best-effort:
	// clocks left 'advancing' by an earlier process are queued again and run
	// like any other job. If recovery fails, log an error and keep booting.
	// New clocks still work. Only the stuck old clocks are not recovered, and
	// they can be deleted by hand.
	catchupWorker.Start()
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := server.TestClockSvc.RecoverInFlight(recoveryCtx); err != nil {
		slog.Error("test-clock catchup recovery failed", "error", err)
	}
	recoveryCancel()

	// The outbox dispatchers always run (ADR-040). The webhook dispatcher
	// turns webhook_outbox rows into events and their deliveries, and the
	// email dispatcher drains email_outbox into *email.Sender. Each tick takes
	// a leader lease (ADR-114), so with several copies only one drains at a
	// time. An operator pauses a role, with no restart, by running
	// `SELECT leader_pause('webhook_outbox', 'oncall', 'why')`
	// (docs/ops/runbook/background-job-stuck.md).
	workers.Add(1)
	go func() {
		defer workers.Done()
		dispatcher := webhook.NewDispatcher(server.OutboxStore, server.WebhookOutSvc, webhook.DispatcherConfig{}, gate)
		dispatcher.Start(ctx)
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		dispatcher := email.NewDispatcher(server.EmailOutboxStore, server.EmailSender, email.DispatcherConfig{}, gate)
		dispatcher.SetSettledChecker(server.EmailSettledChecker)
		dispatcher.Start(ctx)
	}()

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down — announcing drain, then draining workers and in-flight requests",
		"drain_delay", cfg.ShutdownDrainDelay)

	// Readiness first. From now on /health/ready answers 503 'draining' while
	// /health (liveness) stays 200, so a load balancer that follows health
	// checks stops sending traffic here before the listener closes. Otherwise
	// every rolling deploy ends with connection resets on this copy. Keep
	// listening for the configured delay, so the balancer's probes see the
	// 503. Removal driven by an orchestrator, such as Kubernetes endpoints,
	// happens at once. The delay is only for the health-check path. The delay
	// is 0 in local.
	api.SetDraining()
	if cfg.ShutdownDrainDelay > 0 {
		time.Sleep(cfg.ShutdownDrainDelay)
	}

	// Stage 1 (at most 30s): stop accepting HTTP requests and finish the ones
	// in flight. RegisterOnShutdown above closes the SSE subscribers first,
	// so an open live tail does not hold this stage to its limit.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownHTTPDrain)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http shutdown error", "error", err)
	}

	// Stage 2 (at most 30s): the test-clock catch-up worker. Catch-up runs on
	// context.Background(), so cancelling ctx does not reach it. An advance in
	// flight must not stop mid-write. Stop waits up to shutdownCatchupWait
	// for the advance in flight (most finish in seconds), then abandons it.
	// An abandoned advance leaves the clock 'advancing', and RecoverInFlight
	// runs only when a copy boots. So in a rolling deploy, where the new copy
	// is already up, nothing resumes the advance until some copy restarts:
	// hazard 7, with its planned fix ha-9, in
	// docs/dev/records/ha-readiness-2026-07-06.md.
	if !catchupWorker.Stop(shutdownCatchupWait) {
		slog.Warn("test-clock catchup worker still busy — abandoning the in-flight advance; the clock stays 'advancing' until a replica restarts (docs/ops/runbook/test-clock-advance-hung.md)")
	}

	// Stage 3 (at most 30s): the scheduler and the dispatchers. Their ticks
	// run on ctx, which is now cancelled, and abort atomically at their next
	// database call. This wait is for the work that runs with
	// context.WithoutCancel: a Stripe charge, a tax retry's finalize and its
	// CommitTax, and an outbox row's detached mark. Waiting lets a tick that
	// is part-way through record its result, instead of leaving a claimed
	// row for its lease to expire. Each Stripe attempt has a 30s client
	// timeout, but stripe-go retries a timeout up to twice, so this stage's
	// own limit caps the wait. The tax_commit sweep re-commits a CommitTax
	// that this limit cuts off. The limit exists so a hung worker cannot block
	// the exit.
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("workers drained cleanly")
	case <-time.After(shutdownWorkerDrain):
		slog.Warn("workers did not drain within the budget — forcing exit; claimed rows resume via their leases")
	}
}

// loadDSN loads only the database URL, for the migrate subcommands. It skips
// the Stripe, Redis and encryption checks, which migrations do not need.
func loadDSN() string {
	cfg, err := config.LoadDBOnly()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	return cfg.URL
}

func runMigrate() {
	sum, err := migrate.UpReport(loadDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "migration failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(migrateSummaryLine(sum))
}

// migrateSummaryLine says what `hardcount migrate` did, including when no
// migration ran (#1245).
func migrateSummaryLine(sum migrate.UpSummary) string {
	switch sum.Applied {
	case 0:
		return fmt.Sprintf("already up to date: the database is at version %d", sum.Version)
	case 1:
		return fmt.Sprintf("applied 1 migration: the database is at version %d", sum.Version)
	default:
		return fmt.Sprintf("applied %d migrations: the database is at version %d", sum.Applied, sum.Version)
	}
}

// runMigrateStatus prints the upgrade plan: the database's version, this
// release's, the state, the files still to apply and whether a migration
// is running now. It reads only, takes no lock and creates nothing, so it
// is safe to run with the new release before an upgrade. It exits 0 when
// it could read the state.
func runMigrateStatus() {
	p, err := migrate.ReadPlan(loadDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "status failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(migrate.FormatPlan(p, version.Version))
}

// rollbackRefused is what `hardcount migrate rollback` prints outside local.
const rollbackRefused = `hardcount migrate rollback runs only with APP_ENV=local.
Rolling back in staging or production loses data the newer release wrote. To go back, restore the backup taken before the upgrade: docs/upgrades.md#going-back`

// runMigrateRollback undoes the latest migration, one step, on a local
// database. It is for the edit-and-retry loop while writing a new migration.
// Migration 180, the baseline, never rolls back.
func runMigrateRollback() {
	if config.AppEnv() != "local" {
		fmt.Fprintln(os.Stderr, rollbackRefused)
		os.Exit(2)
	}
	v, err := migrate.Down(loadDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("rolled back to version %d\n", v)
}

// newLogger returns the server's JSON logger at LOG_LEVEL
// (config.ParseLogLevel), so production can change verbosity without a
// rebuild. An unknown level is an error and the server does not start.
// Falling back to info without a word would let LOG_LEVEL=verbose, meant as
// debug, hide every debug line it was set to show.
func newLogger(w io.Writer) (*slog.Logger, error) {
	level, err := config.ParseLogLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		return nil, err
	}
	jsonHandler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(telemetry.NewContextHandler(jsonHandler)), nil
}

func printUsage() {
	fmt.Println(`hardcount — usage-based billing engine

Commands:
  serve                Start the API server (default)
  migrate              Apply pending migrations
  migrate status       Show the upgrade plan: the database's version, the
                       migrations still to apply, and whether one is running
  migrate rollback     Undo the latest migration (APP_ENV=local only; never
                       below 180, the baseline)
  restore begin        Mark a database restored from a snapshot or to a point
                       in time, and pause billing and dunning.
                       scripts/restore.sh does this for a dump. serve
                       refuses to start until the catch-up finishes.
                       Refused while a copy of Hardcount runs on the
                       database, unless --force.
  restore catch-up     Apply what Stripe did since the backup, then let serve
                       start. Flags: --acknowledge pi_…|re_…|evt_…,
                       --stripe-key <tenant>:<live|test>=@<file>,
                       --since <time>
  restore status       Show whether the database is marked as restored, and
                       whether billing and dunning are paused
  version              Print version
  help                 Show this help

Environment:
  DATABASE_URL               PostgreSQL connection string — admin/migration role (or the
                             split DB_HOST/DB_NAME/DB_USER/DB_PASSWORD shape; every setting is in
                             docs/settings.md)
  APP_DATABASE_URL           Runtime connection for the least-privilege hardcount_app role (RLS
                             enforced). Used verbatim; REQUIRED in staging/production. Local
                             dev derives hardcount_app:hardcount_app from DATABASE_URL when unset.
  PORT                       HTTP port (default: 8080)
  APP_ENV                    Environment: local, staging, production (default: local)
  RUN_MIGRATIONS_ON_BOOT     Run migrations on server start (default: false)
  HARDCOUNT_BOOTSTRAP_TOKEN  Token for POST /v1/bootstrap endpoint (min 16 chars outside local)
  PAYMENT_UPDATE_URL         Base URL for payment update page (e.g. https://app.example.com/update-payment)
  PAYMENT_UPDATE_RETURN_URL  Where Stripe Checkout returns after the public payment-update flow; must be a real SPA route, e.g. https://app.example.com/payment-method-added (handler appends ?status=success|cancel)
  HARDCOUNT_ENCRYPTION_KEY   64-char hex key for PII encryption at rest
  REDIS_URL                  Redis URL for distributed rate limiting`)
}

// deriveAppURL replaces the user:password in a DATABASE_URL with
// hardcount_app:hardcount_app. It returns "" if the URL cannot be parsed. It
// is for local development only (ADR-073): staging and production require an
// explicit APP_DATABASE_URL.
func deriveAppURL(adminURL string) string {
	u, err := neturl.Parse(adminURL)
	if err != nil {
		return ""
	}
	u.User = neturl.UserPassword("hardcount_app", "hardcount_app")
	return u.String()
}

// resolveAppURL picks the database URL for the runtime pool, where RLS is
// enforced, by ADR-073's fail-closed rules. It returns one of three results:
//   - url: the URL to open.
//   - fallbackWarn, when not empty: use the admin pool and log this text.
//     Only local development gets this result.
//   - err: an error that must stop the boot.
//
// It has no side effects, so the rules can be unit-tested.
func resolveAppURL(env, adminURL, appURL string) (url, fallbackWarn string, err error) {
	local := env == "local"

	if appURL != "" {
		u, perr := neturl.Parse(appURL)
		if perr != nil || u.User == nil {
			return "", "", fmt.Errorf("APP_DATABASE_URL is not a parseable connection URL with credentials: %v", perr)
		}
		username := u.User.Username()
		password, _ := u.User.Password()
		if !local {
			// Outside local development, refuse the documented default
			// password and the no-password (trust-auth) shape. Either one
			// gives anyone who can reach the database read and write
			// access across every tenant.
			switch password {
			case "":
				return "", "", fmt.Errorf("APP_DATABASE_URL has no password (trust-auth shape) — forbidden in %s; set a real password for the app role (ALTER ROLE %s PASSWORD ...)", env, username)
			case "hardcount_app", username:
				return "", "", fmt.Errorf("APP_DATABASE_URL uses the default/guessable password for role %q — forbidden in %s; rotate it (openssl rand -hex 24, then ALTER ROLE ... PASSWORD) and update APP_DATABASE_URL", username, env)
			}
		}
		return appURL, "", nil
	}

	if !local {
		return "", "", fmt.Errorf("APP_DATABASE_URL is required in %s: the runtime pool must be a least-privilege role with its own password, not one derived from DATABASE_URL with the documented default — see docs/ops/postgres-requirements.md#the-two-database-users", env)
	}

	derived := deriveAppURL(adminURL)
	if derived == "" || derived == adminURL {
		return "", "running with admin database connection — RLS NOT enforced. Create the hardcount_app role.", nil
	}
	return derived, "", nil
}

// checkRLSCapability reports whether the pool's role can bypass RLS.
// Superuser and BYPASSRLS defeat row-level security wholesale; table
// ownership does not (every RLS table is FORCE ROW LEVEL SECURITY).
func checkRLSCapability(pool *sql.DB) (role string, canBypass bool, err error) {
	err = pool.QueryRow(
		`SELECT current_user, (rolsuper OR rolbypassrls) FROM pg_roles WHERE rolname = current_user`,
	).Scan(&role, &canBypass)
	return role, canBypass, err
}

// warnIfNoIdleTxTimeout logs one warning when idle_in_transaction_session_timeout
// is 0 for the pool's role, which Postgres reads as no limit. Without it, a
// Hardcount process that freezes inside a transaction keeps its row locks
// until something ends its session. It warns and does not stop the boot: the
// server works without the setting, and a Compose data volume created before
// the stack set it has none. A failed read is logged too, so a check that did
// not run never looks like a role that passed it.
func warnIfNoIdleTxTimeout(logger *slog.Logger, pool *sql.DB, role string) {
	var timeout string
	if err := pool.QueryRow(`SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&timeout); err != nil {
		logger.Warn("could not read the app database role's idle_in_transaction_session_timeout, so start-up did not check it — see docs/ops/postgres-requirements.md#timeouts-on-the-limited-user.",
			"role", role, "error", err)
		return
	}
	if timeout == "0" {
		logger.Warn("the app database role has no idle-transaction timeout (idle_in_transaction_session_timeout is 0), so a Hardcount process that freezes inside a transaction keeps its locks until its session ends. Set it as the admin role, then restart Hardcount — see docs/ops/postgres-requirements.md#timeouts-on-the-limited-user.",
			"role", role, "statement", "ALTER ROLE "+role+" SET idle_in_transaction_session_timeout = '30s'")
	}
}

// openAppPool returns the connection for request-time queries, where RLS must
// be enforced (ADR-073). APP_DATABASE_URL is used as given when set.
// Staging and production refuse to start without it, with an empty or
// default password, or with a role that can bypass RLS. The RLS capability
// check (checkRLSCapability) catches the easy mistake of copying
// DATABASE_URL into APP_DATABASE_URL, which no check on the URL text can
// see. Local development still derives hardcount_app:hardcount_app. When
// that URL cannot be used, openAppPool falls back to the admin pool with a
// warning. The returned cleanup does nothing after a fallback. On the app
// pool it also warns when the role has no idle-transaction timeout.
func openAppPool(cfg config.Config, adminPool *sql.DB) (*sql.DB, func()) {
	noop := func() {}

	appURL, fallbackWarn, err := resolveAppURL(cfg.Env, cfg.DB.URL, cfg.DB.AppURL)
	if err != nil {
		slog.Error("refusing to start: "+err.Error(), "env", cfg.Env)
		os.Exit(1)
	}
	if fallbackWarn != "" {
		slog.Warn(fallbackWarn)
		return adminPool, noop
	}

	appCfg := cfg.DB
	appCfg.URL = appURL
	appPool, err := config.OpenPostgres(appCfg)
	if err != nil {
		// An APP_DATABASE_URL that is set but does not work stops the boot
		// in every environment: a setting someone chose never falls back
		// quietly. Only the URL derived in local development falls back with
		// a warning, for when the role has not been created.
		if cfg.DB.AppURL != "" || cfg.Env != "local" {
			slog.Error("refusing to start: could not open the app database connection, so RLS would not be enforced and tenants would not be isolated. Check APP_DATABASE_URL and that the role exists with LOGIN — see docs/ops/postgres-requirements.md#the-two-database-users.", "error", err, "env", cfg.Env)
			os.Exit(1)
		}
		slog.Warn("could not open app database connection, falling back to admin", "error", err)
		return adminPool, noop
	}

	role, canBypass, err := checkRLSCapability(appPool)
	if err != nil {
		slog.Error("refusing to start: could not verify the app role's RLS posture", "error", err, "env", cfg.Env)
		os.Exit(1)
	}
	if canBypass {
		if cfg.Env != "local" {
			slog.Error("refusing to start: the APP_DATABASE_URL role can BYPASS row-level security (superuser or BYPASSRLS) — tenants would not be isolated. Point APP_DATABASE_URL at a least-privilege role like hardcount_app, not the admin role — see docs/ops/postgres-requirements.md#the-two-database-users.", "role", role, "env", cfg.Env)
			os.Exit(1)
		}
		slog.Warn("app database role can bypass RLS — acceptable in local dev only", "role", role)
	}

	// The role must also be able to read and write the tables the app uses.
	// Without the default privileges that the schema setup grants, requests
	// would fail later with 500s (#1317). When a grant is missing, the server
	// refuses to start in every environment, and the log names the missing
	// grants.
	if _, missing, err := missingAppGrants(appPool); err != nil {
		slog.Error("refusing to start: could not read the app role's table privileges", "role", role, "error", err)
		os.Exit(1)
	} else if len(missing) > 0 {
		slog.Error("refusing to start: the app database role is missing grants the app needs. Run these as the admin role, then set the default privileges for new tables — see docs/ops/postgres-requirements.md#the-two-database-users.",
			"role", role, "grants", strings.Join(missing, "; "))
		os.Exit(1)
	}

	warnIfNoIdleTxTimeout(slog.Default(), appPool, role)

	slog.Info("using app database connection (RLS enforced)", "role", role)
	return appPool, func() { _ = appPool.Close() }
}

// redisBootFatal is the one place that decides whether a Redis error stops
// the boot. An error opening or pinging Redis is fatal in production only.
func redisBootFatal(env string, err error) bool {
	return err != nil && env == "production"
}
