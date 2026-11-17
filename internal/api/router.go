package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
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
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/recipe"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/user"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Server is the HTTP API. Its exported fields hold the services and stores
// that cmd/hardcount wires into the background workers.
type Server struct {
	router chi.Router

	// What the routes read; wire sets them.
	db                  *postgres.DB
	clk                 clock.Clock
	invoiceStore        *invoice.PostgresStore
	subStore            *subscription.PostgresStore
	usageStore          *usage.PostgresStore
	customerStore       *customer.PostgresStore
	tenantStripeH       *tenantstripe.Handler
	pricingSvc          *pricing.Service
	customerH           *customer.Handler
	pricingH            *pricing.Handler
	subH                *subscription.Handler
	usageSvc            *usage.Service
	usageH              *usage.Handler
	customerUsageH      *usage.CustomerUsageHandler
	providerCostH       *usage.ProviderCostHandler
	creditH             *credit.Handler
	creditNoteStore     *creditnote.PostgresStore
	creditNoteH         *creditnote.Handler
	webhookOutH         *webhook.Handler
	auditLogger         *audit.Logger
	auditH              *audit.Handler
	authSvc             *auth.Service
	authH               *auth.Handler
	settingsH           *tenant.SettingsHandler
	dunningStore        *dunning.PostgresStore
	dunningH            *dunning.Handler
	recipeH             *recipe.Handler
	webhookH            *payment.Handler
	invoiceH            *invoice.Handler
	publicPaymentH      *payment.PublicPaymentHandler
	hostedInvoiceH      *hostedinvoice.Handler
	postmarkH           *email.PostmarkHandler
	setupLinksEmailable bool
	testClockH          *testclock.Handler
	billingH            *billing.Handler
	createPreviewH      *billing.CreatePreviewHandler
	analyticsH          *analytics.Handler
	paymentMethodsH     *paymentmethods.Handler
	sessionSvc          *session.Service
	userSvc             *user.Service
	dashboardAuthH      *user.Handler
	membersSvc          *dashmembers.Service
	membersH            *dashmembers.Handler
	emailSettingsH      *emailSettingsHandler
	rateLimiter         *mw.RateLimiter
	bootstrapLimiter    *mw.RateLimiter
	authLimiter         *mw.RateLimiter
	ingestLimiter       *mw.RateLimiter
	hostedInvoiceRL     *mw.RateLimiter
	opts                serverOptions
	settings            config.Server

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

// NewServer builds every store, service and handler, wires them together
// and mounts the routes. A nil clk means the real clock. It panics when a
// required collaborator or audit emitter is not wired, or when the
// built-in recipes fail to load, so a broken build or wiring stops boot.
func NewServer(db *postgres.DB, clk clock.Clock, rdb *redis.Client, opts ...ServerOption) *Server {
	s := wire(db, clk, rdb, opts...)
	s.router = s.routes()
	return s
}

// routes registers every route and its middleware, and returns the router.
func (s *Server) routes() chi.Router {
	r := chi.NewRouter()
	// Finds each request's route for its record (request.Begin).
	finder := &routeFinder{}
	// The clock door (clockDoor) runs last in every group, after
	// authentication, the limiter and the timeout, so it reads a named record
	// as the caller's tenant and mode, a replay costs no lookup, and its
	// lookups are bounded. A route's own permission check runs after it, so a
	// caller without the permission can meet the door's 503 before its 403.
	door := &clockDoor{
		resolver: s.BillingEngine,
		clock:    s.clk,
		creditNoteInvoice: func(ctx context.Context, tenantID, id string) (string, error) {
			cn, err := s.creditNoteStore.Get(ctx, tenantID, id)
			return cn.InvoiceID, err
		},
		dunningRunInvoice: func(ctx context.Context, tenantID, id string) (string, error) {
			run, err := s.dunningStore.GetRun(ctx, tenantID, id)
			return run.InvoiceID, err
		},
	}
	var allow *allowIndex // built once every route is registered, below
	// An unknown path and a method the path does not take answer the JSON
	// error envelope, not chi's plain text. chi hands both handlers down to
	// every router mounted below. The 405 lists the path's methods in Allow.
	r.NotFound(respond.RouteNotFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		respond.MethodNotAllowed(w, req, allow.methods(req.URL))
	})
	// The root chain, in order:
	//   - the edge (useRequestEdge): the trace span, the request id and the
	//     trusted client IP; then request.Begin, which makes the record every
	//     later layer reads; then the span correction, which reads it;
	//   - the metrics and the access log, above the Recoverer, so a recovered
	//     panic is still counted and logged as its 500;
	//   - the Recoverer;
	//   - CORS, below them, so a preflight it answers is counted and logged.
	// Begin and the three readers above the Recoverer run where a panic is
	// not recovered by ours; routeFinder says why Begin cannot panic for a
	// request.
	useRequestEdge(r, s.settings.TrustProxy, s.settings.TrustedProxies(), request.Begin(finder.route))
	r.Use(mw.Metrics())
	r.Use(requestLogger)
	r.Use(mw.Recoverer(loggablePath))
	// The origins, parsed once for both consumers (CORS and the CSRF origin
	// allowlist) with the parser behind the boot check. Both match entries
	// exactly, so entries are trimmed ("a, b" must allow "b") and blank ones
	// are dropped (a blank entry would match a request with no Origin).
	allowedOrigins := s.settings.CORSOrigins()
	r.Use(mw.CORS(allowedOrigins))

	// Cap every request body (limitRequestBody): 1 MB, or the LiteLLM
	// adapter's own larger cap.
	r.Use(limitRequestBody)

	r.Use(mw.SecurityHeaders(s.opts.appEnv))

	// The audit-coverage detector (ADR-090) only observes. It reports any
	// mutating 2xx that wrote no audit row and is not declared exempt in the
	// route table (route_table.go), with slog.Error and the
	// hardcount_audit_uncovered_mutation_total counter. It never touches the
	// response and never buffers the body.
	//
	// It is mounted at the root, not on /v1, so it also sees /v1/auth,
	// /v1/public/*, /v1/webhooks and /v1/bootstrap (ADR-090 RC1). The audit
	// mark it reads, and the client address every audit row records, are on
	// the request's record.
	r.Use(mw.AuditCoverage(auditRoute))

	// No global timeout: each group sets its own, and the event stream sets
	// none.

	// Public
	r.With(door.Middleware).Get("/health", handleHealth)
	r.With(door.Middleware).Get("/health/ready", handleDeepHealth(s.db))
	r.With(door.Middleware).Handle("/metrics", mw.MetricsAuth(s.opts.appEnv, s.settings.MetricsToken, mw.MetricsHandler()))

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
		CreateUserTx: user.NewPostgresStore(s.db).CreateInTx,
		CreateKeyTx:  s.authSvc.CreateKeyTx,
		// Every new account starts with a default dunning policy per mode
		// (ADR-123). RunBootstrap refuses to run when this is nil.
		SeedDunningTx: s.DunningSvc.SeedDefaultPoliciesTx,
		Audit:         s.auditLogger,
	}
	audit.MustWired(&bootstrapDeps)
	bootstrapH := tenant.NewBootstrapHandler(s.db, bootstrapDeps)
	r.Group(func(r chi.Router) {
		r.Use(s.bootstrapLimiter.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		r.Use(door.Middleware)
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
		r.Use(s.authLimiter.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		r.Use(door.Middleware)
		// The invite accept flow is public like login and reset, because
		// the token is the credential. It shares their limiter, so guessing
		// invite tokens is throttled like credential stuffing. Explicit
		// routes win over the Mount("/") wildcard below.
		s.membersH.RegisterPublicRoutes(r)
		r.Mount("/", s.dashboardAuthH.Routes())
	})

	// Stripe webhooks need no API key, because their signature verifies them.
	r.Group(func(r chi.Router) {
		r.Use(mw.Timeout(30 * time.Second))
		r.Use(door.Middleware)
		r.Mount("/v1/webhooks", s.webhookH.Routes())
		// Postmark email webhooks need no API key either. Basic auth gates
		// them, because Postmark signs nothing. The static route wins over the
		// Mount wildcard above.
		r.Post("/v1/webhooks/postmark", s.postmarkH.HandleWebhook)
	})

	// Public payment update: no auth, because the token validates the request.
	// It uses the hosted-invoice routes' tight per-IP bucket, because both are
	// unauthenticated payment endpoints. Without a limit, anyone could guess
	// tokens for free and multiply calls to the Stripe API.
	if s.publicPaymentH != nil {
		r.Group(func(r chi.Router) {
			r.Use(s.hostedInvoiceRL.Middleware())
			r.Use(mw.Timeout(30 * time.Second))
			r.Use(door.Middleware)
			r.Mount("/v1/public/payment-updates", s.publicPaymentH.Routes())
		})
	}

	// Public hosted invoice, the equivalent of Stripe's hosted_invoice_url. It
	// is unauthenticated: the 256-bit public_token in the URL is the only
	// credential. It has its own rate-limit bucket (60/min per IP), because
	// payment pages need tighter limits than the general API (see
	// hostedInvoiceRL above).
	r.Route("/v1/public/invoices", func(r chi.Router) {
		r.Use(s.hostedInvoiceRL.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		r.Use(door.Middleware)
		r.Mount("/", s.hostedInvoiceH.Routes())
	})

	// Public cost dashboard (ADR-031). It is unauthenticated: the 256-bit
	// cost_dashboard_token in the path is the credential. Its rate limit
	// (60/min per IP) is tighter than the general API's, because the embed
	// widget may poll, and abuse from one site must not exhaust shared
	// buckets. The reason and the limit are the same as for the hosted
	// invoice page.
	r.Route("/v1/public/cost-dashboard", func(r chi.Router) {
		r.Use(s.hostedInvoiceRL.Middleware())
		r.Use(mw.Timeout(30 * time.Second))
		r.Use(door.Middleware)
		r.Mount("/", s.customerH.PublicCostDashboardRoutes())
	})

	// Tenant routes take a dashboard session cookie or `Authorization: Bearer
	// <key>`; session.Authenticate decides which. Every route in this block
	// passes the CSRF guard and authentication. The four groups below differ
	// in their rate limit, idempotency and timeout. chi runs a group's
	// middleware only once a route matches, so a path no route here matches
	// answers its 404 or 405 after authentication alone.
	r.Route("/v1", func(r chi.Router) {
		// The CSRF guard refuses a cross-site browser write before auth reads
		// the session cookie. A Bearer request with no session cookie is
		// exempt: a browser does not attach a key on its own.
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(session.RequireCaller(s.sessionSvc, s.authSvc))

		// The event stream: no timeout (an EventSource stays open for hours),
		// and no Idempotency (GET only).
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimiter.Middleware())
			r.Use(door.Middleware)
			r.Route("/webhook_events/stream", eventStreamRoute(s.webhookOutH, s.sessionSvc, s.authSvc, webhook.StreamHeartbeatInterval))
		})

		// The CSV exports: five minutes (a full export outlasts 30 s), and no
		// Idempotency (GET only).
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimiter.Middleware())
			r.Use(mw.Timeout(exportTimeout))
			r.Use(door.Middleware)
			exportsH := newExportsHandler(s.customerStore, s.invoiceStore, s.subStore, s.usageStore, s.auditLogger, s.auditLogger)
			r.Mount("/exports", exportsH.Routes(
				auth.Require(auth.PermCustomerRead),
				auth.Require(auth.PermInvoiceRead),
				auth.Require(auth.PermSubscriptionRead),
				auth.Require(auth.PermUsageRead),
				auth.Require(auth.PermAPIKeyRead),
			))
		})

		// Usage ingest: its own, much larger bucket (ingestLimiter), because
		// LiteLLM drops a rate-limited event for good, and no Idempotency,
		// because each event's own key (or the held row moving once) is its
		// dedup. The LiteLLM body cap is still chosen by path prefix, in
		// limitRequestBody.
		r.Group(func(r chi.Router) {
			r.Use(s.ingestLimiter.Middleware())
			r.Use(mw.Timeout(30 * time.Second))
			r.Use(door.Middleware)
			// Backfill is mounted before the /usage-events subtree, so chi picks
			// the more specific pattern. PermUsageWrite limits it to secret-tier
			// keys, so an ingest key cannot backfill (ADR-133).
			r.With(auth.Require(auth.PermUsageWrite)).Post("/usage-events/backfill", s.usageH.Backfill)
			// Sending events needs PermUsageIngest, the one permission an ingest
			// key holds (ADR-133). The reads, and the held-usage release and
			// discard, need usage read and write.
			r.Mount("/usage-events", s.usageH.Routes(
				auth.Require(auth.PermUsageIngest),
				auth.Require(auth.PermUsageRead),
				auth.Require(auth.PermUsageWrite),
			))
			// The LiteLLM adapter: POST /v1/integrations/litellm/spend. The
			// operator's LiteLLM proxy sends the Bearer key on every callback,
			// so it needs only PermUsageIngest, which an ingest key (ADR-133) or
			// a secret key holds. See ADR-033 and docs/integrations/litellm.md.
			r.With(auth.Require(auth.PermUsageIngest)).Mount("/integrations/litellm",
				litellm.New(s.customerStore, s.pricingSvc, s.usageSvc, s.usageSvc).Routes())
		})

		// Ordinary routes: replayable by Idempotency-Key, 30 s.
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimiter.Middleware())
			// A request whose answer carries a secret shown only once ignores
			// the Idempotency-Key header (answersWithShowOnceSecret): that
			// answer is never kept for a replay.
			r.Use(mw.Idempotency(s.db, answersWithShowOnceSecret))
			r.Use(mw.Timeout(30 * time.Second))
			r.Use(door.Middleware)

			// whoami needs no permission. A key gets key_id and key_type; a session
			// gets user_id and email, which the dashboard needs to stay signed in.
			r.Get("/whoami", s.handleWhoami)

			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/api-keys", s.authH.Routes())
			// Team membership is for dashboard sessions only. An API key is a
			// machine credential with no user to attribute an invite or removal
			// to, so it gets 403 here. The store checks the caller's membership on
			// each write.
			r.With(auth.RequireSession("manage team membership")).Mount("/members", s.membersH.Routes())
			// The /customers subtree mixes reads (GET list, get, billing-profile)
			// and writes (POST create, PATCH update, PUT billing-profile, GDPR
			// delete-data). RequireMethod picks the permission by HTTP method:
			// customer:read for a read, customer:write for a write.
			r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers", s.customerH.Routes())
			// The customer-scoped usage view combines usage totals with customer,
			// subscription and pricing reads. It is gated on PermUsageRead alone,
			// not on the /customers permissions.
			r.Mount("/customers/{id}/usage", s.customerUsageH.CustomerUsageRoutes(
				auth.Require(auth.PermUsageRead),
			))
			// The per-customer margin report (ADR-079) compares rated usage
			// revenue with the stamped provider costs (COGS, cost of goods
			// sold). Only operators can call it: COGS never shows on a
			// customer-facing page, the hosted cost dashboard included.
			r.With(auth.Require(auth.PermUsageRead)).Get("/customers/{id}/margin", s.providerCostH.Margin)
			// Provider cost rates CRUD (ADR-079): the operator's COGS table.
			r.With(auth.RequireMethod(auth.PermUsageRead, auth.PermUsageWrite)).Mount("/provider-costs", s.providerCostH.Routes())
			// The operator's payment-method management. It uses the same Service
			// as the /v1/me/payment-methods customer-facing routes. Only the auth
			// model and the source of customer_id differ. It offers list,
			// setDefault and detach, plus "send setup link", which mints a
			// Stripe Checkout setup URL the operator can give the customer. Card
			// capture stays in the Stripe-hosted iframe, so the operator's PCI
			// scope stays SAQ-A. Chargebee, Lago and Orb offer the same shape.
			r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers/{customer_id}/payment-methods", s.paymentMethodsH.OperatorRoutes())
			r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/meters", s.pricingH.MeterRoutes())
			// A plan and its prices (/v1/plans/{id}/prices) are one subtree.
			r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/plans", s.pricingH.PlanRoutes())
			r.With(auth.Require(auth.PermPricingWrite)).Mount("/recipes", s.recipeH.Routes())
			r.With(auth.RequireMethod(auth.PermSubscriptionRead, auth.PermSubscriptionWrite)).Mount("/subscriptions", s.subH.Routes())
			// create_preview must be mounted before /invoices, because chi tries
			// patterns in registration order. Once /invoices is mounted with its
			// /{id}/... children, "create_preview" would be taken as an invoice
			// ID. See docs/dev/design/create-preview.md.
			r.With(auth.Require(auth.PermInvoiceRead)).Mount("/invoices/create_preview", s.createPreviewH.Routes())
			r.With(auth.RequireMethod(auth.PermInvoiceRead, auth.PermInvoiceWrite)).Mount("/invoices", s.invoiceH.Routes())
			r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/credit-notes", s.creditNoteH.Routes())
			r.With(auth.Require(auth.PermPricingWrite)).Mount("/price-overrides", s.pricingH.OverrideRoutes())
			r.With(auth.Require(auth.PermCustomerWrite)).Mount("/credits", s.creditH.Routes())
			r.With(auth.RequireMethod(auth.PermDunningRead, auth.PermDunningWrite)).Mount("/dunning", s.dunningH.Routes())
			r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/billing", s.billingH.Routes())
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/webhook-endpoints", s.webhookOutH.Routes())
			// The real-time event UI's replay and deliveries timeline live here;
			// the stream has its own group above. The path uses an
			// underscore (webhook_events) on purpose, to match Stripe's
			// /v1/events convention and not collide with the
			// /webhook-endpoints/events list.
			//
			// PermAPIKeyRead is enough for both the read (deliveries) and the
			// replay. Replay changes state, but it only re-issues an event that
			// was already emitted.
			r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/webhook_events", s.webhookOutH.EventRoutes())
			r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/audit-log", s.auditH.Routes())
			// Test and live mode share the account settings, and a key belongs
			// to one mode, so a key may read them but only the dashboard changes
			// them.
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings", s.settingsH.Routes(auth.RequireSession("change account settings")))
			// Each mode has its own Stripe connection, and a caller manages only
			// its own mode's: a key the mode it was made in, a dashboard session
			// the mode it is in. tenantstripe.Service enforces this, not a
			// middleware, because the mode comes from the body or the path.
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings/stripe", s.tenantStripeH.Routes())
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings/email", s.emailSettingsH.Routes())
			r.With(auth.Require(auth.PermInvoiceRead)).Mount("/analytics", s.analyticsH.Routes())
			r.With(auth.Require(auth.PermTestClockWrite)).Mount("/test-clocks", s.testClockH.Routes())

			// Customer Detail: the dashboard's composed per-customer views, in the
			// same /customers/{...} namespace as usage, margin and
			// payment-methods. They are not under /customer-portal, so that name
			// means only the removed self-serve portal (ADR-051).
			customerDetail := newCustomerDetailHandler(s.customerStore, s.subStore, s.invoiceStore)
			r.With(auth.Require(auth.PermCustomerRead)).Get("/customers/{customer_id}/overview", customerDetail.overview)
		})
	})

	allow = newAllowIndex(r)
	finder.root = r

	return r
}

