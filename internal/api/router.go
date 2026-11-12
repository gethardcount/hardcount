package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/redis/go-redis/v9"

	"github.com/gethardcount/hardcount/internal/analytics"
	mw "github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/hostedinvoice"
	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/payment/breaker"
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/recipe"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/user"
	"github.com/gethardcount/hardcount/internal/webhook"

	"github.com/stripe/stripe-go/v82"
)

// --- Scheduler health tracking ---

var (
	schedulerMu       sync.RWMutex
	schedulerLastRun  time.Time
	schedulerInterval time.Duration

	registerQueueGaugesOnce sync.Once

	// draining turns /health/ready into 503 {"status":"draining"} after
	// SIGTERM, while /health (liveness) stays 200. A load balancer then stops
	// sending traffic to this replica before it stops listening, and an
	// orchestrator does not kill it early. SetDraining sets it once, and
	// nothing clears it, because the process is exiting.
	draining atomic.Bool
)

// SetDraining announces shutdown: readiness answers 503 from now on.
func SetDraining() { draining.Store(true) }

// resetDrainingForTest clears the draining flag. Only tests call it.
func resetDrainingForTest() { draining.Store(false) }

// RecordSchedulerRun is called by the billing scheduler after each cycle,
// so the health check can tell whether the scheduler is alive.
func RecordSchedulerRun() {
	schedulerMu.Lock()
	schedulerLastRun = time.Now()
	schedulerMu.Unlock()
	// Export the same fact as a metric. The runbook's scheduler-liveness
	// alert reads /metrics, so the fact cannot live only inside
	// /health/ready.
	mw.RecordSchedulerTick()
}

// SetSchedulerInterval stores the configured scheduler interval. The health
// check flags the scheduler as degraded when more than two intervals pass
// after its last run.
func SetSchedulerInterval(d time.Duration) {
	schedulerMu.Lock()
	schedulerInterval = d
	schedulerMu.Unlock()
}

// Server is the HTTP API. Its exported fields hold the services and stores
// that cmd/hardcount wires into the background workers.
type Server struct {
	router chi.Router

	// Exported so main.go can wire the billing scheduler and dunning.
	BillingEngine    *billing.Engine
	DunningSvc       *dunning.Service
	SettingsStore    *tenant.SettingsStore
	WebhookOutSvc    *webhook.Service
	OutboxStore      *webhook.OutboxStore
	EmailOutboxStore *email.OutboxStore
	EmailSender      *email.Sender
	// EmailSettledChecker feeds the email dispatcher's staleness gate.
	// cmd/hardcount passes it to the dispatcher with SetSettledChecker. The
	// dispatcher skips an action-required email whose invoice was settled
	// while it was queued.
	EmailSettledChecker email.InvoiceSettledChecker
	CreditSvc           *credit.Service
	InvoiceSvc          *invoice.Service
	CreditNoteSvc       *creditnote.Service
	SubscriptionSvc     *subscription.Service
	TokenSvc            *payment.TokenService
	PaymentReconciler   *payment.Reconciler
	// LoginThrottle is the sign-in throttle. main.go adds its Cleanup to the
	// leader's cleanup steps.
	LoginThrottle *user.LoginThrottle

	// TestClockSvc lets main.go run the test-clock catch-up: everything that
	// fell due in the time an advancing test clock skipped, such as trial
	// ends, billing, tax retries, dunning and credit expiries. It runs off
	// the request, as Stripe's test-clock advance does, as the
	// testclock_catchup leader role (ADR-140). The service is built here,
	// and main.go runs its loop with the other background workers.
	TestClockSvc *testclock.Service

	// RestoreCatchUp applies Stripe's events to a restored database through
	// the same webhook handlers serve uses (ADR-135). `hardcount restore
	// catch-up` runs it; serve never does.
	RestoreCatchUp *payment.CatchUp
}

// ServerOption sets a boot-time value that NewServer passes to the services it
// builds. cmd/hardcount passes what config.Load read. Tests pass what they
// exercise, and otherwise run as an install with none of them set.
type ServerOption func(*serverOptions)

type serverOptions struct {
	invoiceGrace time.Duration
	appEnv       string
	rateLimits   config.RateLimits
	stripeKeys   map[tenantstripe.Account]string
}

// WithStripeKeys sets Stripe secret keys that replace the stored ones for
// the life of this Server, and are never saved. Only `hardcount restore
// catch-up --stripe-key` passes it: for a key Stripe refuses since it was
// stored, or an account connected after the backup.
func WithStripeKeys(keys map[tenantstripe.Account]string) ServerOption {
	return func(o *serverOptions) { o.stripeKeys = keys }
}

// WithAppEnv sets the environment to config.Config.Env, which is APP_ENV as
// config.Load normalised it, so every consumer below compares the same
// string. Without it, NewServer runs as "local", as an unset APP_ENV does.
func WithAppEnv(env string) ServerOption {
	return func(o *serverOptions) { o.appEnv = env }
}

// WithRateLimits sets the HARDCOUNT_RATE_LIMIT_* limits config.Load parsed.
// Without it NewServer uses config.DefaultRateLimits.
func WithRateLimits(rl config.RateLimits) ServerOption {
	return func(o *serverOptions) { o.rateLimits = rl }
}

// WithInvoiceGrace sets INVOICE_GRACE_PERIOD (ADR-120). One value feeds the
// two places that must agree on when a period is due: the billing runs' due
// scans and the billing backlog gauges. The credit-expiry sweep does not use
// it: it holds a grant until the close that draws on it has run (ADR-071 §2).
func WithInvoiceGrace(d time.Duration) ServerOption {
	return func(o *serverOptions) { o.invoiceGrace = d }
}

