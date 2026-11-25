package api

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"

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
	creditNoteH         *creditnote.Handler
	webhookOutH         *webhook.Handler
	auditLogger         *audit.Logger
	auditH              *audit.Handler
	authSvc             *auth.Service
	authH               *auth.Handler
	settingsH           *tenant.SettingsHandler
	dunningH            *dunning.Handler
	recipeH             *recipe.Handler
	webhookH            *payment.Handler
	invoiceH            *invoice.Handler
	publicPaymentH      *payment.PublicPaymentHandler
	hostedInvoiceH      *hostedinvoice.Handler
	postmarkH           *email.PostmarkHandler
	setupLinksEmailable bool
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
	publicPageLimiter   *mw.RateLimiter
	opts                serverOptions
	settings            config.Server

	BillingEngine    *billing.Engine
	DunningSvc       *dunning.Service
	SettingsStore    *tenant.SettingsStore
	WebhookOutSvc    *webhook.Service
	OutboxStore      *webhook.OutboxStore
	EmailOutboxStore *email.OutboxStore
	EmailSender      *email.Sender
	// EmailSettledChecker lets the email dispatcher skip an action-required
	// email whose invoice was settled while it was queued.
	EmailSettledChecker email.InvoiceSettledChecker
	CreditSvc           *credit.Service
	InvoiceSvc          *invoice.Service
	CreditNoteSvc       *creditnote.Service
	SubscriptionSvc     *subscription.Service
	TokenSvc            *payment.TokenService
	PaymentReconciler   *payment.Reconciler
	LoginThrottle       *user.LoginThrottle
	// RestoreCatchUp applies Stripe's events to a restored database through
	// the webhook handlers (ADR-135); only `hardcount restore catch-up` runs
	// it.
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
	finder := &routeFinder{} // its root is set at the end of routes()
	// Binding the request's time ends every group;
	// docs/architecture.md#the-path-of-a-request says why it sits last.
	requestTime := bindRequestTime(s.clk)
	// chi hands both handlers down to every router mounted below, so the
	// router's every 404 and 405 answers the JSON envelope; a 405 lists the
	// path's methods.
	r.NotFound(respond.RouteNotFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		respond.MethodNotAllowed(w, req, finder.methods(req))
	})
	// The root chain; docs/architecture.md#the-path-of-a-request lists each
	// step and says why the record, the metrics, the access log and the
	// request's time sit where they do.
	// Steps 1 to 5: useRequestEdge, below.
	useRequestEdge(r, s.settings.TrustProxy, s.settings.TrustedProxies(), request.Begin(finder.route))
	r.Use(mw.Metrics())
	r.Use(accessLog)
	r.Use(mw.Recoverer(loggablePath))
	// One parsed list of origins serves CORS and the CSRF guards.
	allowedOrigins := s.settings.CORSOrigins()
	r.Use(mw.CORS(allowedOrigins))
	r.Use(limitRequestBody)
	r.Use(mw.SecurityHeaders(s.opts.appEnv))
	// At the root, not on /v1, so the detector also sees the routes outside
	// /v1's authentication (ADR-090).
	r.Use(mw.AuditCoverage(auditRoute))

	// Public. Every group refuses a query parameter its route does not take
	// (refuseUnknownQuery), right after its rate limit, where it has one.
	r.With(refuseUnknownQuery, requestTime).Get("/health", handleHealth)
	r.With(refuseUnknownQuery, requestTime).Get("/health/ready", handleDeepHealth(s.db))
	r.With(refuseUnknownQuery, requestTime).Handle("/metrics", mw.MetricsAuth(s.opts.appEnv, s.settings.MetricsToken, mw.MetricsHandler()))

	// Bootstrap, the one-time setup, takes no auth. RunBootstrap writes no
	// audit row when deps.Audit is nil, and it mints a live secret key, so
	// MustWired stops the boot instead.
	bootstrapDeps := tenant.BootstrapDeps{
		HashPassword:  user.HashPassword,
		CreateUserTx:  user.NewPostgresStore(s.db).CreateInTx,
		CreateKeyTx:   s.authSvc.CreateKeyTx,
		SeedDunningTx: s.DunningSvc.SeedPoliciesTx,
		Audit:         s.auditLogger,
	}
	audit.MustWired(&bootstrapDeps)
	bootstrapH := tenant.NewBootstrapHandler(s.db, bootstrapDeps)
	r.Group(func(r chi.Router) {
		r.Use(s.bootstrapLimiter.Middleware())
		r.Use(refuseUnknownQuery)
		r.Use(mw.Timeout(requestTimeout))
		r.Use(requestTime)
		r.Mount("/v1/bootstrap", bootstrapH.Routes())
	})

	// The dashboard's account routes (sign-in and sign-out, the mode switch,
	// password reset, invitations) take no API key. The CSRF guard is
	// here because sign-in sets the session cookie: a cross-site POST would
	// fix a sign-in. One limiter slows password and invite-token guessing.
	r.Route("/v1/auth", func(r chi.Router) {
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(s.authLimiter.Middleware())
		r.Use(refuseUnknownQuery)
		r.Use(mw.Timeout(requestTimeout))
		r.Use(requestTime)
		s.membersH.RegisterPublicRoutes(r)
		r.Mount("/", s.dashboardAuthH.Routes())
	})

	// Inbound webhooks take no API key: their handlers check Stripe's
	// signature and Postmark's Basic Auth. The static Postmark route wins
	// over the mount's wildcard.
	r.Group(func(r chi.Router) {
		r.Use(refuseUnknownQuery)
		r.Use(mw.Timeout(requestTimeout))
		r.Use(requestTime)
		r.Mount("/v1/webhooks", s.webhookH.Routes())
		r.Post("/v1/webhooks/postmark", s.postmarkH.HandleWebhook)
	})

	// The public pages (a card update, the hosted invoice, the cost dashboard)
	// take no auth: the token in the path is the credential. They share one
	// tight per-address bucket, so guessing a token is not free.
	r.Group(func(r chi.Router) {
		r.Use(s.publicPageLimiter.Middleware())
		r.Use(refuseUnknownQuery)
		r.Use(mw.Timeout(requestTimeout))
		r.Use(requestTime)
		r.Mount("/v1/public/payment-updates", s.publicPaymentH.Routes())
		r.Mount("/v1/public/invoices", s.hostedInvoiceH.Routes())
		r.Mount("/v1/public/cost-dashboard", s.customerH.PublicCostDashboardRoutes())
	})

	// Every other /v1 route takes a session cookie or a Bearer key, passes the
	// route table's permission check, then joins one of four groups, which
	// differ in rate limit, Idempotency and time limit. The CSRF guard comes
	// before authentication, so a cross-site write is refused before the
	// session cookie is read. The permission check answers a path no route
	// serves with its 404 or 405 (noRoute), so a group's steps and handlers run
	// only for a listed route the caller may call. chi matches a static path
	// segment before a parameter, whatever the order of the mounts below, so
	// /webhook_events/stream, /usage-events/backfill, /invoices/create_preview
	// and /settings/stripe each win over their sibling.
	r.Route("/v1", func(r chi.Router) {
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(session.RequireCaller(s.sessionSvc, s.authSvc))
		r.Use(auth.RequirePermission(tableAccess{}, noRoute{finder}))

		// The event stream: no timeout (an EventSource stays open for hours),
		// and no Idempotency (GET only). It takes the session and key services
		// because it checks its caller again while the stream is open
		// (event_stream_route.go).
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimiter.Middleware())
			r.Use(refuseUnknownQuery)
			r.Use(requestTime)
			r.Route("/webhook_events/stream", eventStreamRoute(s.webhookOutH, s.sessionSvc, s.authSvc, webhook.StreamHeartbeatInterval))
		})

		// The CSV exports: five minutes (a full export outlasts 30 s), and no
		// Idempotency (GET only).
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimiter.Middleware())
			r.Use(refuseUnknownQuery)
			r.Use(mw.Timeout(exportTimeout))
			r.Use(requestTime)
			exportsH := newExportsHandler(s.customerStore, s.invoiceStore, s.subStore, s.usageStore, s.auditLogger, s.auditLogger)
			r.Mount("/exports", exportsH.Routes())
		})

		// Usage ingest: its own, much larger bucket, because LiteLLM drops a
		// rate-limited event for good, and no Idempotency, because each event's
		// own key already stops a second copy, and releasing or discarding
		// held usage marks each held row once, so a second call finds nothing
		// held (route_table.go's usage entries say what held usage is).
		r.Group(func(r chi.Router) {
			r.Use(s.ingestLimiter.Middleware())
			r.Use(refuseUnknownQuery)
			r.Use(mw.Timeout(requestTimeout))
			r.Use(requestTime)
			r.Post("/usage-events/backfill", s.usageH.Backfill)
			r.Mount("/usage-events", s.usageH.Routes())
			r.Mount("/integrations/litellm", litellm.New(s.customerStore, s.pricingSvc, s.usageSvc, s.usageSvc).Routes())
		})

		// Ordinary routes: replayable by Idempotency-Key, 30 s.
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimiter.Middleware())
			r.Use(refuseUnknownQuery)
			// keepsNoKey says which requests Idempotency holds no key for.
			r.Use(mw.Idempotency(s.db, keepsNoKey))
			r.Use(mw.Timeout(requestTimeout))
			r.Use(requestTime)

			r.Get("/whoami", s.handleWhoami)
			r.Mount("/api-keys", s.authH.Routes())
			r.Mount("/members", s.membersH.Routes())
			r.Mount("/customers", s.customerH.Routes())
			r.Mount("/customers/{id}/usage", s.customerUsageH.CustomerUsageRoutes())
			r.Get("/customers/{id}/margin", s.providerCostH.Margin)
			r.Mount("/provider-costs", s.providerCostH.Routes())
			r.Mount("/customers/{customer_id}/payment-methods", s.paymentMethodsH.OperatorRoutes())
			r.Mount("/meters", s.pricingH.MeterRoutes())
			r.Mount("/plans", s.pricingH.PlanRoutes())
			r.Mount("/recipes", s.recipeH.Routes())
			r.Mount("/subscriptions", s.subH.Routes())
			r.Mount("/invoices/create_preview", s.createPreviewH.Routes())
			r.Mount("/invoices", s.invoiceH.Routes())
			r.Mount("/credit-notes", s.creditNoteH.Routes())
			r.Mount("/price-overrides", s.pricingH.OverrideRoutes())
			r.Mount("/credits", s.creditH.Routes())
			r.Mount("/dunning", s.dunningH.Routes())
			r.Mount("/billing", s.billingH.Routes())
			r.Mount("/webhook-endpoints", s.webhookOutH.Routes())
			r.Mount("/webhook_events", s.webhookOutH.EventRoutes())
			r.Mount("/audit-log", s.auditH.Routes())
			r.Mount("/settings", s.settingsH.Routes())
			// A caller manages only its own mode's Stripe connection.
			// tenantstripe.Service checks it, not a middleware, because the mode
			// comes from the body or the path.
			r.Mount("/settings/stripe", s.tenantStripeH.Routes())
			r.Mount("/settings/email", s.emailSettingsH.Routes())
			r.Mount("/analytics", s.analyticsH.Routes())
			customerDetail := newCustomerDetailHandler(s.customerStore, s.subStore, s.invoiceStore)
			r.Get("/customers/{customer_id}/overview", customerDetail.overview)
		})
	})

	finder.root = r
	mustListEveryRoute(r)

	return r
}