// ServeHTTP serves the request through the router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// useRequestEdge mounts the first middlewares every request passes, begin
// among them; routes() says their order and why.
func useRequestEdge(r chi.Router, trustProxy string, trustedProxies []*net.IPNet, begin func(http.Handler) http.Handler) {
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
	// limits, audit and session rows, logs and spans all read it from the
	// request's record.
	switch {
	case strings.EqualFold(strings.TrimSpace(trustProxy), "none"):
		slog.Info("TRUST_PROXY=none — no proxy in front; rate limiting keys on the direct TCP peer")
	case len(trustedProxies) == 0:
		// Production refuses to start in this state (config.validateFatal).
		// Local and staging get the warning.
		slog.Warn("TRUST_PROXY not set — X-Forwarded-For ignored; rate limiting keys on the direct TCP peer. Behind a balancer that is ONE bucket for everyone: set TRUST_PROXY to the balancer's CIDR, or TRUST_PROXY=none if there is no proxy.")
	}
	r.Use(mw.TrustedClientIP(trustedProxies))
	r.Use(begin)
	r.Use(mw.TraceRequestAttrs(loggablePath))
}

// loggablePath returns a path that holds no link token, for the access
// log, the panic log and the trace's url.path. A link token works like a
// password, and in every link it sits after the path's first segment:
// "/v1/public/invoices/{token}", and the hosted invoice page's own address
// "/invoice/{token}", which the dashboard serves but a deployment may point
// at the API. That two-segment address is why only one segment is kept. It
// returns one of two things:
//
//   - When the request matched a route, its pattern from the request's
//     record, such as "/v1/public/invoices/{token}".
//   - When it matched none, the first segment followed by "/…", such as
//     "/invoice/…" or "/v1/…". A path of one segment or fewer is returned
//     whole. The later segments of an unmatched path add little, and the
//     rule needs no list of routes to keep in step.
func loggablePath(r *http.Request) string {
	if pattern := request.Of(r.Context()).Route().Pattern; pattern != "" {
		return pattern
	}
	segs := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(segs) < 2 || segs[1] == "" {
		return r.URL.Path
	}
	return "/" + segs[0] + "/…"
}

// maxRequestBodyBytes caps a request body on every route but the LiteLLM
// adapter, against oversized payloads (DoS).
const maxRequestBodyBytes = 1 << 20 // 1 MB

// limitRequestBody caps the request body: 1 MB, except on the LiteLLM
// adapter, whose batches of full call records run to several MB
// (litellm.MaxBodyBytes says why and how much). It is the one place every
// route's cap is set, so no route goes uncapped. The adapter is matched on
// the path prefix it is mounted at.
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