// NewServer builds every store, service and handler, wires them together
// and mounts the routes. A nil clk means the real clock. It panics when a
// required collaborator or audit emitter is not wired, or when the
// built-in recipes fail to load, so a broken build or wiring stops boot.
func NewServer(db *postgres.DB, clk clock.Clock, rdb *redis.Client, opts ...ServerOption) *Server {
	if clk == nil {
		clk = clock.Real()
	}
	o := serverOptions{appEnv: "local", rateLimits: config.DefaultRateLimits()}
	for _, opt := range opts {
		opt(&o)
	}

	// Stores
	invoiceStore := invoice.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	webhookStore := payment.NewPostgresWebhookStore(db)

	// Auth
	authSvc := auth.NewService(auth.NewPostgresStore(db))
	authH := auth.NewHandler(authSvc)

	// Domain handlers
	tenantSvc := tenant.NewService(tenant.NewPostgresStore(db))
	customerStore := customer.NewPostgresStore(db)

	// Encryption at rest uses AES-256-GCM with HARDCOUNT_ENCRYPTION_KEY.
	// Customer PII (email, names, phone, tax IDs), webhook signing secrets and
	// per-tenant Stripe credentials share one encryptor, so a key rotation
	// reaches all of them. Without a key they are stored as plaintext.
	// config.go already requires the key in production.
	var sharedEnc *crypto.Encryptor
	if encKey := strings.TrimSpace(os.Getenv("HARDCOUNT_ENCRYPTION_KEY")); encKey != "" {
		enc, err := crypto.NewEncryptor(encKey)
		if err != nil {
			slog.Error("invalid HARDCOUNT_ENCRYPTION_KEY, encryption at rest disabled", "error", err)
		} else {
			sharedEnc = enc
			customerStore.SetEncryptor(enc)
			invoiceStore.SetEncryptor(enc)
			// The key does not cover email_outbox.payload. A queued or
			// failed email holds its link token or URL as written, and
			// marking the email sent removes it (email.scrubbedPayloadKeys).
			slog.Info("encryption at rest enabled for customer PII, webhook secrets, Stripe credentials, and hosted-invoice tokens on invoices; an unsent email in email_outbox holds its link token unencrypted until it is sent")
		}
	} else {
		slog.Warn("HARDCOUNT_ENCRYPTION_KEY not set — customer PII, webhook secrets, Stripe credentials, and hosted-invoice tokens stored in plaintext")
	}

	// Stripe credentials are per tenant. Each tenant connects its own Stripe
	// account with POST /v1/settings/stripe, and the service looks up the keys
	// on each request. There is no platform-level STRIPE_SECRET_KEY, because
	// Hardcount is a billing engine, not a merchant of record.
	tenantStripeStore := tenantstripe.NewStore(db)
	if sharedEnc != nil {
		tenantStripeStore.SetEncryptor(sharedEnc)
	}
	tenantStripeSvc := tenantstripe.NewService(tenantStripeStore, func(secretKey string) *stripe.Client {
		return stripe.NewClient(secretKey)
	})
	tenantStripeH := tenantstripe.NewHandler(tenantStripeSvc)
	var stripeCreds payment.StripeCredentials = tenantStripeSvc
	if len(o.stripeKeys) > 0 {
		stripeCreds = payment.KeyOverrides(tenantStripeSvc, o.stripeKeys)
	}
	stripeClients := payment.NewStripeClients(stripeCreds)

	// Build the payment-methods service early. The dunning retrier, the
	// invoice handler and the billing engine read payment-method status
	// through it, from the canonical sources (customers.stripe_customer_id and
	// payment_methods), not from the deprecated customer_payment_setups
	// summary.
	paymentMethodsStripe := paymentmethods.NewStripeAdapter(stripeClients, customerStore)
	paymentMethodsStore := paymentmethods.NewPostgresStore(db)
	paymentMethodsSvc := paymentmethods.NewService(paymentMethodsStore, paymentMethodsStripe, customerStore)
	// paymentMethodsSvc gets auditLogger later, once auditLogger exists (see
	// the SetAuditLogger call below).
	//
	// CUSTOMER_PORTAL_URL is the dashboard address a setup session returns
	// the customer to when the caller passes no return_url. It has no
	// default: an address that is wrong for this install would strand the
	// customer, so without it those setup sessions are refused.
	customerPortalURL := strings.TrimSpace(os.Getenv("CUSTOMER_PORTAL_URL"))
	if customerPortalURL == "" {
		slog.Warn("CUSTOMER_PORTAL_URL NOT SET — setup links (Send setup link, and setup sessions with no return_url) are refused with return_address_not_set, because Stripe would have no page to send the customer back to. Set this to your dashboard's address, e.g. https://app.example.com")
	}
	paymentMethodsSvc.SetPortalBaseURL(customerPortalURL)
	// compositePaymentSetupStore serves the customer_payment_setups shape
	// (domain.CustomerPaymentSetup) from the canonical sources. So
	// payment/stripe.go and the dunning retrier keep their PaymentSetupStore
	// wiring, and still never read the deprecated table.
	paymentSetupStore := &compositePaymentSetupStore{customers: customerStore, pms: paymentMethodsSvc}

	pricingSvc := pricing.NewService(pricingStore)
	// A plan's billing fields freeze once any live subscription uses the plan
	// (ADR-034). UpdatePlan counts live subscriptions through subStore, which
	// implements the narrow CountLiveSubsByPlan query.
	pricingSvc.SetSubscriptionPlanUsageReader(subStore)
	pricingSvc.SetCustomerReader(customerStore)
	customerSvc := customer.NewService(customerStore)
	customerSvc.SetStripeSyncer(payment.NewStripeBillingSync(stripeClients))
	customerH := customer.NewHandler(customerSvc)
	pricingH := pricing.NewHandler(pricingSvc)
	subSvc := subscription.NewService(subStore, clk)
	subH := subscription.NewHandler(subSvc)
	// Proration deps are wired below, once creditSvc and invoiceStore exist.
	usageSvc := usage.NewService(usageStore)
	usageH := usage.NewHandler(usageSvc, customerStore, pricingSvc)
	usageH.SetMeterLister(pricingSvc)
	usageH.SetHeldDeps(customerStore)
	customerUsageSvc := usage.NewCustomerUsageService(usageSvc, customerStore, subStore, pricingSvc)
	customerUsageH := usage.NewCustomerUsageHandler(customerUsageSvc)
	// Provider cost rates and margin (ADR-079).
	providerCostH := usage.NewProviderCostHandler(usageStore, usage.NewMarginAssembler(usageStore, customerUsageSvc))
	settingsStore := tenant.NewSettingsStore(db)
	// Subscriptions read tenant settings, so period boundaries snap to the
	// start of the day in the tenant's timezone. This is day-grade calendar
	// billing, the Chargebee and Lago default.
	subSvc.SetSettingsReader(settingsStore)
	// A test clock attaches at the customer level (ADR-027). Subscription
	// create copies the customer's test_clock_id onto the new subscription.
	// The API takes no per-subscription test_clock_id, and neither does
	// Stripe's. Accepting one would add a second validation path beside the
	// customer-level value, which is the canonical one.
	subSvc.SetCustomerReader(customerStore)
	subSvc.SetProfileReader(customerStore)
	// Period anchoring knows about yearly plans. At Create, Activate, EndTrial
	// and ExtendTrial, the subscription service reads the first item's plan to
	// find its BillingInterval. Without the plan reader, the period helpers
	// use fixed monthly math, and a yearly subscription with a trial gets a
	// one-month first cycle instead of a full year.
	subSvc.SetPlanReader(pricingStore)
	subSvc.SetPlanMeterReader(pricingStore)
	creditStore := credit.NewPostgresStore(db)
	creditSvc := credit.NewService(creditStore)
	creditSvc.SetCustomerReader(customerStore)
	creditH := credit.NewHandler(creditSvc)
	creditNoteStore := creditnote.NewPostgresStore(db)
	// The credit-note issue stamp copies customer PII, so it needs the
	// customer store's key (ADR-109).
	if sharedEnc != nil {
		creditNoteStore.SetEncryptor(sharedEnc)
	}

	// Proration dependencies, for plan-change invoices.
	subH.SetProrationDeps(pricingSvc, &prorationInvoiceCreatorAdapter{PostgresStore: invoiceStore, numberer: settingsStore})

	// Payment, webhook, checkout and refund handlers.
	stripeRefunder := payment.NewStripeRefunder(stripeClients)
	creditNoteSvc := creditnote.NewService(creditNoteStore, invoiceStore, stripeRefunder, &creditGrantAdapter{svc: creditSvc})
	creditNoteSvc.SetNumberGenerator(settingsStore)
	creditNoteH := creditnote.NewHandler(creditNoteSvc, creditnote.HandlerDeps{
		Customers: customerStore,
		Settings:  settingsStore,
		Invoices:  invoiceStore,
	})
	webhookOutStore := webhook.NewPostgresStore(db)
	if sharedEnc != nil {
		webhookOutStore.SetEncryptor(sharedEnc)
	}
	webhookOutSvc := webhook.NewService(webhookOutStore, nil, o.appEnv)
	webhookOutH := webhook.NewHandler(webhookOutSvc)

	// Outbound events go through the webhook_outbox table, which the
	// dispatcher worker (cmd/hardcount/main.go) drains into events and their
	// deliveries. The outbox is always on (ADR-040), because dispatching
	// directly gives up every retry guarantee. Every component given
	// outboxStore below queues its events in the transaction of the change
	// each reports, so a crash after the commit cannot lose one.
	outboxStore := webhook.NewOutboxStore(db)
	// Queue-depth gauges, which the Grafana and runbook alerts watch. The
	// counts run on a TxBypass transaction because they span tenants. Under
	// the app role's RLS they would always report a healthy-looking 0.
	// registerQueueGaugesOnce stops a second promauto registration when
	// NewServer runs twice in one process, as integration tests do.
	registerQueueGaugesOnce.Do(func() {
		mw.RegisterQueueDepthGauges(o.invoiceGrace, func(query string) (float64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				return 0, err
			}
			defer postgres.Rollback(tx)
			var n float64
			if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
				return 0, err
			}
			return n, nil
		})
	})
	// The invoice store queues every invoice.* event in the transaction of
	// its change: invoice.paid with the move to paid, so it fires exactly
	// once on every settlement path, and invoice.finalized, .voided,
	// .marked_uncollectible and .payment_recorded with theirs.
	invoiceStore.SetOutboxEnqueuer(outboxStore)
	// credit.balance_low, _depleted and _recovered are queued in every
	// transaction that writes the ledger, through the same outbox (ADR-078).
	creditStore.SetOutboxEnqueuer(outboxStore)
	// Every subscription.* event is queued in the transaction of its change:
	// the store's transition writers emit the lifecycle events for the
	// operator API, the engine's schedule paths and the trial sweeps, and
	// the item changes and the billing engine's threshold fire and
	// scheduled plan changes queue theirs through the same store. Because
	// the event commits with its change, a crash after the commit cannot
	// drop an event that a provisioning webhook consumer depends on.
	subStore.SetOutboxEnqueuer(outboxStore)
	// customer.email_bounced and .email_complained are queued in the
	// transaction of the suppression write each reports, so a bounce or a
	// complaint that is saved always sends its event.
	customerStore.SetOutboxEnqueuer(outboxStore)
	// payment_method.attached and .updated are queued in the transaction of
	// the attach each reports, so a card that is saved always sends its event.
	paymentMethodsStore.SetOutboxEnqueuer(outboxStore)
	// Prepaid commits are funded at finalize (ADR-078 D2). FinalizeWithDates
	// grants the commit block through credit.Service, in the finalize
	// coordinator transaction.
	invoiceStore.SetCommitFunder(creditSvc)
	// When the invoice that a dunning pause was set for is paid, the paid
	// transaction lifts the pause (ADR-036 amendment 2026-10-22).
	invoiceStore.SetDunningPauseReleaser(subStore)
	auditLogger := audit.NewLogger(db)
	// The audit logger decrypts actor names. actor_name comes from a LEFT
	// JOIN at read time, and customers.display_name is encrypted at rest.
	// Without this call, every row with a customer actor (a hosted-invoice
	// Pay click, a payment-update link) would show ciphertext as its actor in
	// production, on the dashboard and in the CSV handed to an auditor.
	// SetEncryptor accepts nil and does nothing when HARDCOUNT_ENCRYPTION_KEY
	// is unset.
	auditLogger.SetEncryptor(sharedEnc)
	auditH := audit.NewHandler(auditLogger)
	// The customer handler's audit logger is currently used to record
	// cost-dashboard token rotations, without writing the plaintext token to
	// the audit trail.
	customerH.SetAuditLogger(auditLogger)
	// costDashboardSvc builds the public cost dashboard. It finds the customer
	// from the token, builds the usage view and sanitizes the response.
	// customerH gets it so one handler serves both the operator's rotate
	// endpoint and the public GET-by-token route (see ADR-031).
	costDashboardSvc := usage.NewCostDashboardAssembler(customerStore, customerUsageSvc, subStore, costDashboardCredits{svc: creditSvc})
	customerH.SetCostDashboardService(costDashboardSvc)
	// Warn at boot when HARDCOUNT_API_BASE_URL is unset, as for the
	// HOSTED_INVOICE_BASE_URL, PAYMENT_UPDATE_* and DASHBOARD_BASE_URL
	// warnings below. Unset, the rotate response's public_url is a bare path,
	// and the operator's Copy button gives them a link a customer cannot open.
	// Building an origin from the request would be a guess, because the API
	// origin is not the dashboard origin. So the setting stays explicit, and
	// the warning stays.
	if base := strings.TrimSpace(os.Getenv("HARDCOUNT_API_BASE_URL")); base != "" {
		customerH.SetAPIBaseURL(base)
	} else {
		slog.Warn("HARDCOUNT_API_BASE_URL NOT SET — the public cost-dashboard URL operators copy will be a relative path, not a shareable link. Set this to your public API origin (e.g. https://api.example.com).")
	}
	settingsH := tenant.NewSettingsHandler(settingsStore)
	// The settings handler audits each change field by field, with the before
	// and after values. It is the only record of a settings change (ADR-090).
	// The boot gate below (audit.MustWired) makes sure this wiring is never
	// dropped.
	settingsH.SetAuditLogger(auditLogger)
	stripeClient := payment.NewLiveStripeClient(stripeClients)
	dunningStore := dunning.NewPostgresStore(db)
	// Every dunning.* event is queued in the transaction of the run write it
	// reports: StartRun queues dunning.started with the run and its timeline
	// event, the escalation queues dunning.escalated, and the resolve's
	// compare-and-set queues dunning.resolved for its winner.
	dunningStore.SetOutboxEnqueuer(outboxStore)
	dunningSvc := dunning.NewService(dunningStore, nil, clk) // retrier set below after stripeAdapter created
	dunningH := dunning.NewHandler(dunningSvc)

	// The recipe registry holds the built-in pricing recipes, loaded once at
	// boot from the embedded YAML. A load failure is fatal. A malformed recipe
	// would crash the first time it is instantiated, and TestLoad checks the
	// recipes in CI. So reaching this panic means a broken build that should
	// not run.
	recipeRegistry, err := recipe.Load()
	if err != nil {
		panic(fmt.Sprintf("load recipe registry: %v", err))
	}
	recipeStore := recipe.NewPostgresStore(db)
	recipeSvc := recipe.NewService(db, recipeStore, recipeRegistry, pricingSvc)
	recipeH := recipe.NewHandler(recipeSvc)

	// One shared circuit breaker guards Stripe calls, for every caller and
	// tenant. When Stripe is unhealthy (5xx, timeout, network error), every
	// caller backs off together, so retries do not pile onto a struggling
	// dependency. It opens after 5 consecutive Unknown failures and probes
	// after 30s. It reports each state change on the
	// hardcount_stripe_breaker_state gauge, for operator alerts.
	stripeBreaker := breaker.New(breaker.Config{
		FailureThreshold: 5,
		Cooldown:         30 * time.Second,
		Interval:         60 * time.Second,
		Countable:        payment.IsUnknownPaymentFailure,
		OnStateChange: func(_, to breaker.State) {
			mw.RecordStripeBreakerState(string(to))
		},
	})

	stripeAdapter := payment.NewStripe(stripeClient, invoiceStore, webhookStore, paymentSetupStore, dunningSvc)
	stripeAdapter.SetCardFetcher(stripeClient)
	// Payment anomalies are stored on the invoice (ADR-068). The settle path
	// records a duplicate charge, an amount mismatch, a charge in another
	// currency or a paid voided invoice on the invoice row, and the invoice
	// store queues the anomaly's webhook event in that write's transaction.
	// The dashboard attention banner shows them as Critical, even on an
	// invoice in a terminal state, where it otherwise returns early.
	stripeAdapter.SetAnomalyRecorder(invoiceStore)
	// The checkout-session store is the claim ledger. The adapter keeps it in
	// step with checkout.session.completed, and at settle time tries to
	// expire, at Stripe, any session still open (ADR-068).
	checkoutSessionStore := payment.NewCheckoutSessionStore(db)
	stripeAdapter.SetCheckoutSessionStore(checkoutSessionStore, stripeClients)
	stripeAdapter.SetBreaker(stripeBreaker)
	// Refund webhooks carry the later outcome of a refund (pending to
	// succeeded or failed) onto its credit note. The create call sees only the
	// first state.
	stripeAdapter.SetRefundStatusUpdater(creditNoteSvc)
	// Card disputes (charge.dispute.*) are recorded against the disputed
	// invoice, with payment.dispute_opened or .closed queued in the same
	// transaction (#979).
	stripeAdapter.SetDisputeRecorder(invoiceStore)
	// A refund made in Stripe outside Hardcount is recorded (ADR-134), as an
	// issued credit note whose refund is already done or as a flagged row.
	// payment.refunded_in_stripe is queued in the same transaction. The
	// credit-note domain owns these records. It finds the invoice by
	// PaymentIntent through the invoice store's dispute lookup.
	creditNoteSvc.SetStripeRefundRecording(creditNoteStore, invoiceStore, outboxStore)
	if stripeRefunder != nil {
		creditNoteSvc.SetRefundReader(stripeRefunder)
	}
	stripeAdapter.SetExternalRefundRecorder(creditNoteSvc)
	// A settled card payment resolves the invoice's active dunning run at
	// once. The background-settle paths resolve it through the engine (#317).
	// dunningSvc implements ResolveByInvoice.
	stripeAdapter.SetDunningResolver(dunningSvc)
	// When a dunning retry's payment comes back processing, the run waits on
	// that PaymentIntent. If the payment later fails, the failure settle's
	// transaction records the failure on the run, with the retry's warning
	// email (#1289). dunningSvc implements RetryPaymentFailed. The settle's
	// own write frees the retry's charge hold.
	stripeAdapter.SetDunningRetryFailure(dunningSvc)
	// A charge Stripe refuses for the key (401/403) shows on the Settings →
	// Payments card, as a failed Connect check does.
	stripeAdapter.SetKeyHealth(tenantStripeSvc)
	// A credit note that leaves a dunned invoice with nothing due marks it
	// paid. The resolver then closes the invoice's dunning run, escalated runs
	// included.
	creditNoteSvc.SetDunningResolver(dunningSvc)

	// The payment retrier needs stripeAdapter, so it is wired here.
	dunningSvc.SetRetrier(&paymentRetrierAdapter{
		charger:       stripeAdapter,
		invoiceStore:  invoiceStore,
		paymentSetups: paymentSetupStore,
		credits:       creditSvc,
		stripeReady:   stripeClients,
	})
	// These dunning adapters are wired before invoiceSvc exists. The pauser
	// and canceler depend only on subscription.Service and the invoice store,
	// both built above.
	dunningSvc.SetSubscriptionPauser(&subscriptionPauserAdapter{svc: subSvc}, invoiceStore)
	dunningSvc.SetSubscriptionCanceler(&subscriptionCancelerAdapter{svc: subSvc})
	dunningSvc.SetSubscriptionStateReader(&subscriptionStateAdapter{svc: subSvc})
	// The dunning service reads each customer's dunning_policy_id, to pick the
	// effective policy at StartDunning (ADR-036).
	dunningSvc.SetCustomerPolicyReader(customerSvc)
	webhookH := payment.NewHandler(stripeAdapter, tenantStripeSvc)

	invoiceSvc := invoice.NewService(invoiceStore, clk, settingsStore)
	// Every committed void stops collection at Stripe and closes the invoice's
	// dunning run (invoice.Service.AfterVoid), whichever path voided it: the
	// operator, a dunning resolve, or the cancel or plan-swap relief. Cancel
	// or plan-swap relief is the credit note or void for the unused part of
	// an in_advance period.
	invoiceSvc.SetCollectionStopper(stripeAdapter)
	invoiceSvc.SetDunningResolver(dunningSvc)
	// Finalize issues any relief held on a draft before the invoice can be
	// charged.
	invoiceSvc.SetHeldReliefIssuer(heldReliefIssuerAdapter{svc: creditNoteSvc})
	// The refunded_in_stripe notice, the timeline's refund rows and the
	// invoice's stripe-refunds routes read the credit-note domain's records
	// of refunds made in Stripe (ADR-134).
	invoiceSvc.SetStripeRefunds(creditNoteSvc)
	// Mark uncollectible is a dunning terminal action, as in Stripe (ADR-036
	// amendment). Its adapter is wired here because it needs the invoice
	// service, built just above. The other dunning adapters (pauser, canceler)
	// are wired earlier, next to dunningSvc.
	dunningSvc.SetInvoiceUncollectibleMarker(&invoiceUncollectibleAdapter{svc: invoiceSvc})
	// Dunning's final action takes the invoice's charge hold (ADR-127). So no
	// charge starts while it cancels or pauses the subscription, and it does
	// not run while a lost charge waits for the stale-hold sweep.
	dunningSvc.SetChargeHoldClaimer(invoiceStore)
	// The dunning manual-resolve void goes through the invoice service, not
	// the raw store. The service is the one void writer: it runs the guards,
	// the in-flight block, the tax reversal and the invoice.voided event. Like
	// the marker above, it is wired late, once invoiceSvc exists.
	dunningH.SetInvoiceVoider(invoiceSvc)
	// The tax retry after a Stripe connect (ADR-019). When an operator
	// connects or reconnects Stripe in Settings → Payments, the tenantstripe
	// service starts a goroutine that retries every invoice stuck on
	// provider_not_configured or provider_auth.
	tenantStripeSvc.SetStuckRetrier(invoiceSvc)
	invoiceH := invoice.NewHandler(invoiceSvc, customerStore, settingsStore, invoice.HandlerDeps{
		CreditNotes:       &creditNoteListerAdapter{svc: creditNoteSvc},
		IssuedCreditNotes: &creditNoteListerAdapter{svc: creditNoteSvc},
		Charger:           stripeAdapter,
		PaymentSetups:     paymentSetupStore,
		Dunning:           dunningSvc,
		DunningTimeline:   &dunningTimelineAdapter{store: dunningStore},
		RefundIssuer:      &refundIssuerAdapter{svc: creditNoteSvc},
	})
	// tokenSvc mints the tokens in public payment-update links. It is used by
	// the email sent when an invoice is finalized with no payment method
	// (noPaymentMethodNotifierAdapter) and by the public payment-update SPA
	// flow that the token authenticates. The post-decline path
	// (Stripe.handlePaymentFailed) mints no single-use token. Like the dunning
	// email, it points the customer at the long-lived hosted invoice URL,
	// where they can update the payment method and retry (ADR-023).
	tokenSvc := payment.NewTokenService(db)

	// The reconciler resolves PaymentUnknown invoices, whose Stripe outcome is
	// ambiguous. A 60s cool-off lets a webhook resolve the state first, before
	// the reconciler spends an extra API call.
	paymentReconciler := payment.NewReconciler(stripeClient, invoiceStore, 60*time.Second)
	paymentReconciler.SetBreaker(stripeBreaker)
	// The reconciler settles a recovered final outcome through the shared
	// settlement primitive (ADR-049 Phase 2). So a recovery from a dropped
	// webhook starts dunning, notifies and emits the event, instead of only
	// changing payment_status. As a backstop for dropped webhooks, it also
	// sweeps stale 'processing' invoices, not only 'unknown' ones. The 30m
	// processing cool-off lets the webhook win the common race, because a
	// healthy card PaymentIntent settles in seconds. A webhook that was truly
	// dropped is still resolved within about one extra tick.
	paymentReconciler.SetSettler(stripeAdapter)
	paymentReconciler.SetProcessingReconcileAfter(30 * time.Minute)
	if stripeClient != nil {
		// It also finds finished hosted sessions that no webhook reported
		// (reconciler_hosted.go).
		paymentReconciler.SetHostedSessions(checkoutSessionStore, stripeClient, stripeAdapter)
	}
	// The engine does not exist yet, so paymentReconciler's resolver is wired
	// below at line ~640, with dunningSvc's and stripeAdapter's. Keep this
	// declaration near the constructor. The resolver-wiring block is where to
	// find every clock.Resolver hookup in the router.
	//
	// PAYMENT_UPDATE_RETURN_URL is where Stripe returns a customer who added a
	// card from the "add a payment method" email. It has no default. Without
	// it the page refuses to add the card, so the email is not sent either.
	paymentUpdateReturnURL := strings.TrimSpace(os.Getenv("PAYMENT_UPDATE_RETURN_URL"))
	if paymentUpdateReturnURL == "" {
		slog.Warn("PAYMENT_UPDATE_RETURN_URL NOT SET — \"add a payment method\" emails are not sent, and an existing link's page refuses to add the card, because Stripe would have no page to send the customer back to. Set this to your dashboard's /payment-method-added page, e.g. https://app.example.com/payment-method-added")
	}
	publicPaymentH := payment.NewPublicPaymentHandler(tokenSvc, db, stripeClients, customerSvc, paymentMethodsStripe,
		paymentUpdateReturnURL)

	// paymentmethods.StripeAdapter is built early so the hosted-invoice Pay
	// flow can use it. Before it creates the Checkout session, the flow makes
	// sure a Stripe customer exists for the Hardcount customer. Stripe saves
	// the payment method to the customer's Stripe record only when the
	// session has `customer` and `setup_future_usage=off_session`, and
	// `customer` needs that Stripe customer to exist. Otherwise it is a guest
	// checkout, and "save card" does nothing. The store and service are built
	// later (line ~657), with the rest of the customer-portal stack.
	//
	// paymentMethodsStripe, its store and its service are built earlier,
	// right after stripeClients. So the dunning retrier, the invoice handler
	// and the billing engine all use the canonical payment-status view
	// (compositePaymentSetupStore), with no reads from two tables.

	// The hosted invoice page is the equivalent of Stripe's
	// hosted_invoice_url. Its public routes take a token, and the partner's
	// end customer opens them from an email. The Stripe adapter picks live or
	// test keys from the invoice row's livemode. HOSTED_INVOICE_BASE_URL is
	// the page's origin: the email links point there, and Stripe Checkout
	// returns the customer there. It has no default. Without it the emails
	// carry no link and the page offers no Pay button.
	//
	// The adapter is a named variable, not an inline literal, so the ADR-090
	// boot gate below can check that its audit emitter is wired.
	hostedInvoiceBaseURL := strings.TrimSpace(os.Getenv("HOSTED_INVOICE_BASE_URL"))
	hostedInvoiceStripe := &hostedInvoiceStripeAdapter{clients: stripeClients, ensurer: paymentMethodsStripe, sessions: checkoutSessionStore, audit: auditLogger, belowMinimum: stripeAdapter}

	hostedInvoiceH := hostedinvoice.New(hostedinvoice.Deps{
		Invoices:    invoiceSvc,
		Customers:   customerStore,
		Settings:    settingsStore,
		CreditNotes: &creditNoteListerAdapter{svc: creditNoteSvc},
		Stripe:      hostedInvoiceStripe,
		StripeReady: stripeClients,
		BaseURL:     hostedInvoiceBaseURL,
	})

	// Email goes through the outbox. Producers always queue into email_outbox
	// through OutboxSender. The dispatcher worker (cmd/hardcount/main.go)
	// drains the queue and calls the *email.Sender below for the SMTP round
	// trip. So email delivery survives crashes and brief SMTP failures, and
	// operators get a dead-letter queue (DLQ) to inspect. Producers never send
	// through SMTP directly (ADR-040).
	emailSender := email.NewSender(o.appEnv)
	// Customer-facing emails take each tenant's branding from its settings:
	// logo, brand color, company name and support URL. A new tenant with no
	// settings gets the Hardcount defaults.
	emailSender.SetSettingsGetter(settingsStore)
	// The invoice and final-notice emails offer to pay online only when the
	// hosted page will. They read the page's own Stripe check at send time.
	emailSender.SetStripeReadiness(stripeClients)
	// Warn at boot when SMTP is not configured, so operators do not run
	// production without it and learn only when a customer misses an invoice.
	// Without SMTP, Sender returns ErrSMTPNotConfigured on each send, and each
	// customer-facing email producer reports that error in its own request
	// handler or job log. This one boot line stands out above that
	// per-request volume.
	if !emailSender.IsConfigured() {
		slog.Warn("SMTP NOT CONFIGURED — every customer-facing email (invoices, receipts, dunning, payment-setup links, password resets, team invites) will fail with ErrSMTPNotConfigured. Set SMTP_HOST + credentials. See docs/ops/email-setup.md.")
	} else {
		slog.Info("SMTP configured", "host", emailSender.SMTPHost())
	}
	// One warning covers both effects of an unset HOSTED_INVOICE_BASE_URL.
	// The email sender reads the same setting itself (email.NewSender).
	if hostedInvoiceBaseURL == "" {
		slog.Warn("HOSTED_INVOICE_BASE_URL NOT SET — invoice, receipt, dunning and payment-failed emails carry no payment link, and the hosted invoice page offers no Pay button. Set this to your hosted invoice page's address (e.g. https://billing.example.com).")
	}
	// bounceReporterAdapter passes the Sender's SMTP 5xx errors to
	// customer.MarkEmailBounced. It needs the email blinder, set up further
	// down from HARDCOUNT_EMAIL_BIDX_KEY. The blinder computes the email blind
	// index, a keyed hash that finds a customer by email without decrypting.
	// Without the blinder the adapter returns early. Bounces are still
	// logged, and customer.email_status stays 'unknown'. The adapter is
	// attached inside the blinder branch below, once the blinder exists.
	emailOutboxStore := email.NewOutboxStore(db)
	// The invoice timeline shows the emails sent to the customer. Without
	// this wiring, the timeline still renders, but operators cannot see that
	// the customer was told about no_payment_method, payment_failed or
	// dunning events.
	invoiceH.SetEmailEvents(&invoiceEmailEventsAdapter{store: emailOutboxStore})
	// customerSentEmailsAdapter lists the "Sent emails" section on the
	// customer detail page. This follows Stripe, which shows an email log with
	// a 30-day window on the customer page
	// (docs.stripe.com/invoicing/send-email).
	customerH.SetSentEmailsLister(&customerSentEmailsAdapter{store: emailOutboxStore})
	// Postmark's Delivery, Bounce and SpamComplaint webhooks are ingested
	// (ADR-098). There is one platform-level route, because one Postmark
	// account serves all tenants. Unlike Stripe, where each tenant brings its
	// own account, there is no per-tenant secret. The tenant and livemode come
	// from the stamped outbox row. The suppressor is attached inside the
	// blinder branch below.
	postmarkWebhookUser := strings.TrimSpace(os.Getenv("POSTMARK_WEBHOOK_USER"))
	postmarkWebhookPass := strings.TrimSpace(os.Getenv("POSTMARK_WEBHOOK_PASS"))
	postmarkH := email.NewPostmarkHandler(emailOutboxStore, postmarkWebhookUser, postmarkWebhookPass)
	if postmarkWebhookUser == "" || postmarkWebhookPass == "" {
		slog.Warn("POSTMARK_WEBHOOK_USER/POSTMARK_WEBHOOK_PASS not set — /v1/webhooks/postmark rejects all posts (401); delivery/bounce webhooks will not ingest")
	}
	// One OutboxSender serves every producer. The seven typed interface
	// variables below are per-domain views, each fitting one consumer's narrow
	// interface, and all of them are the same OutboxSender. The blinder-wiring
	// block below attaches the suppression checker to emailOutboxSenderRef,
	// the shared reference.
	outboxSender := email.NewOutboxSender(emailOutboxStore)
	emailOutboxSenderRef := outboxSender
	var (
		invoiceEmail       invoice.EmailSender        = outboxSender
		dunningEmail       dunning.EmailNotifier      = outboxSender
		receiptEmail       payment.EmailReceipt       = outboxSender
		paymentSetupEmail  paymentSetupEmailSender    = outboxSender
		paymentFailedEmail payment.EmailPaymentFailed = outboxSender
		passwordResetEmail interface {
			SendPasswordReset(ctx context.Context, tenantID, to, displayName, resetURL string) error
		} = outboxSender
		setupLinkEmail paymentmethods.SetupLinkEmailer = outboxSender
	)
	invoiceH.SetEmailSender(invoiceEmail)
	// Operator email actions answer 409 email_not_configured when SMTP is not
	// set up. Otherwise the queued email would fail unseen after the action
	// reported "sent".
	invoiceH.SetEmailConfigured(emailSender.IsConfigured())
	customerEmailAdapter := &customerEmailFetcherAdapter{store: customerStore}
	dunningSvc.SetEmailNotifier(dunningEmail)
	dunningSvc.SetCustomerEmailFetcher(customerEmailAdapter)
	stripeAdapter.SetEmailReceipt(receiptEmail, customerEmailAdapter)
	paymentUpdateURL := strings.TrimSpace(os.Getenv("PAYMENT_UPDATE_URL"))
	if paymentUpdateURL == "" {
		slog.Warn("PAYMENT_UPDATE_URL NOT SET — payment-setup-request emails (no-PM-at-finalize) will fail at send time. Set this to your customer-portal payment-update page URL.")
	}
	// The no-payment-method notice says a setup link is emailed, and offers
	// Resend, only when this server can send one. The setup-link sender
	// refuses without PAYMENT_UPDATE_URL or PAYMENT_UPDATE_RETURN_URL, even
	// when SMTP works.
	setupLinksEmailable := canEmailSetupLinks(emailSender.IsConfigured(), paymentUpdateURL, paymentUpdateReturnURL)
	invoiceSvc.SetSetupLinkEmailConfigured(setupLinksEmailable)
	// The payment-failure and no-payment-method notices offer the hosted
	// invoice page and a copied setup link only when those pages can work.
	invoiceSvc.SetHostedPayConfigured(hostedInvoiceBaseURL != "")
	invoiceSvc.SetSetupLinkCopyConfigured(customerPortalURL != "")
	// Without Stripe the customer is emailed the invoice instead, which needs
	// SMTP only (ADR-060 amendment 2026-10-30).
	invoiceSvc.SetInvoiceEmailConfigured(emailSender.IsConfigured())
	stripeAdapter.SetEmailPaymentFailed(paymentFailedEmail, customerEmailAdapter)

	// Audit logging for financial operations.
	// credit: the service writes the grant and adjust audit rows in the ledger
	// transaction (ADR-090).
	creditSvc.SetAuditLogger(auditLogger)
	// creditnote: Issue() and relief write their rows on the coordinator
	// transaction. That gives one credit_note.issued row for the operator,
	// engine, reconciler and catch-up paths. A refund webhook's status change
	// writes its row on the store transaction.
	creditNoteSvc.SetAuditLogger(auditLogger)
	// payment: the payment-method change from checkout.session.completed,
	// which a webhook drives, and the public payment-update token's consume
	// and restore pair. The hosted-invoice checkout claim gets its emitter
	// when its adapter is built (the hostedInvoiceStripeAdapter literal
	// above).
	stripeAdapter.SetAuditLogger(auditLogger)
	if publicPaymentH != nil {
		publicPaymentH.SetAuditLogger(auditLogger)
	}
	invoiceH.SetAuditLogger(auditLogger)
	// Timeline lifecycle rows join their wall-clock audit stamps (ADR-104).
	invoiceH.SetAuditStamps(&invoiceAuditStampsAdapter{logger: auditLogger})
	// Invoice activity rows link the invoice's subscription by name (ADR-128).
	invoiceH.SetSubscriptionLabeler(&invoiceSubscriptionLabelAdapter{store: subStore})
	// The Dunning page's recovery timeline shows the same lines as the
	// invoice's Activity card for the same events (ADR-128). The invoice
	// handler builds both from the same sources.
	dunningH.SetEventLiner(invoiceH)
	subH.SetAuditLogger(auditLogger)
	// Subscription activity rows link the invoices they name (ADR-128).
	subH.SetInvoiceLabeler(&subscriptionInvoiceLabelAdapter{store: invoiceStore})
	creditNoteH.SetAuditLogger(auditLogger)
	// The credit-note document email (ADR-082 rider) goes through the
	// transactional outbox, like every other customer email.
	creditNoteH.SetEmailSender(outboxSender)
	creditNoteH.SetEmailConfigured(emailSender.IsConfigured())
	// paymentmethods.Service audits attach, setDefault and detach, so they
	// show in the operator Activity feed and on the AuditLog page. Without it,
	// operators cannot see the card changes a customer makes.
	paymentMethodsSvc.SetAuditLogger(auditLogger)
	// paymentMethodsH, the handler, is built further down. Its operator-only
	// send-setup-email path is wired right after its NewHandler call, so the
	// declaration and its wiring stay together.
	//
	// Handler-side audit for API keys, dunning policies, pricing, the Stripe
	// connection and the webhook endpoint lifecycle. Each gives operator
	// state changes an audit trail. testClockH is wired after its declaration
	// further down.
	authH.SetAuditLogger(auditLogger)
	dunningH.SetAuditLogger(auditLogger)
	pricingH.SetAuditLogger(auditLogger)
	tenantStripeH.SetAuditLogger(auditLogger)
	// In-transaction emitters (ADR-090). These are service-side seams,
	// distinct from the handler seams above, which log after the fact.
	customerSvc.SetAuditLogger(auditLogger)
	pricingSvc.SetAuditLogger(auditLogger)
	recipeSvc.SetAuditLogger(auditLogger)
	providerCostH.SetAuditLogger(auditLogger)
	// usage: backfill only. Live machine ingest is not audited, by design,
	// because usage_events is the record. A backdated operator insert is a
	// money-path action, so its audit row goes in the ingest transaction.
	usageSvc.SetAuditLogger(auditLogger)
	webhookOutH.SetAuditLogger(auditLogger)
	// The subscription service audits its own state changes. Because the row
	// is written in the service, a change gets one audit row whichever entry
	// point it comes from (the operator handler, the dunning adapter, any
	// future caller).
	subSvc.SetAuditLogger(auditLogger)
	// The invoice service audits its own transitions. MarkUncollectible is
	// reachable from the operator dashboard, from dunning's terminal action
	// and from ResolveRun(invoice_not_collectible). All three go through the
	// service, so each gives one audit row, whatever the trigger. The
	// invoice.* webhook events are queued by the invoice store, in the
	// transaction of each change (invoiceStore.SetOutboxEnqueuer above).
	invoiceSvc.SetAuditLogger(auditLogger)

	// Test clocks simulate frozen time, in test mode only. The billing engine
	// reads clock state from the store; the clock service, built after the
	// engine, runs the engine in its catch-up.
	testClockStore := testclock.NewPostgresStore(db)
	// A test clock attaches at the customer level (ADR-027), and the customer
	// service checks test_clock_id at create. This is wired once the store
	// exists. Without it, customer create always refuses a test_clock_id, as
	// a safe default.
	customerSvc.SetTestClockChecker(testClockStore)
	customerSvc.SetDunningPolicyReader(dunningStore)
	// The archive and currency guard (ADR-067). customer.Update refuses to
	// archive a customer while any subscription still bills.
	// UpsertBillingProfile refuses a currency that would re-denominate an
	// active subscription's plan prices. The adapter lives here, so the
	// customer and subscription packages do not import each other.
	customerSvc.SetSubscriptionChecker(&subscriptionCheckerAdapter{subs: subStore, plans: pricingSvc})
	// A billing-profile save (the operator edits country, postal_code, state
	// or tax_id) retries tax on every draft invoice of that customer stuck on
	// customer_data_invalid. It is the per-customer sibling of the ADR-019
	// flush after a Stripe reconnect. Without it, the operator must click
	// Retry on each invoice after fixing the address.
	customerSvc.SetTaxFlusher(invoiceSvc)

	// Billing engine and manual trigger (with credit auto-application).
	// paymentReadiness answers "can we charge this customer?" in one call,
	// from customers.stripe_customer_id and the canonical payment_methods. It
	// replaces the legacy customer_payment_setups summary table.
	paymentReadiness := &paymentReadinessAdapter{customers: customerStore, pms: paymentMethodsSvc}
	engine := billing.NewEngine(subStore, usageStore, pricingSvc,
		&invoiceWriterAdapter{PostgresStore: invoiceStore}, creditSvc, settingsStore, paymentReadiness, stripeAdapter, clk, customerStore)
	// billing_intervals is the only segment source (ADR-101).
	engine.SetIntervalReader(subStore)
	engine.SetInvoiceGrace(o.invoiceGrace)
	engine.SetTestClockReader(testClockStore)
	testClockSvc := testclock.NewService(testClockStore, engine)
	// The attached-customers panel reads through the customer service, so
	// display_name and email arrive decrypted. The customer package owns the
	// encryption at rest. Without this, the testclock domain would have to
	// read the customers table directly and would skip decryption.
	testClockSvc.SetCustomerReader(customerSvc)
	testClockSvc.SetSubscriptionLister(subStore)
	testClockH := testclock.NewHandler(testClockSvc)
	testClockH.SetAuditLogger(auditLogger)
	// The engine writes audit_log rows for lifecycle events it fires in the
	// background (currently a scheduled cancellation firing at period end).
	// Without it, the subscription's Activity timeline shows "Cancellation
	// scheduled" with no "Subscription canceled" to follow it.
	engine.SetAuditLogger(auditLogger)
	// The customer reader backs SimForCustomer, used by subscription create,
	// the one-off invoice composer and the clock-pinned customer path. A
	// clock-pinned customer is one attached to a test clock. Without the
	// reader, the engine resolves a customer's clock to the wall clock, and
	// operator creates for clock-pinned customers break simulated time.
	engine.SetCustomerReader(customerStore)
	// The engine computes the live credit figure: the settled balance less
	// unbilled usage (#1019). Only the engine prices usage the way the period
	// close does. The secret-key read and the public cost dashboard both serve
	// that one function, so they cannot disagree.
	engine.SetLiveBalanceReaders(creditSvc, subStore)
	// Customer usage, the public cost dashboard and the margin report price
	// usage through the engine's rater (ADR-130 §10).
	customerUsageSvc.SetRater(engine)
	creditSvc.SetLiveBalanceReader(engine)
	costDashboardSvc.SetLiveBalanceReader(engine)
	// Dunning resolves "now" through the engine. Every per-invoice time it
	// writes (next_action_at, last_attempt_at, resolved_at, created_at) then
	// uses the clock that the catch-up query reads: frozen_time for a
	// clock-pinned run, the wall clock otherwise. Without it, a clock-pinned
	// run's times land in wall-clock 2026, and the catch-up window, which
	// compares against the frozen_time the operator advances through, never
	// reaches the run (ADR-029).
	dunningSvc.SetResolver(engine)
	// The dunning handler's resolveRun binds the clock from the invoice
	// before MarkPaid, so `invoice.paid_at` is in simulated time on a
	// clock-pinned invoice, not in wall-clock time.
	dunningH.SetResolver(engine)
	// The payment reconciler is the scheduled fallback that resolves invoices
	// stuck in PaymentUnknown by polling Stripe for the real outcome. Without
	// the resolver, paid_at on a clock-pinned invoice would be wall-clock
	// time, disagreeing with issued_at, due_at and billing_period on the same
	// row (ADR-030).
	paymentReconciler.SetResolver(engine)
	// The Stripe webhook handlers (handlePaymentSucceeded,
	// handlePaymentFailed) run asynchronously, with no clock bound on their
	// ctx. The resolver puts every write a webhook drives (paid_at, the
	// payment-failure stamp, the dunning-run create) in simulated time on a
	// clock-pinned invoice, not in wall-clock time.
	stripeAdapter.SetResolver(engine)
	// Customer-service writes (Update, UpsertBillingProfile and others) bind
	// the effective now from the customer's clock, so updated_at on a
	// clock-pinned customer's row is in simulated time.
	customerSvc.SetResolver(engine)
	// A credit grant for a clock-pinned customer must stamp the ledger
	// entry's created_at in simulated time. Then the dashboard's "credit
	// granted on …" line matches the rest of the simulated timeline.
	creditSvc.SetResolver(engine)
	// Usage ingest for a clock-pinned customer defaults event times to the
	// clock's frozen_time and checks them against it. So usage in simulated
	// time can be ingested onto an advanced clock. Every ingest path goes
	// through here: live POST, batch, backfill and LiteLLM. Live mode never
	// pays for the lookup, because a DB CHECK keeps test clocks in test mode.
	usageSvc.SetResolver(engine)
	// Ingest reads the customer's open periods before it writes an event. It
	// flags an event whose period was already invoiced (billing_status
	// "after_close", ADR-120). The held-usage release reads them too, to say
	// which released rows an invoice counts. The invoice store supplies the
	// threshold watermarks.
	usageSvc.SetOpenPeriodReader(usageOpenPeriods{subs: subStore, pricing: pricingStore, thresholds: invoiceStore})
	// subscription.Service uses the resolver at Create, Activate, ChangeItem,
	// ScheduleCancel, PauseCollection, EndTrial and ExtendTrial, so every
	// subscription time the operator writes uses the right clock. Without it,
	// a clock-pinned subscription created today on a clock left idle for a
	// month gets a wall-clock 2026 next_billing_at against a 2026-04
	// frozen_time. Catch-up then never reaches it, as with dunning runs above.
	subSvc.SetResolver(engine)
	// The handler uses the resolver for proration math and changeAt stamps on
	// clock-pinned subscriptions. Without it, remainingPeriodRatio and
	// handleItemProration would use the wall clock even when the subscription
	// is in simulated time.
	subH.SetResolver(engine)
	// The proration denominator (fullBillingCycleDays) is anchored in the
	// tenant's billing timezone, the zone the engine advances period
	// boundaries in (ADR-058). So the cycle length cannot disagree with the
	// period, and it does not depend on the host's time.Local.
	subH.SetTenantLocator(engine)
	// Proration invoices take the tenant's configured Net terms, and the due
	// date derived from them, resolved as for the engine's cycle and create
	// invoices. Otherwise a Net-15 tenant's proration invoice would carry a
	// different due date than its siblings.
	subH.SetNetTermsReader(engine)
	// A proration invoice created already finalized writes the engine's
	// finalize audit row (#1279). audit.MustWired below refuses to boot
	// without it. Its invoice.finalized is queued by the invoice store, on
	// the transaction that inserts it.
	subH.SetInvoiceFinalizeRecorder(engine)
	// With the engine wired, subscription.Service.Create emits the day-1
	// invoice for an in_advance plan (ADR-031). It does nothing for
	// in_arrears, the default. It is best-effort: a billing error is logged
	// and does not fail Create, and the cycle scheduler bills later periods.
	subSvc.SetBiller(engine)
	// The operator's retry-billing bills one subscription through the
	// engine's own attempt (ADR-119).
	subSvc.SetDueBiller(engine)
	// Dunning's final pause and cancel wait for the same grace as the billing
	// runs (ADR-120 amendment 2026-10-22). Operator writes do not wait.
	subSvc.SetInvoiceGrace(o.invoiceGrace)
	// The cancel and plan-swap relief for the unused part of an in_advance
	// period is written in the change's transaction (ADR-031, ADR-118). It is
	// a credit note crediting the balance (paid invoice), a credit note
	// reducing amount_due (unpaid, partly used) or a void (unpaid, unused).
	engine.SetInvoiceVoider(invoiceSvc)
	engine.SetCreditNoteAdjuster(creditNoteSvc)
	engine.SetCreditHeadroomReader(creditNoteSvc)
	// invoice.Service uses the resolver at Create, so one-off composer
	// invoices and manual addenda on a subscription stamp due_at in simulated
	// time for clock-pinned customers and subscriptions. The reminder
	// catch-up compares due_at with frozen_time. Without the resolver, the
	// row never falls inside the catch-up window.
	invoiceSvc.SetResolver(engine)
	// The customer reader lets manual-invoice Create set is_simulated from the
	// customer's test_clock_id. That is the authoritative signal at write
	// time, like the engine's sub.TestClockID for cycle invoices. Without it,
	// manual invoices never carry the simulated badge.
	invoiceSvc.SetCustomerReader(customerStore)
	// The attention classifier picks the notice an invoice shows. A narrow
	// read on the dunning store tells it whether dunning escalated.
	invoiceSvc.SetDunningRunReader(dunningStore)
	// The tax-retry auto-finalize gate looks up pause_collection. A paused
	// subscription's draft stays a draft even once its tax resolves.
	invoiceSvc.SetSubscriptionPauseReader(subStore)
	// Invoice search by customer name or email: the customer store matches
	// after decrypting, since both columns are encrypted at rest.
	invoiceSvc.SetCustomerSearcher(customerStore)
	// The attention classifier reports prepaid-commit exposure: credit already
	// spendable against an invoice nobody paid (ADR-078 D3 fast-follow).
	invoiceSvc.SetCommitGrantReader(creditStore)
	// The attention banner checks for a live hosted payment page. While one
	// is open, Hardcount holds its own charges (ADR-068), so the banner must
	// not offer Retry.
	invoiceSvc.SetPaymentPageReader(invoiceStore)
	// A card the bank refused for good (#966) turns the decline notice into
	// "needs a different card". It is the same rule that makes the dunning
	// retry skip the card.
	invoiceSvc.SetCardRefusalReader(invoiceStore)
	// Card disputes (#979) feed the dispute_lost notice, the timeline's
	// dispute rows and GET /v1/invoices/{id}/disputes.
	invoiceSvc.SetDisputeReader(invoiceStore)
	invoiceSvc.SetCreditApplier(creditSvc)
	// A manual invoice created without net_payment_term_days takes the
	// tenant's configured default, then 30, as the cycle engine does.
	// settingsStore satisfies TenantSettingsReader.
	invoiceSvc.SetTenantSettingsReader(settingsStore)
	// The currency a manual invoice must agree with, or takes when none is
	// sent (ADR-100, ADR-129): billing profile, then live subscription plans.
	invoiceSvc.SetCustomerCurrencyFacts(customerCurrencyFactsAdapter{profiles: customerStore, subs: subStore})
	// Catch-up Phase 0.5 is trial expiry, before Phase 1 cycle billing. It
	// moves subscriptions with status='trialing' to active once simulated
	// time passes trial_end_at. Without it, the status stays 'trialing' for
	// up to about 30 days past the real end of the trial, until calendar
	// billing's stub period closes.
	testClockSvc.SetTrialExpirer(subSvc)
	// Catch-up Phase 1.2 resumes pause_collection, after cycle billing. Each
	// period closes with the pause as it stood at the period's end. Then a
	// pause whose resumes_at has passed clears in the same Advance.
	testClockSvc.SetPauseResumer(subSvc)
	// Catch-up retries tax on clock-pinned invoices (ADR-029 Phase 2). The
	// scheduled retry rightly skips them, by its NOT EXISTS filter on clock
	// pinning. Without this phase no other path would retry them, and a
	// clock-pinned invoice with pending tax would wait until an operator
	// clicked Retry tax on it.
	testClockSvc.SetTaxRetrier(invoiceSvc)
	// Catch-up issues a clock's deferred simulated clawback drafts against
	// frozen_time (ADR-029 Phase 3.7). The wall-clock clawback reconciler
	// skips simulated rows. So without this, a simulated downgrade whose
	// source was in flight would leave its clawback draft stranded.
	testClockSvc.SetClawbackRetrier(creditNoteSvc)
	// Catch-up expires the credit grants of clock-pinned customers against
	// the clock's frozen_time (ADR-029 Phase 4).
	testClockSvc.SetCreditExpirer(creditSvc)
	// Catch-up advances dunning for clock-pinned invoices against the clock's
	// frozen_time, not on the wall-clock 5-min tick (ADR-029 Phase 5).
	testClockSvc.SetDunning(dunningSvc)
	// The no-payment-method notifier runs at finalize when the customer has
	// no PaymentSetup ready (ADR-013). The engine queues the invoice for
	// retry, so a payment method added later collects it automatically, and
	// also emails the customer a payment-update link. Otherwise the customer
	// learns of the missing payment method only when the invoice is overdue
	// weeks later, too late for normal collection. Stripe sends a similar
	// email when a charge fails. Hardcount does the same at finalize, so the
	// customer gets the same experience in both cases.
	//
	// It is always wired. If PAYMENT_UPDATE_URL or PAYMENT_UPDATE_RETURN_URL
	// is empty, the adapter refuses at send time, as the boot warnings above
	// say. There is one send path and no silent skip.
	noPMNotifier := &noPaymentMethodNotifierAdapter{
		email:                  paymentSetupEmail,
		customerEmail:          customerEmailAdapter,
		paymentUpdateURL:       paymentUpdateURL,
		paymentUpdateReturnURL: paymentUpdateReturnURL,
		tokenSvc:               tokenSvc,
		auditLogger:            auditLogger,
	}
	// The adapter's emitter is set in a struct literal, which the
	// SetAuditLogger pin test does not see, so check it here. The adapter
	// writes the engine-driven setup_link_sent row at finalize. The
	// operator-driven row comes from paymentmethods.Handler. The adapter's
	// emitter is nil-guarded, so a dropped wiring would lose the
	// engine-driven row in silence.
	audit.MustWired(noPMNotifier)
	engine.SetNoPaymentMethodNotifier(noPMNotifier)
	// Same adapter backs the operator's "resend payment setup link" action.
	invoiceH.SetNoPaymentMethodNotifier(noPMNotifier)
	// Manual finalize collects through the engine's one collector, the same
	// one the auto-charge sweep runs, so a one-off invoice collects exactly
	// like a cycle invoice.
	invoiceH.SetCollector(engine)
	// No-payment dunning enrollment. The scheduler, and test-clock catch-up,
	// put card-less auto_charge_pending invoices into dunning. So they
	// escalate to a terminal action instead of only being revisited by the
	// sweep.
	engine.SetDunningStarter(&dunningStarterAdapter{dunning: dunningSvc})
	// When an invoice is owed with no card and no Stripe for its mode, the
	// customer is emailed the invoice itself, once, instead of a card-setup
	// link (ADR-060 amendment 2026-10-30). The readiness check is the hosted
	// page's. The email is the operator's "Email invoice", through the same
	// handler body.
	engine.SetStripeReadiness(stripeClients)
	engine.SetInvoiceEmailer(invoiceH.Emailer())
	// A cancel_at strictly inside a billing period fires through the
	// subscription service, in one transaction: the status change, the final
	// invoice and the relief drafts (ADR-097). The engine's scans find a due
	// cancel through their cancel arm. Unwired, a due mid-period cancel would
	// spin the scan forever.
	engine.SetScheduledCancelExecutor(subSvc)
	// A background settle (the credit-cover sweep, a threshold close) that
	// pays an invoice outside the invoice handler closes its active dunning
	// run. dunningSvc.ResolveByInvoice fits the interface directly, with no
	// adapter, and does nothing when there is no active run.
	engine.SetDunningResolver(dunningSvc)

	// Tax: each tenant's provider (none, manual or stripe_tax), and a durable
	// record in tax_calculations. The resolver reads tenant_settings and
	// returns the concrete Provider. The store keeps the request and response
	// payloads, so a tax decision can be rebuilt after Stripe's calculation
	// expires at 24h.
	engine.SetTaxProviderResolver(tax.NewResolver(stripeClients))
	engine.SetTaxCalculationStore(tax.NewPostgresStore(db))
	// The last engine collaborator is wired above. Fail closed now if any is
	// missing, naming every nil field, instead of silently taking a different
	// money path at run time.
	engine.MustValidate()
	// Audit wiring fails closed the same way (ADR-090). Services skip emission
	// on a nil emitter, which keeps test fakes simple. So a forgotten
	// SetAuditLogger line would silently stop auditing a domain. Every
	// component audited in a transaction joins this list in the PR that
	// audits it, and TestMustWired_CoversEveryAuditedComponent pins the list.
	// publicPaymentH is not in the list: its constructor returns nil when
	// Stripe is not configured, so its wiring is guarded above.
	audit.MustWired(creditSvc, subSvc, invoiceSvc, creditNoteSvc,
		stripeAdapter, paymentMethodsSvc, engine, hostedInvoiceStripe,
		customerSvc, pricingSvc, recipeSvc, providerCostH, usageSvc)
	if publicPaymentH != nil {
		// Guarded, because the constructor returns nil without Stripe keys.
		// When the handler exists, its emitter must be wired.
		audit.MustWired(publicPaymentH)
	}
	// Webhook outbox wiring fails closed the same way. A store or service
	// with no outbox queues none of its events, so a dropped setter line would
	// boot and send nothing. Every component that holds an outbox joins this
	// list; TestMustWiredOutbox_CoversEveryOutboxHolder pins it.
	webhook.MustWiredOutbox(invoiceStore, creditStore, subStore, dunningStore, customerStore, paymentMethodsStore, creditNoteSvc)
	// The payment adapter queues no event itself: each payment anomaly's
	// marker and webhook event go through its anomaly recorder, the invoice
	// store. Refuse to boot without one, for the same reason.
	stripeAdapter.MustHaveAnomalyRecorder()

	// Invoice finalize commits the Stripe Tax calculation into a
	// tax_transaction, so the tenant's Stripe Tax reports show the final
	// invoice. The manual and none providers get the call and do nothing.
	invoiceSvc.SetTaxCommitter(engine)

	// Invoice Void reverses the tax_transaction, so the tax authority stops
	// seeing a voided invoice as collected tax. The same engine handles
	// credit-note tax reversals. One code path keeps the Reference uniqueness
	// contract intact.
	invoiceSvc.SetTaxReverser(engine)
	// The credit-note totaler makes void and uncollectible reverse exactly
	// the tax that no credit note covers (total − credited). This stays
	// correct when customer credit was applied to the invoice.
	invoiceSvc.SetCreditNoteTotaler(creditNoteSvc)

	// Void reverses applied customer credits atomically with the status
	// change, in one coordinator transaction. So a brief failure cannot
	// silently strip the credits that paid the invoice.
	invoiceSvc.SetCreditReverser(creditSvc)

	// An operator's retry-tax goes through the billing engine, which owns
	// resolving the provider, recomputing and saving atomically. It backs the
	// "Retry tax" action that the unified Attention shape offers on invoices
	// in tax_status pending.
	invoiceSvc.SetTaxRetrier(engine)

	// The payment-method reader backs the no_payment_method attention branch.
	// It tells the "add a payment method" state, which the operator can act
	// on, apart from the generic awaiting_payment race window.
	// paymentSetupStore, the canonical composite, satisfies
	// PaymentMethodReader.
	invoiceSvc.SetPaymentMethodReader(paymentSetupStore)
	// The Stripe-connected check lets the attention classifier change its
	// text on tax_status='pending' invoices, from "Stripe isn't connected" to
	// "calculation queued, retry now". That covers the gap after the operator
	// connects Stripe and before the scheduler ticks. ADR-019's reconnect
	// flush already retries those invoices. This only stops the banner from
	// misleading the operator in that gap.
	invoiceSvc.SetStripeChecker(stripeClients)

	// Issuing a credit note reverses the invoice's tax_transaction, so the
	// tenant's tax liability drops with the refund. EU VAT, UK VAT and India
	// GST compliance require it. Without it, the credit note refunds the
	// customer but leaves the tenant paying too much tax. The manual and none
	// providers get the call and do nothing.
	creditNoteSvc.SetTaxReverser(engine)

	// The credit-note service resolves "now" through the engine (ADR-030).
	// Its operator entry points (Create, Issue, Void, refund, commit relief)
	// then stamp simulated time on rows owned by a clock-pinned invoice.
	// Otherwise an operator credit note could be dated weeks before the
	// invoice it credits. The engine's own clawback paths are correct without
	// it, because their callers bind the clock.
	creditNoteSvc.SetResolver(engine)

	// Proration invoices use the billing engine's tax resolution, so a plan
	// upgrade is never silently tax-free. The adapter translates between
	// billing.TaxApplication and subscription.ProrationTaxResult.
	subH.SetProrationTaxApplier(&prorationTaxApplierAdapter{engine: engine})

	// The credit for unused prepaid time on a plan downgrade, quantity
	// decrease or item removal is a tax-reversing credit note on each invoice
	// that charged it (ADR-048, ADR-118). It credits the gross amount the
	// customer paid for the unused part, and reverses the matching share of
	// output tax. *creditnote.Service satisfies CreditNoteIssuer directly.
	subH.SetCreditNoteIssuer(creditNoteSvc)

	billingH := billing.NewHandler(engine)
	// create_preview resolves the customer and subscription, then runs the
	// engine's preview path. It is mounted at /v1/invoices/create_preview, as
	// in Stripe, behind PermInvoiceRead, and it writes nothing. It is mounted
	// before /invoices below, so chi picks the more specific pattern.
	// Otherwise /{id} would capture "create_preview".
	createPreviewH := billing.NewCreatePreviewHandler(
		billing.NewPreviewService(engine, customerStore, subStore),
	)

	analyticsH := analytics.NewHandler(db)

	// The email blind index is a keyed hash that finds a customer by email
	// without decrypting. It backs bounce reporting and the
	// recipient-suppression gate. Its HMAC key is separate from
	// HARDCOUNT_ENCRYPTION_KEY, so leaking one does not reveal the other.
	// Without it, both fail closed.
	if bidxKey := strings.TrimSpace(os.Getenv("HARDCOUNT_EMAIL_BIDX_KEY")); bidxKey != "" {
		b, err := crypto.NewBlinder(bidxKey)
		if err != nil {
			slog.Error("invalid HARDCOUNT_EMAIL_BIDX_KEY — bounce-report and suppression lookups will fail closed", "error", err)
		} else {
			customerStore.SetBlinder(b)
			// Wire bounce reporting now that the blinder exists. One adapter
			// serves the synchronous SMTP path (ReportBounce, fire-and-forget)
			// and the Postmark webhook's async path (SuppressBounced and
			// SuppressComplained, which return errors).
			bounceReporter := &bounceReporterAdapter{
				blinder: b,
				store:   customerStore,
				svc:     customerSvc,
			}
			emailSender.SetBounceReporter(bounceReporter)
			postmarkH.SetSuppressor(bounceReporter)
			// The recipient-suppression gate goes on both paths, the direct
			// Sender and the OutboxSender. Once email_status='bounced' is
			// recorded, no further email to that recipient enters the outbox.
			// The same blinder backs both, as for the bounce reporter.
			suppressionChecker := &suppressionCheckerAdapter{blinder: b, store: customerStore}
			emailSender.SetSuppressionChecker(suppressionChecker)
			if emailOutboxSenderRef != nil {
				emailOutboxSenderRef.SetSuppressionChecker(suppressionChecker)
			}
			slog.Info("email blind index enabled for bounce reporting + recipient suppression gate")
		}
	} else {
		slog.Warn("HARDCOUNT_EMAIL_BIDX_KEY not set — bounce reporting + suppression will fail closed (no customers findable by email)")
	}
	// The operator's payment-method handler. Only the handler is wired here.
	// The store and service were built earlier, next to the
	// paymentMethodsStripe adapter, so the billing engine's PaymentReadiness
	// adapter can use the same service.
	paymentMethodsH := paymentmethods.NewHandler(paymentMethodsSvc)
	// The operator's send-setup-email needs a customer lookup, an emailer and
	// an audit logger. The service's own audit (attach, setDefault, detach) is
	// wired on paymentMethodsSvc above. The per-customer cooldown limiter is
	// wired further down, once rdb is constructed (line ~960).
	paymentMethodsH.SetCustomerLookup(&pmCustomerLookupAdapter{store: customerStore})
	paymentMethodsH.SetEmailer(setupLinkEmail)
	paymentMethodsH.SetEmailConfigured(emailSender.IsConfigured())
	paymentMethodsH.SetAuditLogger(auditLogger)

	// setup_intent.succeeded webhooks write payment_methods rows through the
	// service. It is wired here because stripeAdapter (payment/) must not
	// import paymentmethods/. The interface lives in payment/, and its method
	// is AttachForWebhook, so the two AttachFromSetupIntent callers (tests,
	// webhook) keep their own signatures.
	stripeAdapter.SetPaymentMethodAttacher(paymentMethodsSvc)
	// When a SetupIntent carries no hardcount_customer_id metadata, find the
	// Hardcount customer from the SetupIntent's `customer` field, which is
	// authoritative, so the saved card is still stored. The hosted "update
	// payment" and operator add-card Checkout flows do not set
	// setup_intent_data.metadata.
	stripeAdapter.SetCustomerResolver(&stripeCustomerResolverAdapter{customers: customerStore})

	// There is no GDPR data export or erasure. A rebuild must also erase the
	// Stripe Customer and track the acknowledgement window. Build it when the
	// first design partner that targets the EU signs.

	// Dashboard sessions belong to a user (ADR-011). The email and password
	// login creates them as an httpOnly cookie, and the server can revoke
	// them. The session service is a thin wrapper over the dashboard_sessions
	// table.
	sessionSvc := session.NewService(session.NewPostgresStore(db))

	// User accounts: email and password sign-in for the dashboard (ADR-011).
	// The auth handler serves /v1/auth/login, /v1/auth/logout and the
	// password-reset flow.
	userStore := user.NewPostgresStore(db)
	userSvc := user.NewService(userStore, nil)
	// Password-reset emails use the same email path as every other
	// transactional email. The user.EmailSender interface is narrower than
	// email.OutboxSender: at password-reset time there is no tenant or display
	// name, because the operator has not signed in. So an adapter passes an
	// empty tenant and the email address as the name. brandingFor() uses the
	// platform defaults for an empty tenantID, which is fine for an operator
	// email.
	//
	// DASHBOARD_BASE_URL is the dashboard SPA origin for password-reset links.
	// It is optional in single-origin production, where the request Host
	// serves both the API and the SPA, so the bare host works. It is required
	// in split-origin development, where the Vite server (:5173) and the API
	// (:8080) are different origins. Without it there, the reset link points
	// at the API server, which serves no /reset-password route, and the
	// operator gets a 404 from the email link.
	dashboardBaseURL := strings.TrimSpace(os.Getenv("DASHBOARD_BASE_URL"))
	if dashboardBaseURL == "" {
		slog.Warn("DASHBOARD_BASE_URL NOT SET — password-reset emails will NOT be sent (the reset link origin is never derived from request headers, to prevent host-header poisoning / token theft). Set this to your canonical dashboard URL (e.g. http://localhost:5173 in dev) to enable password-reset emails.")
	}
	dashboardAuthH := user.NewHandler(
		userSvc, sessionSvc, session.DefaultCookieConfig(o.appEnv),
		&passwordResetEmailAdapter{sender: passwordResetEmail},
		dashboardBaseURL,
		emailSender.IsConfigured(),
	)
	// Audit the signed-in auth events: login, logout, mode change and password
	// reset. Failed logins go to the structured security log instead, not to
	// the per-tenant audit_log. This is the dashboard (session) auth handler,
	// not authH, the API-key handler wired above.
	dashboardAuthH.SetAuditLogger(auditLogger)

	// The sign-in throttle (ADR-094, amended 2026-10-18) lives only in
	// Postgres, so it works without Redis and every replica shares one count.
	// Its HMAC key is derived from HARDCOUNT_ENCRYPTION_KEY, which production
	// requires and config validates. With no key, in local development, it
	// uses a fixed one.
	throttleKey, err := user.ThrottleKey(strings.TrimSpace(os.Getenv("HARDCOUNT_ENCRYPTION_KEY")))
	if err != nil {
		slog.Error("invalid HARDCOUNT_ENCRYPTION_KEY: the sign-in throttle uses the local development key", "error", err)
		throttleKey, _ = user.ThrottleKey("")
	}
	loginThrottle := user.NewLoginThrottle(db, throttleKey, session.DefaultCookieConfig(o.appEnv))
	dashboardAuthH.SetLoginThrottle(loginThrottle)

	// Team membership: invite, accept and remove. It is minimal, with no RBAC:
	// every member gets the full owner permission set until roles land. Invite
	// emails go through the transactional outbox like every other email.
	// Accept links are built from DASHBOARD_BASE_URL, so they resist
	// host-header poisoning as password-reset links do.
	membersSvc := dashmembers.NewService(
		dashmembers.NewPostgresStore(db),
		memberUserDirectoryAdapter{PostgresStore: userStore},
		sessionSvc,
		outboxSender,
		&memberTenantNamerAdapter{svc: tenantSvc},
		nil, // real clock
		dashboardBaseURL,
	)
	membersSvc.SetEmailConfigured(emailSender.IsConfigured())
	membersH := dashmembers.NewHandler(membersSvc, sessionSvc, session.DefaultCookieConfig(o.appEnv))
	membersH.SetAuditLogger(auditLogger)
	// An accepted invite counts as a sign-in for the throttle.
	membersH.SetSignInRecorder(loginThrottle)

	// Settings → Email: read-only SMTP status, the link env vars and a test
	// send to the signed-in member.
	emailSettingsH := &emailSettingsHandler{
		sender:             emailSender,
		links:              emailLinkEnvs(os.Getenv),
		canEmailSetupLinks: setupLinksEmailable,
		canSendInvites:     membersSvc.CanInvite(),
		userEmail: func(ctx context.Context, userID string) (string, error) {
			u, err := userSvc.GetByID(ctx, userID)
			if err != nil {
				return "", err
			}
			return u.Email, nil
		},
		audit: auditLogger,
	}

	// Handler emitters are checked at boot too. Most handlers take an audit
	// interface (auth.AuditWriter, subscription.auditRecorder,
	// invoice.auditWriter, …), so a nil emitter type-checks. Every handler
	// skips emission when its emitter is nil, including the two that hold a
	// concrete *audit.Logger (customerH, creditNoteH). So without this check,
	// a forgotten SetAuditLogger line here would compile, pass every test and
	// every other gate, and stop auditing a whole domain in silence, while
	// the route registry still declares those routes `explicit`.
	//
	// MustWired finds the audit field of any struct by reflection, so it
	// takes handlers as well as services. This is the second call because
	// paymentMethodsH, dashboardAuthH and membersH are built after the first.
	audit.MustWired(
		customerH, settingsH, invoiceH, subH, creditNoteH,
		authH, dunningH, pricingH, tenantStripeH, webhookOutH,
		testClockH, paymentMethodsH, dashboardAuthH, membersH,
		emailSettingsH,
	)

	s := &Server{
		BillingEngine:    engine,
		DunningSvc:       dunningSvc,
		SettingsStore:    settingsStore,
		WebhookOutSvc:    webhookOutSvc,
		OutboxStore:      outboxStore,
		EmailOutboxStore: emailOutboxStore,
		EmailSender:      emailSender,
		// The invoice store satisfies email.InvoiceSettledChecker directly,
		// with InvoiceTerminalState. The interface is defined by its consumer
		// and wired here, at the composition root.
		EmailSettledChecker: invoiceStore,
		CreditSvc:           creditSvc,
		InvoiceSvc:          invoiceSvc,
		CreditNoteSvc:       creditNoteSvc,
		SubscriptionSvc:     subSvc,
		TokenSvc:            tokenSvc,
		PaymentReconciler:   paymentReconciler,
		LoginThrottle:       loginThrottle,
		TestClockSvc:        testClockSvc,
		RestoreCatchUp:      payment.NewCatchUp(stripeAdapter, stripeClients, tenantStripeSvc, customerStore, creditNoteStore),
	}

	// Redis is for distributed rate limiting. cmd/hardcount/main.go opens and
	// checks the client (mw.OpenRedis). Production refuses to start without a
	// reachable Redis, because without it the general and hosted-invoice
	// limiters fail closed. A replica would otherwise pass its health check
	// and still answer 429 to its share of traffic. config.validateFatal
	// catches an unset Redis, and main.go an unreachable or invalid one. rdb
	// is nil only outside production, where every limiter but /v1/auth's
	// fails open.
	//
	// The limiters below differ in what they do when Redis fails after boot:
	//   - general and hosted-invoice fail closed in production, against DDoS;
	//   - ingest always fails open, because revenue events must survive a
	//     Redis blip;
	//   - /v1/auth counts in this process, because operator login, and its
	//     throttle, must survive a Redis blip.
	prod := o.appEnv == "production"
	// Login brute-force protection has two layers. The outer one is the
	// per-IP /v1/auth limiter below. The inner one is the sign-in throttle
	// (user.LoginThrottle, wired above). It counts per (address × email) and
	// per email, in Postgres, and never locks an account (ADR-094). The
	// users.locked_until lockout stays removed. Its count split between Redis
	// and memory, and anyone who knew an operator's email could use it to
	// lock them out for 15 minutes on demand.
	//
	// The general limit is sized for the dashboard plus an integration, not
	// for sign-in. One dashboard page sends about 10 requests, and every
	// operator of an account shares the account's per-mode bucket. So a
	// limit of 100 a minute would stop one operator after about 10 pages.
	// The default is 1,000 a minute (about 16 a second) per API key, or per
	// account and mode. Stripe's lowest documented global limit is 25 a
	// second, in a sandbox (ADR-095 amendment 2026-10-31).
	rateLimiter := mw.NewRateLimiter(rdb, "general", o.rateLimits.GeneralPerMinute, time.Minute)
	// In production, refuse requests when Redis is unreachable instead of
	// silently turning rate limiting off, which would open a DDoS vector.
	// Outside production the general limiter fails open on purpose.
	// OWASP ASVS and common practice say that generic API rate limiting
	// should fail open when its store blips. The sign-in throttle does not
	// depend on Redis.
	if prod {
		rateLimiter.SetFailClosed(true)
	}
	// Before sign-in, /v1/bootstrap and /v1/auth count per client address on
	// their own limit, HARDCOUNT_RATE_LIMIT_AUTH_PER_MIN (default 100 a
	// minute). So a larger general limit never loosens password guessing.
	// Both use the "auth" Redis namespace with the same rate, so one address
	// shares one bucket across sign-in and bootstrap. They cannot use
	// "general", because that namespace holds a different rate, and two rates
	// on one GCRA key corrupt each other's state.
	//
	// The bootstrap instance fails closed in production, like the general
	// one. The /v1/auth instance below never does.
	bootstrapLimiter := mw.NewRateLimiter(rdb, "auth", o.rateLimits.AuthPerMinute, time.Minute)
	if prod {
		bootstrapLimiter.SetFailClosed(true)
	}
	// /v1/auth gets its own limiter instance. It shares the bootstrap
	// limiter's Redis namespace and limit, so the buckets are shared, but it
	// never fails closed. Locking operators out of login during a Redis blip
	// would be a denial of service. Login still keeps a brute-force floor
	// then, because the sign-in throttle lives in Postgres.
	//
	// It does not fail open either, because that would leave password
	// guessing unlimited for as long as Redis is down. Without Redis (unset,
	// or a call fails) it counts in this process with the same limit (ADR-095
	// amendment 2026-10-18). That count is per replica, so N replicas allow
	// one address up to N times the limit until Redis is back.
	authLimiter := mw.NewRateLimiter(rdb, "auth", o.rateLimits.AuthPerMinute, time.Minute)
	authLimiter.SetLocalFallback()

	// The ingest routes (/v1/usage-events* and the LiteLLM adapter) get their
	// own, much larger bucket. LiteLLM posts one callback per LLM call
	// (ADR-033), so a bucket sized for operator CRUD would run out under real
	// AI traffic. At its stock config LiteLLM retries nothing: max_retries=0,
	// and the queue is cleared on any send error
	// (docs/dev/records/ha-readiness-2026-07-06.md). So every 429 is a
	// silently dropped event that is never billed. For comparison, Stripe
	// meter events allow 1,000 calls/s live, and Orb's test mode alone allows
	// 2,000 events/min. The default is 1000 req/s per key.
	// HARDCOUNT_RATE_LIMIT_INGEST_PER_SEC overrides it.
	//
	// It never fails closed, on purpose, even in production. The routes are
	// authenticated (a secret key is required), and failing closed would turn
	// a Redis blip into lost revenue data. For ingest, the risk of lost
	// events outweighs the DoS argument that closes the general bucket.
	ingestLimiter := mw.NewRateLimiter(rdb, "ingest", o.rateLimits.IngestPerSecond, time.Second)

	// The public hosted-invoice routes get a tighter bucket than the general
	// API, following industry practice for payment pages (Stripe, Paddle).
	// The reasons are:
	//   - the URL can be shared publicly, so it is easier to abuse;
	//   - each Pay click creates a Stripe Checkout Session, which has its own
	//     rate limit at Stripe;
	//   - real customer traffic from one NAT is small, about 5-10 requests a
	//     visit.
	// 60/min per IP leaves room for about 10 customers at once behind one
	// corporate NAT, and still bounds enumeration and scraping.
	hostedInvoiceRL := mw.NewRateLimiter(rdb, "hosted_invoice", 60, time.Minute)
	if prod {
		hostedInvoiceRL.SetFailClosed(true)
	}

	// The operator's send-setup-email has a per-customer cooldown: 1 send per
	// (tenant, customer) per 60s. It guards against a double click and the
	// refresh-then-click race. It always fails open, even in production, so a
	// Redis hiccup does not block a real "send link" action. The worst case,
	// Redis down and a real double click, is two near-identical emails, with
	// no money or state at risk.
	setupLinkCooldown := mw.NewRateLimiter(rdb, "setup_link", 1, time.Minute)
	paymentMethodsH.SetCooldown(setupLinkCooldown)

	r := chi.NewRouter()
	// An unknown path and a method the path does not take answer the JSON
	// error envelope, not chi's plain text. chi hands both handlers down to
	// every router mounted below. The 405 lists the path's methods in Allow.
	var allow *allowIndex // built once every route is registered, below
	r.NotFound(respond.RouteNotFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		respond.MethodNotAllowed(w, req, allow.methods(req.URL))
	})
	useRequestEdge(r, os.Getenv("TRUST_PROXY"))
	corsEnv := os.Getenv("CORS_ALLOWED_ORIGINS")
	if corsEnv == "" {
		corsEnv = "http://localhost:3000,http://localhost:5173,http://localhost:5174"
	}
	// Parse the origins once, for both consumers (CORS and the CSRF origin
	// allowlist), with the parser behind the boot check. Both match entries
	// exactly, so entries are trimmed ("a, b" must allow "b") and blank ones
	// are dropped (a blank entry would match a request with no Origin). The
	// error is ignored. config.Load already refused to start on it, and the
	// list returned with it holds only the usable entries.
	allowedOrigins, _ := config.ParseCORSOrigins(corsEnv)
	r.Use(mw.CORS(allowedOrigins))
	r.Use(mw.Metrics())
	r.Use(requestLogger)
	r.Use(mw.Recoverer)

	// Cap every request body (limitRequestBody): 1 MB, or the LiteLLM
	// adapter's own larger cap.
	r.Use(limitRequestBody)

	r.Use(mw.SecurityHeaders(o.appEnv))

	// The audit-coverage detector (ADR-090) only observes. It reports any
	// mutating 2xx that wrote no audit row and is not declared exempt in the
	// route-audit registry (audit_routes.go), with slog.Error and the
	// hardcount_audit_uncovered_mutation_total counter. It never touches the
	// response and never buffers the body.
	//
	// It is mounted at the root, not on /v1, so it also sees /v1/auth,
	// /v1/public/*, /v1/webhooks and /v1/bootstrap (ADR-090 RC1). It sits
	// after RequestID and TrustedClientIP, so the ctx it seeds for every later
	// audit emission carries the request id and the real client IP.
	r.Use(mw.AuditCoverage(auditRouteExempt))

	// There is no global request timeout. The SSE stream needs a connection
	// with no time limit, because clients tail webhook events for hours. So
	// each authenticated route block sets its own timeout, and the SSE block
	// sets none. The unauthenticated public routes (health, metrics,
	// auth/login) need no 30s cap, because their handlers finish in
	// milliseconds or fail fast on their own.

	// Public
	r.Get("/health", handleHealth)
	r.Get("/health/ready", handleDeepHealth(db))
	r.Handle("/metrics", mw.MetricsAuth(o.appEnv, mw.MetricsHandler()))

	// Bootstrap is the one-time setup: no auth, and it works only while no
	// tenant exists. The deps let tenant.RunBootstrap create the owner user
	// through the peer user package, in the same transaction (ADR-073).
	//
	// bootstrapDeps is a named variable, not an inline literal, so the boot
	// gate below can see its Audit field. It is wired by struct literal, so
	// the pin test, which greps for SetAuditLogger receivers, misses it, and
	// MustWired sees only what it is handed. Bootstrap mints a live secret
	// key and the owner account, its route is `explicit` in the registry, and
	// deps.Audit is nil-guarded (bootstrap_run.go). So a dropped wiring would
	// silently stop auditing the creation of a credential that moves money.
	bootstrapDeps := tenant.BootstrapDeps{
		HashPassword: user.HashPassword,
		CreateUserTx: user.NewPostgresStore(db).CreateInTx,
		// Every new account starts with a default dunning policy per mode
		// (ADR-123). RunBootstrap refuses to run when this is nil.
		SeedDunningTx: dunningSvc.SeedDefaultPoliciesTx,
		Audit:         auditLogger,
	}
	audit.MustWired(&bootstrapDeps)
	bootstrapH := tenant.NewBootstrapHandler(db, bootstrapDeps)
	r.Group(func(r chi.Router) {
		r.Use(bootstrapLimiter.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		r.Mount("/v1/bootstrap", bootstrapH.Routes())
	})

	// Dashboard auth: email and password login, logout and the password-reset
	// flow (ADR-011). These routes are public, used before a session exists,
	// and rate limited to slow credential stuffing.
	r.Route("/v1/auth", func(r chi.Router) {
		// CSRF: these routes serve only the browser dashboard, and they create
		// the session cookie. So a cross-site POST here is the login-fixation
		// vector. Reject cross-site browser requests before login can set a
		// cookie. There is no bearer path here, so every unsafe request has its
		// origin checked.
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(authLimiter.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		// The invite accept flow is public like login and reset, because
		// the token is the credential. It shares their limiter, so guessing
		// invite tokens is throttled like credential stuffing. Explicit
		// routes win over the Mount("/") wildcard below.
		membersH.RegisterPublicRoutes(r)
		r.Mount("/", dashboardAuthH.Routes())
	})

	// Stripe webhooks need no API key, because their signature verifies them.
	r.Group(func(r chi.Router) {
		r.Use(mw.Timeout(30 * time.Second))
		r.Mount("/v1/webhooks", webhookH.Routes())
		// Postmark email webhooks need no API key either. Basic auth gates
		// them, because Postmark signs nothing. The static route wins over the
		// Mount wildcard above.
		r.Post("/v1/webhooks/postmark", postmarkH.HandleWebhook)
	})

	// Public payment update: no auth, because the token validates the request.
	// It uses the hosted-invoice routes' tight per-IP bucket, because both are
	// unauthenticated payment endpoints. Without a limit, anyone could guess
	// tokens for free and multiply calls to the Stripe API.
	if publicPaymentH != nil {
		r.Group(func(r chi.Router) {
			r.Use(hostedInvoiceRL.Middleware())
			r.Use(mw.Timeout(30 * time.Second))
			r.Mount("/v1/public/payment-updates", publicPaymentH.Routes())
		})
	}

	// Public hosted invoice, the equivalent of Stripe's hosted_invoice_url. It
	// is unauthenticated: the 256-bit public_token in the URL is the only
	// credential. It has its own rate-limit bucket (60/min per IP), because
	// payment pages need tighter limits than the general API (see
	// hostedInvoiceRL above).
	r.Route("/v1/public/invoices", func(r chi.Router) {
		r.Use(hostedInvoiceRL.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		r.Mount("/", hostedInvoiceH.Routes())
	})

	// Public cost dashboard (ADR-031). It is unauthenticated: the 256-bit
	// cost_dashboard_token in the path is the credential. Its rate limit
	// (60/min per IP) is tighter than the general API's, because the embed
	// widget may poll, and abuse from one site must not exhaust shared
	// buckets. The reason and the limit are the same as for the hosted
	// invoice page.
	r.Route("/v1/public/cost-dashboard", func(r chi.Router) {
		r.Use(hostedInvoiceRL.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		r.Mount("/", customerH.PublicCostDashboardRoutes())
	})

	// The real-time webhook event stream (SSE) is mounted above the /v1 route
	// block, so it skips the global 30s middleware.Timeout, which would cut
	// any long-lived EventSource connection at 30s. It gets the same auth
	// chain as /v1/* (session cookie or Bearer API key, rate limit, permission
	// check), but not idempotency, which adds nothing to a streaming GET, and
	// not the Timeout. The stream is GET-only and read-only: it has no body to
	// key for idempotency, and nothing to audit beyond the access log.
	r.Route("/v1/webhook_events/stream", func(r chi.Router) {
		r.Use(session.MiddlewareOrAPIKey(sessionSvc, authSvc))
		r.Use(rateLimiter.Middleware())
		r.With(auth.Require(auth.PermAPIKeyRead)).Get("/", webhookOutH.StreamHandler())
	})

	// Streaming CSV exports are mounted beside the /v1 block, like the SSE
	// stream above, so they get a five-minute timeout instead of the 30s CRUD
	// cap. A full usage-events export can stream for longer than 30s, and a
	// 30s cap would cut it off mid-file (see exportAbort). They get the same
	// auth chain as /v1, without idempotency, since they are GET-only. Each
	// endpoint requires the matching *Read permission.
	//
	// Nothing between the handler and the socket buffers the response.
	// exports.go streams through http.Flusher, and a buffering wrapper would
	// hide the Flusher and hold the whole CSV in memory instead (ADR-090).
	//
	// Every export writes an action=export audit row before it streams, and
	// refuses to stream if that row cannot be written (ADR-090 §7, read
	// egress). A tamper-evidence system that cannot show who copied the
	// evidence has a hole in its chain of custody. audit-log.csv is here too:
	// the compliance export is itself an audited event, and it uses the same
	// permission as the audit-log read route below.
	r.Route("/v1/exports", func(r chi.Router) {
		r.Use(session.MiddlewareOrAPIKey(sessionSvc, authSvc))
		r.Use(rateLimiter.Middleware())
		// The handler moves each response's write deadline to match this
		// timeout (extendExportWriteDeadline), because the server's
		// WriteTimeout is 35 s.
		r.Use(mw.Timeout(exportTimeout))
		exportsH := newExportsHandler(customerStore, invoiceStore, subStore, usageStore, auditLogger, auditLogger)
		r.Mount("/", exportsH.Routes(
			auth.Require(auth.PermCustomerRead),
			auth.Require(auth.PermInvoiceRead),
			auth.Require(auth.PermSubscriptionRead),
			auth.Require(auth.PermUsageRead),
			auth.Require(auth.PermAPIKeyRead),
		))
	})

	// Tenant-scoped routes accept an httpOnly session cookie (the dashboard,
	// created by /v1/auth/login) or `Authorization: Bearer <api_key>` (SDKs,
	// curl, server-to-server). The cookie wins when both are present, so a
	// stale Bearer header cannot bypass session revocation. Both paths set
	// the same auth ctx keys, so handlers do not branch.
	r.Route("/v1", func(r chi.Router) {
		// CSRF: this group serves both the cookie dashboard and bearer SDK
		// and API callers. The guard checks the origin of unsafe requests on
		// the cookie path, and exempts bearer requests: a credential that the
		// browser does not attach on its own cannot be used for CSRF. It runs
		// before auth, so a cross-site browser write is refused before it
		// touches session state. Safe methods and the SSE and export GET
		// siblings are unaffected.
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(session.MiddlewareOrAPIKey(sessionSvc, authSvc))
		// After auth, so the tenant ID is available for the bucket key. Ingest
		// paths use their own, much larger bucket (see ingestLimiter).
		r.Use(mw.SplitRateLimit(isIngestPath, ingestLimiter, rateLimiter))
		// Ingest routes (isIngestPath) ignore the Idempotency-Key header.
		// There, each event's own idempotency_key is the one dedup rule.
		r.Use(mw.Idempotency(db, isIngestPath))
		// No audit middleware here. The code that owns a write emits its
		// audit row inside its transaction (audit.Logger.LogInTx). Every
		// mutating route declares in the route-audit registry
		// (audit_routes.go) where its evidence comes from. The root-mounted
		// mw.AuditCoverage detector reports any route that stops emitting.
		//
		// A per-request timeout for ordinary CRUD endpoints. The SSE stream
		// is mounted above this block, on the sibling
		// /v1/webhook_events/stream route, so it does not inherit the 30s
		// cap. Long-lived EventSource connections must run with no limit.
		r.Use(mw.Timeout(30 * time.Second))

		// /v1/whoami resolves the active credential to its tenant context,
		// with no specific permission required. The cookie path returns
		// user_id and email (dashboard). The Bearer path returns key_id and
		// key_type (SDK, curl). Both always return tenant_id and livemode.
		// The dashboard's AuthContext needs user_id on the cookie path:
		// without it, refresh() silently signs the operator out.
		//
		// can_email_setup_links is the server's own setting (SMTP and
		// PAYMENT_UPDATE_URL). The invoice page offers "Email a
		// payment-method link" on a written-off invoice only when it is
		// true, because that invoice carries no attention notice to decide
		// it on the server.
		r.Get("/whoami", func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			live, err := postgres.Livemode(ctx)
			if err != nil {
				respond.FromError(w, req, err, "whoami")
				return
			}
			out := map[string]any{
				"tenant_id":             auth.TenantID(ctx),
				"livemode":              live,
				"can_email_setup_links": setupLinksEmailable,
				"can_send_invites":      membersSvc.CanInvite(),
				// smtp_configured lets any page show one global "email is
				// off" notice instead of repeating it on every record.
				"smtp_configured": emailSender.IsConfigured(),
			}
			if uid := auth.UserID(ctx); uid != "" {
				out["user_id"] = uid
				if u, err := userSvc.GetByID(ctx, uid); err == nil {
					out["email"] = u.Email
				}
			}
			if kid := auth.KeyID(ctx); kid != "" {
				out["key_id"] = kid
				out["key_type"] = string(auth.GetKeyType(ctx))
			}
			respond.JSON(w, req, http.StatusOK, out)
		})

		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/api-keys", authH.Routes())
		// Team membership is for dashboard sessions only. An API key is a
		// machine credential with no user to attribute an invite or removal
		// to, so it gets 403 here. The service checks again on invite.
		r.With(auth.RequireSession("manage team membership")).Mount("/members", membersH.Routes())
		// The /customers subtree mixes reads (GET list, get, billing-profile)
		// and writes (POST create, PATCH update, PUT billing-profile, GDPR
		// delete-data). RequireMethod picks the permission by HTTP method,
		// so a read-tier key can list and get but cannot create, update or
		// delete.
		r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers", customerH.Routes())
		// The customer-scoped usage view combines usage totals with customer,
		// subscription and pricing reads. It needs only PermUsageRead, so the
		// dashboard's read-only secret-tier key can call it without gaining
		// customer write access.
		r.Mount("/customers/{id}/usage", customerUsageH.CustomerUsageRoutes(
			auth.Require(auth.PermUsageRead),
		))
		// The per-customer margin report (ADR-079) compares rated usage
		// revenue with the stamped provider costs (COGS, cost of goods
		// sold). Only operators can call it: COGS never shows on a
		// customer-facing page, the hosted cost dashboard included.
		r.With(auth.Require(auth.PermUsageRead)).Get("/customers/{id}/margin", providerCostH.Margin)
		// Provider cost rates CRUD (ADR-079): the operator's COGS table.
		r.With(auth.RequireMethod(auth.PermUsageRead, auth.PermUsageWrite)).Mount("/provider-costs", providerCostH.Routes())
		// The operator's payment-method management. It uses the same Service
		// as the /v1/me/payment-methods customer-facing routes. Only the auth
		// model and the source of customer_id differ. It offers list,
		// setDefault and detach, plus "send setup link", which mints a
		// Stripe Checkout setup URL the operator can give the customer. Card
		// capture stays in the Stripe-hosted iframe, so the operator's PCI
		// scope stays SAQ-A. Chargebee, Lago and Orb offer the same shape.
		r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers/{customer_id}/payment-methods", paymentMethodsH.OperatorRoutes())
		r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/meters", pricingH.MeterRoutes())
		// A plan and its prices (/v1/plans/{id}/prices) are one subtree.
		r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/plans", pricingH.PlanRoutes())
		r.With(auth.Require(auth.PermPricingWrite)).Mount("/recipes", recipeH.Routes())
		r.With(auth.RequireMethod(auth.PermSubscriptionRead, auth.PermSubscriptionWrite)).Mount("/subscriptions", subH.Routes())
		// Backfill is mounted before the /usage-events subtree, so chi picks
		// the more specific pattern. PermUsageWrite limits it to secret-tier
		// keys, so an ingest key cannot backfill (ADR-133).
		r.With(auth.Require(auth.PermUsageWrite)).Post("/usage-events/backfill", usageH.Backfill)
		// Sending events needs PermUsageIngest, the one permission an ingest
		// key holds (ADR-133). The reads, and the held-usage release and
		// discard, need usage read and write.
		r.Mount("/usage-events", usageH.Routes(
			auth.Require(auth.PermUsageIngest),
			auth.Require(auth.PermUsageRead),
			auth.Require(auth.PermUsageWrite),
		))
		// The LiteLLM adapter: POST /v1/integrations/litellm/spend. The
		// operator's LiteLLM proxy sends the Bearer key on every callback,
		// so it needs only PermUsageIngest, which an ingest key (ADR-133) or
		// a secret key holds. See ADR-033 and docs/integrations/litellm.md.
		r.With(auth.Require(auth.PermUsageIngest)).Mount("/integrations/litellm",
			litellm.New(customerStore, pricingSvc, usageSvc, usageSvc).Routes())
		// create_preview must be mounted before /invoices, because chi tries
		// patterns in registration order. Once /invoices is mounted with its
		// /{id}/... children, "create_preview" would be taken as an invoice
		// ID. See docs/dev/design/create-preview.md.
		r.With(auth.Require(auth.PermInvoiceRead)).Mount("/invoices/create_preview", createPreviewH.Routes())
		r.With(auth.RequireMethod(auth.PermInvoiceRead, auth.PermInvoiceWrite)).Mount("/invoices", invoiceH.Routes())
		r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/credit-notes", creditNoteH.Routes())
		r.With(auth.Require(auth.PermPricingWrite)).Mount("/price-overrides", pricingH.OverrideRoutes())
		r.With(auth.Require(auth.PermCustomerWrite)).Mount("/credits", creditH.Routes())
		r.With(auth.RequireMethod(auth.PermDunningRead, auth.PermDunningWrite)).Mount("/dunning", dunningH.Routes())
		r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/billing", billingH.Routes())
		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/webhook-endpoints", webhookOutH.Routes())
		// The real-time event UI's replay and deliveries timeline live here.
		// The SSE stream itself is mounted above /v1, because chi's 30s
		// middleware.Timeout would kill long-lived streams. The path uses an
		// underscore (webhook_events) on purpose, to match Stripe's
		// /v1/events convention and not collide with the
		// /webhook-endpoints/events list.
		//
		// PermAPIKeyRead is enough for both the read (deliveries) and the
		// replay. Replay changes state, but it only re-issues an event that
		// was already emitted.
		r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/webhook_events", webhookOutH.EventRoutes())
		r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/audit-log", auditH.Routes())
		// Test and live mode share the account settings, and a key belongs
		// to one mode, so a key may read them but only the dashboard changes
		// them.
		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings", settingsH.Routes(auth.RequireSession("change account settings")))
		// Each mode has its own Stripe connection, and a caller manages only
		// its own mode's: a key the mode it was made in, a dashboard session
		// the mode it is in. tenantstripe.Service enforces this, not a
		// middleware, because the mode comes from the body or the path.
		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings/stripe", tenantStripeH.Routes())
		r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings/email", emailSettingsH.Routes())
		r.With(auth.Require(auth.PermInvoiceRead)).Mount("/analytics", analyticsH.Routes())
		r.With(auth.Require(auth.PermTestClockWrite)).Mount("/test-clocks", testClockH.Routes())

		// Customer Detail: the dashboard's composed per-customer views, in the
		// same /customers/{...} namespace as usage, margin and
		// payment-methods. They are not under /customer-portal, so that name
		// means only the removed self-serve portal (ADR-051).
		customerDetail := newCustomerDetailHandler(customerStore, subStore, invoiceStore)
		r.With(auth.Require(auth.PermCustomerRead)).Get("/customers/{customer_id}/overview", customerDetail.overview)
		// A card is added through one of two Stripe Checkout setup sessions.
		// The operator's goes through paymentmethods.Service.CreateSetupSession:
		// POST /v1/customers/{id}/payment-methods/send-setup-email (email) or
		// POST /v1/customers/{id}/payment-methods/setup-session (copy-link).
		// The customer's comes from the payment-update email link:
		// POST /v1/public/payment-updates/{token}/checkout.
	})

	allow = newAllowIndex(r)

	s.router = r
	return s
}

// ServeHTTP serves the request through the router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func handleDeepHealth(db *postgres.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		// The status code and the body answer different questions. The code
		// is for the load balancer: 503 only when this replica cannot serve
		// requests (draining, or the database is unreachable). The body also
		// reports the billing tick, which a balancer cannot fix by routing
		// elsewhere.
		overallStatus := "ok"
		status := http.StatusOK
		checks := map[string]string{"api": "ok"}

		// Draining: the replica is shutting down. Readiness fails first, so
		// the balancer stops sending traffic here before the listener closes
		// (cmd/hardcount/main.go).
		if draining.Load() {
			checks["api"] = "draining"
			respond.JSON(w, r, http.StatusServiceUnavailable, map[string]any{
				"status": "draining",
				"checks": checks,
			})
			return
		}

		// Database check. This endpoint is unauthenticated and the driver's
		// error names the database user, database and host, so the body gets
		// a fixed word and the detail goes to the log.
		if err := db.Pool.PingContext(ctx); err != nil {
			slog.ErrorContext(ctx, "readiness: database ping failed", "error", err)
			checks["database"] = "error"
			overallStatus = "degraded"
			status = http.StatusServiceUnavailable
		} else {
			checks["database"] = "ok"
		}

		// Scheduler check: degraded when no run is recorded within twice the
		// interval. Before the first run completes, the scheduler counts as
		// ok, because it has just started. This is reported in the body only.
		// A stale tick leaves the status code at 200, because pulling this
		// replica from the balancer does not restart the tick, and on a
		// single replica it would take the API down too. Alert instead on
		// hardcount_scheduler_last_run_timestamp_seconds and
		// hardcount_leader_last_tick_age_seconds
		// (docs/ops/runbook/monitoring.md).
		schedulerMu.RLock()
		lastRun := schedulerLastRun
		interval := schedulerInterval
		schedulerMu.RUnlock()

		if interval > 0 && !lastRun.IsZero() {
			sinceLastRun := time.Since(lastRun)
			if sinceLastRun > 2*interval {
				checks["scheduler"] = fmt.Sprintf("degraded: last run %s ago (interval %s)", sinceLastRun.Truncate(time.Second), interval)
				overallStatus = "degraded"
			} else {
				checks["scheduler"] = fmt.Sprintf("ok: last run %s ago", sinceLastRun.Truncate(time.Second))
			}
		} else if interval > 0 && lastRun.IsZero() {
			checks["scheduler"] = "ok: awaiting first run"
		} else {
			checks["scheduler"] = "ok: not configured"
		}

		respond.JSON(w, r, status, map[string]any{
			"status": overallStatus,
			"checks": checks,
		})
	}
}