// ServeHTTP serves the request through the router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// useRequestEdge mounts the first steps of the root chain, begin among them
// (docs/architecture.md#the-path-of-a-request).
func useRequestEdge(r chi.Router, trustProxy string, trustedProxies []*net.IPNet, begin func(http.Handler) http.Handler) {
	r.Use(mw.Tracing())
	// mw.RequestID, not chi's: chi's takes an inbound X-Request-Id as sent,
	// which would let a caller choose the request_id on their own audit rows.
	r.Use(mw.RequestID)
	switch {
	case strings.EqualFold(strings.TrimSpace(trustProxy), "none"):
		slog.Info("TRUST_PROXY=none — no proxy in front; rate limiting keys on the direct TCP peer")
	case len(trustedProxies) == 0:
		// Production refuses to start in this state (config.validateFatal).
		slog.Warn("TRUST_PROXY not set — X-Forwarded-For ignored; rate limiting keys on the direct TCP peer. Behind a balancer that is ONE bucket for everyone: set TRUST_PROXY to the balancer's CIDR, or TRUST_PROXY=none if there is no proxy.")
	}
	r.Use(mw.TrustedClientIP(trustedProxies))
	r.Use(begin)
	r.Use(mw.TraceRequestAttrs(loggablePath))
}

// loggablePath is r's path with no link token in it, for the access log, the
// panic log and the span's url.path: the pattern of the route it matched, or,
// when it matched none, its first segment and "/…" (a path of one segment or
// fewer is kept whole). A link token works like a password and always comes
// after the first segment: "/v1/public/invoices/{token}", and
// "/invoice/{token}", the hosted invoice link, which reaches the API when
// HOSTED_INVOICE_BASE_URL is set to the API's address.
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

// requestTimeout is the time limit of most groups. The exports have
// exportTimeout; the event stream, /health and /metrics have none.
const requestTimeout = 30 * time.Second

// maxRequestBodyBytes caps a request body on every route but the LiteLLM
// adapter.
const maxRequestBodyBytes = 1 << 20 // 1 MB

// limitRequestBody caps every request body: 1 MB, or litellm.MaxBodyBytes on
// the LiteLLM adapter, whose batches run to several MB. It is the one place a
// route's cap is set, so no route goes uncapped.
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

// mustListEveryRoute refuses to build a server that serves a route the route
// table does not list, and names the route. Such a route would run with no
// clock decision, no audit and no retry key, and its record could not tell it
// from a path no route serves. TestRouteTable_MatchesLiveRoutes holds the same
// rule in CI.
func mustListEveryRoute(router chi.Routes) {
	// Walk returns only the walk function's error, and this one has none.
	_ = chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := routeKey{Method: method, Pattern: canonicalRoute(route)}
		if _, ok := routeTable[key]; !ok {
			panic(fmt.Sprintf("route %s %s is served but not in the route table (route_table.go)", key.Method, key.Pattern))
		}
		return nil
	})
}
