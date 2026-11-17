package api

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"

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
	// TestClockSvc runs the test-clock catch-up off the request: what fell due
	// in the time an advancing clock skipped (ADR-140).
	TestClockSvc *testclock.Service
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
	finder := &routeFinder{}
	// The clock door ends every group; docs/architecture.md#the-path-of-a-request
	// says why it sits last.
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
	// chi hands both handlers down to every router mounted below, so the
	// router's every 404 and 405 answers the JSON envelope; a 405 lists the
	// path's methods.
	r.NotFound(respond.RouteNotFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		respond.MethodNotAllowed(w, req, allow.methods(req))
	})
	// The root chain; docs/architecture.md#the-path-of-a-request says why each
	// step sits where it does. Everything above the Recoverer runs where a
	// panic is not recovered by ours; routeFinder says why Begin cannot panic.
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

	// No global timeout: each group sets its own, and the event stream sets
	// none.

	// Public
	r.With(door.Middleware).Get("/health", handleHealth)
	r.With(door.Middleware).Get("/health/ready", handleDeepHealth(s.db))
	r.With(door.Middleware).Handle("/metrics", mw.MetricsAuth(s.opts.appEnv, s.settings.MetricsToken, mw.MetricsHandler()))

	// Bootstrap, the one-time setup, takes no auth. RunBootstrap writes no
	// audit row when deps.Audit is nil, and it mints a live secret key, so
	// MustWired stops the boot instead.
	bootstrapDeps := tenant.BootstrapDeps{
		HashPassword:  user.HashPassword,
		CreateUserTx:  user.NewPostgresStore(s.db).CreateInTx,
		CreateKeyTx:   s.authSvc.CreateKeyTx,
		SeedDunningTx: s.DunningSvc.SeedDefaultPoliciesTx,
		Audit:         s.auditLogger,
	}
	audit.MustWired(&bootstrapDeps)
	bootstrapH := tenant.NewBootstrapHandler(s.db, bootstrapDeps)
	r.Group(func(r chi.Router) {
		r.Use(s.bootstrapLimiter.Middleware())
		r.Use(mw.Timeout(requestTimeout))
		r.Use(door.Middleware)
		r.Mount("/v1/bootstrap", bootstrapH.Routes())
	})

	// The dashboard's account routes (sign-in and sign-out, the mode switch,
	// password reset, invitations) take no API key. The CSRF guard comes
	// first, because sign-in sets the session cookie: a cross-site POST would
	// fix a sign-in. One limiter slows password and invite-token guessing.
	r.Route("/v1/auth", func(r chi.Router) {
		r.Use(mw.CSRFGuard(allowedOrigins))
		r.Use(s.authLimiter.Middleware())
		r.Use(mw.Timeout(requestTimeout))
		r.Use(door.Middleware)
		s.membersH.RegisterPublicRoutes(r)
		r.Mount("/", s.dashboardAuthH.Routes())
	})

	// Inbound webhooks take no API key: their handlers check Stripe's
	// signature and Postmark's Basic Auth. The static Postmark route wins
	// over the mount's wildcard.
	r.Group(func(r chi.Router) {
		r.Use(mw.Timeout(requestTimeout))
		r.Use(door.Middleware)
		r.Mount("/v1/webhooks", s.webhookH.Routes())
		r.Post("/v1/webhooks/postmark", s.postmarkH.HandleWebhook)
	})

	// The public pages (a card update, the hosted invoice, the cost dashboard)
	// take no auth: the token in the path is the credential. They share one
	// tight per-address bucket, so guessing a token is not free.
	r.Group(func(r chi.Router) {
		r.Use(s.hostedInvoiceRL.Middleware())
		r.Use(mw.Timeout(requestTimeout))
		r.Use(door.Middleware)
		if s.publicPaymentH != nil {
			r.Mount("/v1/public/payment-updates", s.publicPaymentH.Routes())
		}
		r.Mount("/v1/public/invoices", s.hostedInvoiceH.Routes())
		r.Mount("/v1/public/cost-dashboard", s.customerH.PublicCostDashboardRoutes())
	})

	// Every other /v1 route takes a session cookie or a Bearer key, then joins
	// one of four groups, which differ in rate limit, Idempotency and time
	// limit. chi runs a group's steps only once a route matches, so a path no
	// route matches answers its 404 or 405 after authentication alone. The
	// CSRF guard comes before authentication, so a cross-site write is refused
	// before the session cookie is read.
	r.Route("/v1", func(r chi.Router) {
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

		// Usage ingest: its own, much larger bucket, because LiteLLM drops a
		// rate-limited event for good, and no Idempotency, because each event's
		// own key (or the held row moving once) is its dedup. Sending usage
		// needs only usage:ingest, the one permission an ingest key holds
		// (ADR-133); backfill, the reads and the held-usage actions need more.
		r.Group(func(r chi.Router) {
			r.Use(s.ingestLimiter.Middleware())
			r.Use(mw.Timeout(requestTimeout))
			r.Use(door.Middleware)
			r.With(auth.Require(auth.PermUsageWrite)).Post("/usage-events/backfill", s.usageH.Backfill)
			r.Mount("/usage-events", s.usageH.Routes(
				auth.Require(auth.PermUsageIngest),
				auth.Require(auth.PermUsageRead),
				auth.Require(auth.PermUsageWrite),
			))
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
			r.Use(mw.Timeout(requestTimeout))
			r.Use(door.Middleware)

			r.Get("/whoami", s.handleWhoami)
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/api-keys", s.authH.Routes())
			r.With(auth.RequireSession("manage team membership")).Mount("/members", s.membersH.Routes())
			r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers", s.customerH.Routes())
			// A customer's usage and margin need usage:read, not the /customers
			// permissions.
			r.Mount("/customers/{id}/usage", s.customerUsageH.CustomerUsageRoutes(
				auth.Require(auth.PermUsageRead),
			))
			r.With(auth.Require(auth.PermUsageRead)).Get("/customers/{id}/margin", s.providerCostH.Margin)
			r.With(auth.RequireMethod(auth.PermUsageRead, auth.PermUsageWrite)).Mount("/provider-costs", s.providerCostH.Routes())
			r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).Mount("/customers/{customer_id}/payment-methods", s.paymentMethodsH.OperatorRoutes())
			r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/meters", s.pricingH.MeterRoutes())
			r.With(auth.RequireMethod(auth.PermPricingRead, auth.PermPricingWrite)).Mount("/plans", s.pricingH.PlanRoutes())
			r.With(auth.Require(auth.PermPricingWrite)).Mount("/recipes", s.recipeH.Routes())
			r.With(auth.RequireMethod(auth.PermSubscriptionRead, auth.PermSubscriptionWrite)).Mount("/subscriptions", s.subH.Routes())
			r.With(auth.Require(auth.PermInvoiceRead)).Mount("/invoices/create_preview", s.createPreviewH.Routes())
			r.With(auth.RequireMethod(auth.PermInvoiceRead, auth.PermInvoiceWrite)).Mount("/invoices", s.invoiceH.Routes())
			r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/credit-notes", s.creditNoteH.Routes())
			r.With(auth.Require(auth.PermPricingWrite)).Mount("/price-overrides", s.pricingH.OverrideRoutes())
			r.With(auth.Require(auth.PermCustomerWrite)).Mount("/credits", s.creditH.Routes())
			r.With(auth.RequireMethod(auth.PermDunningRead, auth.PermDunningWrite)).Mount("/dunning", s.dunningH.Routes())
			r.With(auth.Require(auth.PermInvoiceWrite)).Mount("/billing", s.billingH.Routes())
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/webhook-endpoints", s.webhookOutH.Routes())
			// A replay needs only apikey:read: it sends again an event already
			// emitted.
			r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/webhook_events", s.webhookOutH.EventRoutes())
			r.With(auth.Require(auth.PermAPIKeyRead)).Mount("/audit-log", s.auditH.Routes())
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings", s.settingsH.Routes(auth.RequireSession("change account settings")))
			// A caller manages only its own mode's Stripe connection.
			// tenantstripe.Service checks it, not a middleware, because the mode
			// comes from the body or the path.
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings/stripe", s.tenantStripeH.Routes())
			r.With(auth.Require(auth.PermAPIKeyWrite)).Mount("/settings/email", s.emailSettingsH.Routes())
			r.With(auth.Require(auth.PermInvoiceRead)).Mount("/analytics", s.analyticsH.Routes())
			r.With(auth.Require(auth.PermTestClockWrite)).Mount("/test-clocks", s.testClockH.Routes())
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
// "/invoice/{token}", the hosted page a deployment may point at the API.
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

// methods lists the methods the router serves at r's path.
func (a *allowIndex) methods(r *http.Request) []string {
	var out []string
	for _, m := range routeMethods {
		if a.flat.Match(chi.NewRouteContext(), m, routedPath(r)) {
			out = append(out, m)
		}
	}
	return out
}