// requestLogger writes the access log. Each line says who made the request
// and which tenant it touched, not only the method, path, status and
// duration. So after an incident the log can answer "what did this actor
// do" and "which tenant was affected". The request_id on each line joins
// it to the audit row that the same request wrote.
//
// The identity fields come from audit.ResolveActor, the resolver the audit
// rows use, so a request cannot be one actor in the log and another in the
// evidence.
//
// Only the actor id is logged, not the name. A user actor's name is an
// email, and logs go to sinks that cannot erase data. For the same reason,
// the append-only audit_log stores no addresses either.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		// Read the identity after the handler. Auth runs after this
		// middleware, so before ServeHTTP the context is still anonymous.
		ctx := r.Context()
		actorType, actorID := audit.ResolveActor(ctx)

		attrs := []any{
			"method", r.Method,
			"path", loggablePath(r),
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"actor_type", actorType,
			// Never empty. ResolveActor answers "system"/"system" for an
			// unauthenticated request. That is a real answer, not a missing
			// one, and the audit row would record the same.
			"actor_id", actorID,
			// Joins this line to the audit row the same request wrote.
			"request_id", middleware.GetReqID(ctx),
			"ip", audit.ClientIP(ctx),
		}
		// Omit the field rather than log it empty. An unauthenticated request
		// has no tenant, and a blank field reads as "we lost it" instead of
		// "there wasn't one".
		if tenantID := auth.TenantID(ctx); tenantID != "" {
			attrs = append(attrs, "tenant_id", tenantID)
		}
		slog.Info("request", attrs...)
	})
}

// useRequestEdge mounts the first middlewares every request passes: the
// trace span, the server-minted request id, the trusted client IP and the
// span correction. Anything after it may answer without routing, as a CORS
// preflight does, so the span correction must sit here.
func useRequestEdge(r chi.Router, trustProxy string) {
	r.Use(mw.Tracing())
	// Use mw.RequestID, not chi's middleware.RequestID. chi's honours an
	// inbound X-Request-Id header as sent, which would let a caller choose the
	// request_id recorded on their own audit_log rows. mw.RequestID always
	// mints the id on the server. See its doc comment and ADR-090 §7 (read
	// egress).
	r.Use(mw.RequestID)
	// The client IP comes from chi's ClientIPFrom* middleware. X-Forwarded-For
	// is read only when the TCP peer is inside TRUST_PROXY, a comma-separated
	// list of CIDRs or IPs such as "10.0.0.0/8,127.0.0.1". Otherwise, and when
	// TRUST_PROXY is unset or "none", the client IP is the TCP peer. Rate
	// limits, audit and session rows, logs and spans all read it through
	// audit.ExtractClientIP.
	trustedProxies := mw.ParseTrustedProxies(trustProxy)
	switch {
	case strings.EqualFold(strings.TrimSpace(trustProxy), "none"):
		slog.Info("TRUST_PROXY=none — no proxy in front; rate limiting keys on the direct TCP peer")
	case len(trustedProxies) == 0:
		// Production refuses to start in this state (config.validateFatal).
		// Local and staging get the warning.
		slog.Warn("TRUST_PROXY not set — X-Forwarded-For ignored; rate limiting keys on the direct TCP peer. Behind a balancer that is ONE bucket for everyone: set TRUST_PROXY to the balancer's CIDR, or TRUST_PROXY=none if there is no proxy.")
	}
	r.Use(mw.TrustedClientIP(trustedProxies))
	r.Use(mw.TraceRequestAttrs(loggablePath))
}

// loggablePath returns the request path with capability tokens removed.
// Four routes carry a secret in the path that works like a bearer token:
// hosted invoices, payment updates, the public cost dashboard and the team
// invite preview. Logging the raw path would write live credentials into
// every log sink. It prefers chi's matched route pattern
// ("/v1/public/invoices/{token}"). When no pattern matched (a 404, or a
// CORS preflight answered before routing), it redacts the segment after
// any known token prefix instead, because an unmatched request on those
// prefixes may still hold a secret. A new {token} route adds its prefix
// here, and to deploy/compose/nginx-log-redact.conf, which keeps it out of
// the Compose stack's nginx access logs.
func loggablePath(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pat := rctx.RoutePattern(); pat != "" {
			return pat
		}
	}
	path := r.URL.Path
	for _, prefix := range []string{
		"/v1/public/invoices/",
		"/v1/public/payment-updates/",
		"/v1/public/cost-dashboard/",
		"/v1/auth/invite/",
	} {
		if rest, ok := strings.CutPrefix(path, prefix); ok && rest != "" {
			// Keep any sub-path (e.g. /pdf, /checkout) after the token.
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return prefix + "[redacted]" + rest[i:]
			}
			return prefix + "[redacted]"
		}
	}
	return path
}

// maxRequestBodyBytes caps a request body on every route but the LiteLLM
// adapter, against oversized payloads (DoS).
const maxRequestBodyBytes = 1 << 20 // 1 MB

// limitRequestBody caps the request body: 1 MB, except on the LiteLLM
// adapter, whose batches of full call records run to several MB
// (litellm.MaxBodyBytes says why and how much). It is the one place every
// route's cap is set, so no route goes uncapped. The adapter is matched on
// the path prefix it is mounted at, as isIngestPath matches it.
func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxRequestBodyBytes)
		if strings.HasPrefix(r.URL.Path, litellmPathPrefix) {
			limit = litellm.MaxBodyBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// litellmPathPrefix is where the LiteLLM adapter is mounted.
const litellmPathPrefix = "/v1/integrations/litellm"

// isIngestPath reports whether r is a usage route: the usage-events subtree
// (single, batch, backfill, held release and discard, plus the reads, which
// do no harm there) or the LiteLLM adapter. These routes use the dedicated
// ingest rate-limit bucket, because one callback per LLM call must never be
// squeezed through the operator-CRUD bucket. They ignore the
// Idempotency-Key header: each event's idempotency_key, or the held row
// moving only once, is their dedup.
func isIngestPath(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v1/usage-events") ||
		strings.HasPrefix(r.URL.Path, litellmPathPrefix)
}

// routeMethods are the methods the Allow header can list, in its order.
var routeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// allowIndex answers which methods a path takes, for the Allow header of a
// 405. chi's own Find cannot answer it through a mount, because a mount
// point matches every method. So every route the router serves, as
// chi.Walk lists it, is registered on one flat router with no mounts.
type allowIndex struct {
	flat *chi.Mux
}

var pathParamName = regexp.MustCompile(`\{[^}]*\}`)

func newAllowIndex(router chi.Routes) *allowIndex {
	flat := chi.NewMux()
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	// Walk returns only the walk function's error, and this one has none.
	_ = chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// Parameters are renamed by position: two routers may name the
		// same segment differently ({id}, {customer_id}), which one chi
		// tree refuses.
		n := 0
		pattern := pathParamName.ReplaceAllStringFunc(canonicalRoute(route), func(string) string {
			n++
			return fmt.Sprintf("{p%d}", n)
		})
		flat.Method(method, pattern, noop)
		if pattern != "/" && !strings.HasSuffix(pattern, "*") {
			flat.Method(method, pattern+"/", noop)
		}
		return nil
	})
	return &allowIndex{flat: flat}
}

// methods lists the methods the router serves at u's path.
func (a *allowIndex) methods(u *url.URL) []string {
	if a == nil {
		return nil
	}
	path := u.RawPath
	if path == "" {
		path = u.Path
	}
	var out []string
	for _, m := range routeMethods {
		if a.flat.Match(chi.NewRouteContext(), m, path) {
			out = append(out, m)
		}
	}
	return out
}
