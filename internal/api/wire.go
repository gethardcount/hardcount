package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gethardcount/hardcount/internal/analytics"
	mw "github.com/gethardcount/hardcount/internal/api/middleware"
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
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/user"
	"github.com/gethardcount/hardcount/internal/webhook"
	"github.com/redis/go-redis/v9"
	"github.com/stripe/stripe-go/v82"
)

// ServerOption sets a boot-time value that cmd/hardcount reads from config.
// Tests set only what they exercise.
type ServerOption func(*serverOptions)

type serverOptions struct {
	invoiceGrace time.Duration
	appEnv       string
	rateLimits   config.RateLimits
	stripeKeys   map[tenantstripe.Account]string
}

// WithStripeKeys replaces stored Stripe keys for the life of this Server,
// without saving them. Only `hardcount restore catch-up --stripe-key` uses it.
func WithStripeKeys(keys map[tenantstripe.Account]string) ServerOption {
	return func(o *serverOptions) { o.stripeKeys = keys }
}

// WithAppEnv sets the environment, APP_ENV as config.Load normalised it.
// Without it the server runs as "local".
func WithAppEnv(env string) ServerOption {
	return func(o *serverOptions) { o.appEnv = env }
}

// WithRateLimits sets the HARDCOUNT_RATE_LIMIT_* limits config.Load parsed.
// Without it NewServer uses config.DefaultRateLimits.
func WithRateLimits(rl config.RateLimits) ServerOption {
	return func(o *serverOptions) { o.rateLimits = rl }
}

// WithInvoiceGrace sets INVOICE_GRACE_PERIOD (ADR-120). The billing runs'
// due scans and the backlog gauges take the same value, so they agree on
// when a period is due.
func WithInvoiceGrace(d time.Duration) ServerOption {
	return func(o *serverOptions) { o.invoiceGrace = d }
}

// wire builds the stores, services and handlers, and the Server that holds
// what the routes read. Each value is built once what it needs exists. The
// boot gates near the end refuse to start with an engine collaborator, audit
// emitter or outbox left unwired.
func wire(db *postgres.DB, clk clock.Clock, rdb *redis.Client, opts ...ServerOption) *Server {
	if clk == nil {
		clk = clock.Real()
	}
	o := serverOptions{appEnv: "local", rateLimits: config.DefaultRateLimits()}
	for _, opt := range opts {
		opt(&o)
	}
	// docs/settings.md describes each setting. A warning below names what an
	// unset one turns off. Check refuses a value the server cannot use, as
	// config.Load does.
	srv := config.ServerFromEnv()
	if err := srv.Check(); err != nil {
		panic(err.Error())
	}

	invoiceStore := invoice.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	webhookStore := payment.NewPostgresWebhookStore(db)
	tenantSvc := tenant.NewService(tenant.NewPostgresStore(db))
	customerStore := customer.NewPostgresStore(db)

	// One encryptor covers customer PII, webhook secrets, Stripe keys and
	// hosted-invoice tokens, so a key rotation reaches all of them. config
	// requires a key in production, and srv.Check refused a key that is not
	// 64 hex characters.
	var sharedEnc *crypto.Encryptor
	if encKey := srv.EncryptionKey; encKey != "" {
		enc, err := crypto.NewEncryptor(encKey)
		if err != nil {
			panic(err.Error())
		}
		sharedEnc = enc
		customerStore.SetEncryptor(enc)
		invoiceStore.SetEncryptor(enc)
		slog.Info("encryption at rest enabled for customer PII, webhook secrets, Stripe credentials, and hosted-invoice tokens on invoices; an unsent email in email_outbox holds its link token unencrypted until it is sent")
	} else {
		slog.Warn("HARDCOUNT_ENCRYPTION_KEY not set — customer PII, webhook secrets, Stripe credentials, and hosted-invoice tokens stored in plaintext")
	}

	// Each tenant connects its own Stripe account; there is no platform key.
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

	// Payment methods. Card status is read from customers and payment_methods,
	// never from the deprecated customer_payment_setups table.
	paymentMethodsStripe := paymentmethods.NewStripeAdapter(stripeClients, customerStore)
	paymentMethodsStore := paymentmethods.NewPostgresStore(db)
	paymentMethodsSvc := paymentmethods.NewService(paymentMethodsStore, paymentMethodsStripe, customerStore)
	customerPortalURL := srv.CustomerPortalURL
	if customerPortalURL == "" {
		slog.Warn("CUSTOMER_PORTAL_URL NOT SET — setup links (Send setup link, and setup sessions with no return_url) are refused with return_address_not_set, because Stripe would have no page to send the customer back to. Set this to your dashboard's address, e.g. https://app.example.com")
	}
	paymentMethodsSvc.SetPortalBaseURL(customerPortalURL)
	paymentSetupStore := &compositePaymentSetupStore{customers: customerStore, pms: paymentMethodsSvc}

	// Pricing, customers, subscriptions, usage and credit.
	pricingSvc := pricing.NewService(pricingStore)
	pricingSvc.SetSubscriptionPlanUsageReader(subStore)
	pricingSvc.SetCustomerReader(customerStore)
	customerSvc := customer.NewService(customerStore)
	customerSvc.SetStripeSyncer(payment.NewStripeBillingSync(stripeClients))
	customerH := customer.NewHandler(customerSvc)
	pricingH := pricing.NewHandler(pricingSvc)
	subSvc := subscription.NewService(subStore, clk)
	subH := subscription.NewHandler(subSvc)
	usageSvc := usage.NewService(usageStore)
	usageH := usage.NewHandler(usageSvc, customerStore, pricingSvc)
	usageH.SetMeterLister(pricingSvc)
	usageH.SetHeldDeps(customerStore)
	customerUsageSvc := usage.NewCustomerUsageService(usageSvc, customerStore, subStore, pricingSvc)
	customerUsageH := usage.NewCustomerUsageHandler(customerUsageSvc)
	providerCostH := usage.NewProviderCostHandler(usageStore, usage.NewMarginAssembler(usageStore, customerUsageSvc))
	settingsStore := tenant.NewSettingsStore(db)
	subSvc.SetSettingsReader(settingsStore)
	subSvc.SetCustomerReader(customerStore)
	subSvc.SetProfileReader(customerStore)
	subSvc.SetPlanReader(pricingStore)
	subSvc.SetPlanMeterReader(pricingStore)
	creditStore := credit.NewPostgresStore(db)
	creditSvc := credit.NewService(creditStore)
	creditSvc.SetCustomerReader(customerStore)
	creditH := credit.NewHandler(creditSvc)
	creditNoteStore := creditnote.NewPostgresStore(db)
	if sharedEnc != nil {
		creditNoteStore.SetEncryptor(sharedEnc)
	}
	subH.SetProrationDeps(pricingSvc, &prorationInvoiceCreatorAdapter{PostgresStore: invoiceStore, numberer: settingsStore})

	// Credit notes and refunds, and the operator's webhook endpoints.
	stripeRefunder := payment.NewStripeRefunder(stripeClients)
	creditNoteSvc := creditnote.NewService(creditNoteStore, invoiceStore, stripeRefunder, &creditGrantAdapter{svc: creditSvc})
	creditNoteSvc.SetClock(clk)
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

	// Each webhook event is queued in the transaction of the change it
	// reports, so a crash after the commit cannot lose it (ADR-040). The
	// dispatcher worker in cmd/hardcount sends them.
	outboxStore := webhook.NewOutboxStore(db)
	invoiceStore.SetOutboxEnqueuer(outboxStore)
	creditStore.SetOutboxEnqueuer(outboxStore)
	subStore.SetOutboxEnqueuer(outboxStore)
	customerStore.SetOutboxEnqueuer(outboxStore)
	paymentMethodsStore.SetOutboxEnqueuer(outboxStore)

	invoiceStore.SetCommitFunder(creditSvc)
	invoiceStore.SetInvoiceNumberer(settingsStore)
	invoiceStore.SetDunningPauseReleaser(subStore)

	// Audit, API keys, the cost dashboard and settings.
	auditLogger := audit.NewLogger(db)
	auditLogger.SetEncryptor(sharedEnc)
	auditH := audit.NewHandler(auditLogger)
	authSvc := auth.NewService(db, auditLogger)
	authH := auth.NewHandler(authSvc)
	customerH.SetAuditLogger(auditLogger)
	costDashboardSvc := usage.NewCostDashboardAssembler(customerStore, customerUsageSvc, subStore, costDashboardCredits{svc: creditSvc})
	customerH.SetCostDashboardService(costDashboardSvc)
	if base := srv.APIBaseURL; base != "" {
		customerH.SetAPIBaseURL(base)
	} else {
		slog.Warn("HARDCOUNT_API_BASE_URL NOT SET — the public cost-dashboard URL operators copy will be a relative path, not a shareable link. Set this to your public API origin (e.g. https://api.example.com).")
	}
	settingsH := tenant.NewSettingsHandler(settingsStore)
	settingsH.SetAuditLogger(auditLogger)

	// Dunning and recipes.
	stripeClient := payment.NewLiveStripeClient(stripeClients)
	dunningStore := dunning.NewPostgresStore(db)
	dunningStore.SetOutboxEnqueuer(outboxStore)
	dunningSvc := dunning.NewService(dunningStore, nil, clk) // retrier set below after stripeAdapter created
	dunningH := dunning.NewHandler(dunningSvc)
	// The recipes are embedded and TestLoad checks them, so a load error is a
	// broken build.
	recipeRegistry, err := recipe.Load()
	if err != nil {
		panic(fmt.Sprintf("load recipe registry: %v", err))
	}
	recipeStore := recipe.NewPostgresStore(db)
	recipeSvc := recipe.NewService(db, recipeStore, recipeRegistry, pricingSvc)
	recipeH := recipe.NewHandler(recipeSvc)

	// One breaker for charges and the reconciler, across tenants, so both back
	// off together while Stripe is failing.
	stripeBreaker := breaker.New(breaker.Config{
		FailureThreshold: 5,
		Cooldown:         30 * time.Second,
		Interval:         60 * time.Second,
		Countable:        payment.IsUnknownPaymentFailure,
		OnStateChange: func(_, to breaker.State) {
			payment.RecordStripeBreakerState(string(to))
		},
	})

	// The Stripe adapter: charges, and the webhooks Stripe sends.
	stripeAdapter := payment.NewStripe(stripeClient, invoiceStore, webhookStore, paymentSetupStore, dunningSvc)
	stripeAdapter.SetCardFetcher(stripeClient)
	stripeAdapter.SetClock(clk)
	stripeAdapter.SetAnomalyRecorder(invoiceStore)
	checkoutSessionStore := payment.NewCheckoutSessionStore(db)
	stripeAdapter.SetCheckoutSessionStore(checkoutSessionStore, stripeClients)
	stripeAdapter.SetBreaker(stripeBreaker)
	stripeAdapter.SetRefundStatusUpdater(creditNoteSvc)
	stripeAdapter.SetDisputeRecorder(invoiceStore)
	creditNoteSvc.SetStripeRefundRecording(creditNoteStore, invoiceStore, outboxStore)
	creditNoteSvc.SetRefundReader(stripeRefunder)
	stripeAdapter.SetExternalRefundRecorder(creditNoteSvc)
	stripeAdapter.SetDunningResolver(dunningSvc)
	stripeAdapter.SetDunningRetryFailure(dunningSvc)
	stripeAdapter.SetKeyHealth(tenantStripeSvc)
	creditNoteSvc.SetDunningResolver(dunningSvc)
	dunningSvc.SetRetrier(&paymentRetrierAdapter{
		charger:       stripeAdapter,
		invoiceStore:  invoiceStore,
		paymentSetups: paymentSetupStore,
		credits:       creditSvc,
		stripeReady:   stripeClients,
	})
	dunningSvc.SetSubscriptionPauser(&subscriptionPauserAdapter{svc: subSvc}, invoiceStore)
	dunningSvc.SetSubscriptionCanceler(&subscriptionCancelerAdapter{svc: subSvc})
	dunningSvc.SetSubscriptionStateReader(&subscriptionStateAdapter{svc: subSvc})
	dunningSvc.SetCustomerPolicyReader(customerSvc)
	webhookH := payment.NewHandler(stripeAdapter, tenantStripeSvc)

	// Invoices.
	invoiceSvc := invoice.NewService(invoiceStore, clk, settingsStore)
	invoiceSvc.SetCollectionStopper(stripeAdapter)
	invoiceSvc.SetDunningResolver(dunningSvc)
	creditNoteSvc.SetCheckoutExpirer(stripeAdapter)
	invoiceSvc.SetHeldReliefIssuer(heldReliefIssuerAdapter{svc: creditNoteSvc})
	invoiceSvc.SetStripeRefunds(creditNoteSvc)
	dunningSvc.SetInvoiceUncollectibleMarker(&invoiceUncollectibleAdapter{svc: invoiceSvc})
	dunningSvc.SetChargeHoldClaimer(invoiceStore)
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
	tokenSvc := payment.NewTokenService(db)

	// The reconciler asks Stripe for a charge whose outcome is unknown or
	// still processing. It waits 60 s, or 30 minutes for a processing one, so
	// the webhook usually settles it first.
	paymentReconciler := payment.NewReconciler(stripeClient, invoiceStore, stripeAdapter, 60*time.Second)
	paymentReconciler.SetBreaker(stripeBreaker)
	paymentReconciler.SetProcessingReconcileAfter(30 * time.Minute)
	paymentReconciler.SetHostedSessions(checkoutSessionStore, stripeClient, stripeAdapter)

	// The public pages: payment update and the hosted invoice.
	paymentUpdateReturnURL := srv.PaymentUpdateReturnURL
	if paymentUpdateReturnURL == "" {
		slog.Warn("PAYMENT_UPDATE_RETURN_URL NOT SET — \"add a payment method\" emails are not sent, and an existing link's page refuses to add the card, because Stripe would have no page to send the customer back to. Set this to your dashboard's /payment-method-added page, e.g. https://app.example.com/payment-method-added")
	}
	publicPaymentH := payment.NewPublicPaymentHandler(tokenSvc, db, stripeClients, customerSvc, paymentMethodsStripe,
		paymentUpdateReturnURL)
	hostedInvoiceBaseURL := srv.HostedInvoiceBaseURL
	// A named variable, so audit.MustWired can check its emitter.
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

	// Email. Every producer but the Settings test send queues into
	// email_outbox through outboxSender; the dispatcher worker in
	// cmd/hardcount sends through emailSender (ADR-040).
	emailSender := email.NewSender(o.appEnv)
	emailSender.SetSettingsGetter(settingsStore)
	emailSender.SetStripeReadiness(stripeClients)
	if !emailSender.IsConfigured() {
		slog.Warn("SMTP NOT CONFIGURED — every customer-facing email (invoices, receipts, dunning, payment-setup links, password resets, team invites) will fail with ErrSMTPNotConfigured. Set SMTP_HOST + credentials. See docs/ops/email-setup.md.")
	} else {
		slog.Info("SMTP configured", "host", emailSender.SMTPHost())
	}
	if hostedInvoiceBaseURL == "" {
		slog.Warn("HOSTED_INVOICE_BASE_URL NOT SET — invoice, receipt, dunning and payment-failed emails carry no payment link, and the hosted invoice page offers no Pay button. Set this to your hosted invoice page's address (e.g. https://billing.example.com).")
	}
	emailOutboxStore := email.NewOutboxStore(db)
	invoiceH.SetEmailEvents(&invoiceEmailEventsAdapter{store: emailOutboxStore})
	invoiceH.SetTopupGrants(creditStore)
	customerH.SetSentEmailsLister(&customerSentEmailsAdapter{store: emailOutboxStore})
	// Postmark reports to one platform route: one Postmark account serves
	// every tenant (ADR-098).
	postmarkWebhookUser := srv.PostmarkWebhookUser
	postmarkWebhookPass := srv.PostmarkWebhookPass
	postmarkH := email.NewPostmarkHandler(emailOutboxStore, postmarkWebhookUser, postmarkWebhookPass)
	if postmarkWebhookUser == "" || postmarkWebhookPass == "" {
		slog.Warn("POSTMARK_WEBHOOK_USER/POSTMARK_WEBHOOK_PASS not set — /v1/webhooks/postmark rejects all posts (401); delivery/bounce webhooks will not ingest")
	}
	outboxSender := email.NewOutboxSender(emailOutboxStore)
	invoiceH.SetEmailSender(outboxSender)
	invoiceH.SetEmailConfigured(emailSender.IsConfigured())
	customerEmailAdapter := &customerEmailFetcherAdapter{store: customerStore}
	dunningSvc.SetEmailNotifier(outboxSender)
	dunningSvc.SetCustomerEmailFetcher(customerEmailAdapter)
	stripeAdapter.SetEmailReceipt(outboxSender, customerEmailAdapter)
	paymentUpdateURL := srv.PaymentUpdateURL
	if paymentUpdateURL == "" {
		slog.Warn("PAYMENT_UPDATE_URL NOT SET — payment-setup-request emails (no-PM-at-finalize) will fail at send time. Set this to your customer-portal payment-update page URL.")
	}
	setupLinksEmailable := canEmailSetupLinks(emailSender.IsConfigured(), paymentUpdateURL, paymentUpdateReturnURL)
	invoiceSvc.SetSetupLinkEmailConfigured(setupLinksEmailable)
	invoiceSvc.SetHostedPayConfigured(hostedInvoiceBaseURL != "")
	invoiceSvc.SetSetupLinkCopyConfigured(customerPortalURL != "")
	invoiceSvc.SetInvoiceEmailConfigured(emailSender.IsConfigured())
	stripeAdapter.SetEmailPaymentFailed(outboxSender, customerEmailAdapter)

	// Audit emitters, and the activity lines that read their rows. A service
	// writes its row in the transaction of the change (ADR-090); a handler
	// writes after it.
	creditSvc.SetAuditLogger(auditLogger)
	creditNoteSvc.SetAuditLogger(auditLogger)
	stripeAdapter.SetAuditLogger(auditLogger)
	publicPaymentH.SetAuditLogger(auditLogger)
	invoiceH.SetAuditLogger(auditLogger)
	invoiceH.SetAuditStamps(&invoiceAuditStampsAdapter{logger: auditLogger})
	invoiceH.SetSubscriptionLabeler(&invoiceSubscriptionLabelAdapter{store: subStore})
	dunningH.SetEventLiner(invoiceH)
	subH.SetAuditLogger(auditLogger)
	subH.SetInvoiceLabeler(&subscriptionInvoiceLabelAdapter{store: invoiceStore})
	creditNoteH.SetAuditLogger(auditLogger)
	creditNoteH.SetEmailSender(outboxSender)
	creditNoteH.SetEmailConfigured(emailSender.IsConfigured())
	paymentMethodsSvc.SetAuditLogger(auditLogger)
	dunningH.SetAuditLogger(auditLogger)
	pricingH.SetAuditLogger(auditLogger)
	tenantStripeH.SetAuditLogger(auditLogger)
	customerSvc.SetAuditLogger(auditLogger)
	pricingSvc.SetAuditLogger(auditLogger)
	recipeSvc.SetAuditLogger(auditLogger)
	providerCostH.SetAuditLogger(auditLogger)
	usageSvc.SetAuditLogger(auditLogger)
	webhookOutH.SetAuditLogger(auditLogger)
	subSvc.SetAuditLogger(auditLogger)
	invoiceSvc.SetAuditLogger(auditLogger)

	// The billing engine.
	customerSvc.SetDunningPolicyReader(dunningStore)
	customerSvc.SetSubscriptionChecker(&subscriptionCheckerAdapter{subs: subStore})
	customerSvc.SetTaxFlusher(invoiceSvc)
	paymentReadiness := &paymentReadinessAdapter{customers: customerStore, pms: paymentMethodsSvc}
	engine := billing.NewEngine(subStore, usageStore, pricingSvc,
		&invoiceWriterAdapter{PostgresStore: invoiceStore}, creditSvc, settingsStore, paymentReadiness, stripeAdapter, clk, customerStore)
	engine.SetIntervalReader(subStore)
	engine.SetInvoiceGrace(o.invoiceGrace)
	engine.SetAuditLogger(auditLogger)
	engine.SetLiveBalanceReaders(creditSvc, subStore)
	customerUsageSvc.SetRater(engine)
	creditSvc.SetLiveBalanceReader(engine)
	costDashboardSvc.SetLiveBalanceReader(engine)
	// Usage reads through the server's clock: the request's instant when ctx
	// carries one, else a read of that clock.
	usageSvc.SetClock(clk)
	usageSvc.SetOpenPeriodReader(usageOpenPeriods{subs: subStore, pricing: pricingStore, thresholds: invoiceStore})
	subH.SetTenantLocator(engine)
	subH.SetNetTermsReader(engine)
	subH.SetInvoiceFinalizeRecorder(engine)
	subSvc.SetBiller(engine)
	subSvc.SetDueBiller(engine)
	subSvc.SetInvoiceGrace(o.invoiceGrace)
	// The relief for the unused part of an in_advance period, on a cancel or
	// a plan swap, is a credit note or a void (ADR-031, ADR-118).
	engine.SetInvoiceVoider(invoiceSvc)
	engine.SetCreditNoteAdjuster(creditNoteSvc)
	engine.SetCreditHeadroomReader(creditNoteSvc)
	// Commit relief counts usage not invoiced yet as credit already used.
	creditNoteSvc.SetUnbilledUsageReader(unbilledUsageAdapter{engine: engine})
	invoiceSvc.SetCustomerReader(customerStore)
	invoiceSvc.SetDunningRunReader(dunningStore)
	invoiceSvc.SetSubscriptionPauseReader(subStore)
	invoiceSvc.SetCustomerSearcher(customerStore)
	invoiceSvc.SetCommitGrantReader(creditStore)
	invoiceSvc.SetPaymentPageReader(invoiceStore)
	invoiceSvc.SetCardRefusalReader(invoiceStore)
	invoiceSvc.SetDisputeReader(invoiceStore)
	invoiceSvc.SetCreditApplier(creditSvc)
	invoiceSvc.SetCustomerCurrencyFacts(customerCurrencyFactsAdapter{profiles: customerStore, subs: subStore})
	// The no-payment-method notifier is always wired. Without
	// PAYMENT_UPDATE_URL or PAYMENT_UPDATE_RETURN_URL it refuses at send time,
	// as the warnings above say.
	noPMNotifier := &noPaymentMethodNotifierAdapter{
		email:                  outboxSender,
		customerEmail:          customerEmailAdapter,
		paymentUpdateURL:       paymentUpdateURL,
		paymentUpdateReturnURL: paymentUpdateReturnURL,
		tokenSvc:               tokenSvc,
		auditLogger:            auditLogger,
	}
	engine.SetNoPaymentMethodNotifier(noPMNotifier)
	invoiceH.SetNoPaymentMethodNotifier(noPMNotifier)
	invoiceH.SetCollector(engine)
	engine.SetDunningStarter(&dunningStarterAdapter{dunning: dunningSvc})
	engine.SetStripeReadiness(stripeClients)
	engine.SetInvoiceEmailer(invoiceH.Emailer())
	engine.SetScheduledCancelExecutor(subSvc)
	engine.SetDunningResolver(dunningSvc)
	engine.SetTaxProviderResolver(tax.NewResolver(stripeClients))
	engine.SetTaxCalculationStore(tax.NewPostgresStore(db))

	// Boot gates: refuse to start with a collaborator unwired, instead of
	// taking another money path at run time. A component without its audit
	// emitter or outbox writes nothing and reports nothing, so each is listed;
	// TestMustWired_CoversEveryAuditedComponent and
	// TestMustWiredOutbox_CoversEveryOutboxHolder pin the lists.
	engine.MustValidate()
	audit.MustWired(creditSvc, subSvc, invoiceSvc, creditNoteSvc,
		stripeAdapter, paymentMethodsSvc, engine, hostedInvoiceStripe,
		customerSvc, pricingSvc, recipeSvc, providerCostH, usageSvc,
		noPMNotifier, publicPaymentH)
	webhook.MustWiredOutbox(invoiceStore, creditStore, subStore, dunningStore, customerStore, paymentMethodsStore, creditNoteSvc)
	stripeAdapter.MustHaveAnomalyRecorder()

	// Tax goes through the engine, which owns the tax provider.
	invoiceSvc.SetTaxCommitter(engine)
	invoiceSvc.SetTaxReverser(engine)
	// A void or an uncollectible invoice reverses only the tax no credit note
	// already covers.
	invoiceSvc.SetCreditNoteTotaler(creditNoteSvc)
	invoiceSvc.SetCreditReverser(creditSvc)
	invoiceSvc.SetTaxRetrier(engine)
	invoiceSvc.SetPaymentMethodReader(paymentSetupStore)
	invoiceSvc.SetStripeChecker(stripeClients)
	creditNoteSvc.SetTaxReverser(engine)
	subH.SetProrationTaxApplier(&prorationTaxApplierAdapter{engine: engine})
	subH.SetCreditNoteIssuer(creditNoteSvc)

	billingH := billing.NewHandler(engine)
	createPreviewH := billing.NewCreatePreviewHandler(
		billing.NewPreviewService(engine, customerStore, subStore),
	)
	analyticsH := analytics.NewHandler(db)

	// The email blind index finds a customer by email without decrypting, for
	// bounce reports and the suppression gate. Without its key neither runs:
	// bounces are not recorded and bounced addresses are still sent to.
	if bidxKey := srv.EmailBlindIndexKey; bidxKey != "" {
		b, err := crypto.NewBlinder(bidxKey)
		if err != nil {
			slog.Error("invalid HARDCOUNT_EMAIL_BIDX_KEY — bounces are not recorded and bounced addresses are still sent to", "error", err)
		} else {
			customerStore.SetBlinder(b)
			bounceReporter := &bounceReporterAdapter{
				blinder: b,
				store:   customerStore,
				svc:     customerSvc,
			}
			emailSender.SetBounceReporter(bounceReporter)
			postmarkH.SetSuppressor(bounceReporter)
			suppressionChecker := &suppressionCheckerAdapter{blinder: b, store: customerStore}
			emailSender.SetSuppressionChecker(suppressionChecker)
			outboxSender.SetSuppressionChecker(suppressionChecker)
			slog.Info("email blind index enabled for bounce reporting + recipient suppression gate")
		}
	} else {
		slog.Warn("HARDCOUNT_EMAIL_BIDX_KEY not set — bounces are not recorded and bounced addresses are still sent to (no customer can be found by email)")
	}

	paymentMethodsH := paymentmethods.NewHandler(paymentMethodsSvc)
	paymentMethodsH.SetCustomerLookup(&pmCustomerLookupAdapter{store: customerStore})
	paymentMethodsH.SetEmailer(outboxSender)
	paymentMethodsH.SetEmailConfigured(emailSender.IsConfigured())
	paymentMethodsH.SetAuditLogger(auditLogger)
	stripeAdapter.SetPaymentMethodAttacher(paymentMethodsSvc)
	stripeAdapter.SetCustomerResolver(&stripeCustomerResolverAdapter{customers: customerStore})

	// Dashboard sign-in (ADR-011) and team members.
	sessionSvc := session.NewService(session.NewPostgresStore(db))
	userStore := user.NewPostgresStore(db)
	userSvc := user.NewService(userStore, nil, sessionSvc, auditLogger)
	dashboardBaseURL := srv.DashboardBaseURL
	if dashboardBaseURL == "" {
		slog.Warn("DASHBOARD_BASE_URL NOT SET — password-reset emails will NOT be sent (the reset link origin is never derived from request headers, to prevent host-header poisoning / token theft). Set this to your canonical dashboard URL (e.g. http://localhost:5173 in dev) to enable password-reset emails.")
	}
	dashboardAuthH := user.NewHandler(
		userSvc, session.DefaultCookieConfig(o.appEnv, srv.CookieDomain),
		&passwordResetEmailAdapter{sender: outboxSender},
		dashboardBaseURL,
		emailSender.IsConfigured(),
	)
	dashboardAuthH.SetAuditLogger(auditLogger)
	// The sign-in throttle (ADR-094) counts in Postgres, so it holds without
	// Redis. With no encryption key, in development, it uses a fixed key.
	throttleKey, err := user.ThrottleKey(srv.EncryptionKey)
	if err != nil {
		panic(err.Error()) // srv.Check refused a key ThrottleKey refuses
	}
	loginThrottle := user.NewLoginThrottle(db, throttleKey, session.DefaultCookieConfig(o.appEnv, srv.CookieDomain))
	dashboardAuthH.SetLoginThrottle(loginThrottle)
	membersSvc := dashmembers.NewService(
		dashmembers.NewPostgresStore(db),
		memberUserDirectoryAdapter{PostgresStore: userStore},
		sessionSvc,
		auditLogger,
		outboxSender,
		&memberTenantNamerAdapter{svc: tenantSvc},
		nil, // real clock
		dashboardBaseURL,
	)
	membersSvc.SetEmailConfigured(emailSender.IsConfigured())
	membersH := dashmembers.NewHandler(membersSvc, session.DefaultCookieConfig(o.appEnv, srv.CookieDomain))
	membersH.SetSignInRecorder(loginThrottle)

	emailSettingsH := &emailSettingsHandler{
		sender:             emailSender,
		links:              emailLinkEnvs(srv),
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

	// The handlers, and what was built after the first check. Most handlers
	// take their emitter as an interface, so a missing one compiles and boots.
	audit.MustWired(
		customerH, settingsH, invoiceH, subH, creditNoteH,
		dunningH, pricingH, tenantStripeH, webhookOutH,
		paymentMethodsH, dashboardAuthH, membersSvc,
		emailSettingsH,
	)

	s := &Server{
		BillingEngine:       engine,
		DunningSvc:          dunningSvc,
		SettingsStore:       settingsStore,
		WebhookOutSvc:       webhookOutSvc,
		OutboxStore:         outboxStore,
		EmailOutboxStore:    emailOutboxStore,
		EmailSender:         emailSender,
		EmailSettledChecker: invoiceStore,
		CreditSvc:           creditSvc,
		InvoiceSvc:          invoiceSvc,
		CreditNoteSvc:       creditNoteSvc,
		SubscriptionSvc:     subSvc,
		TokenSvc:            tokenSvc,
		PaymentReconciler:   paymentReconciler,
		LoginThrottle:       loginThrottle,
		RestoreCatchUp:      payment.NewCatchUp(stripeAdapter, stripeClients, tenantStripeSvc, customerStore, creditNoteStore),
	}

	// Rate limits. docs/settings.md gives each limit, and the runbook what the
	// general, public-page, ingest and sign-in limits do while Redis is down
	// (docs/ops/runbook/replica-will-not-start.md#redis-going-down-after-boot).
	// rdb is nil outside production, and in `restore catch-up`, which serves
	// no routes.
	failClosedInProd := mw.FailOpen
	if o.appEnv == "production" {
		failClosedInProd = mw.FailClosed
	}
	rateLimiter := mw.NewRateLimiter(rdb, "general", o.rateLimits.GeneralPerMinute, time.Minute, failClosedInProd)
	// Bootstrap and sign-in share the "auth" bucket of each address. It is not
	// "general": two rates on one GCRA key corrupt each other. Bootstrap fails
	// closed in production, as general does.
	bootstrapLimiter := mw.NewRateLimiter(rdb, "auth", o.rateLimits.AuthPerMinute, time.Minute, failClosedInProd)
	// Sign-in never fails closed, or a Redis outage would lock operators out;
	// it counts in this process instead (ADR-095).
	authLimiter := mw.NewRateLimiter(rdb, "auth", o.rateLimits.AuthPerMinute, time.Minute, mw.CountLocally)
	// Ingest never fails closed: LiteLLM does not retry, so a 429 is an event
	// never billed.
	ingestLimiter := mw.NewRateLimiter(rdb, "ingest", o.rateLimits.IngestPerSecond, time.Second, mw.FailOpen)
	// The public pages: 60 a minute per address, room for about ten customers
	// behind one NAT. The Redis key keeps its first name, "hosted_invoice".
	publicPageLimiter := mw.NewRateLimiter(rdb, "hosted_invoice", 60, time.Minute, failClosedInProd)
	// One setup-link email per customer a minute, against a double click. An
	// outage risks only a second email, so it fails open.
	setupLinkCooldown := mw.NewRateLimiter(rdb, "setup_link", 1, time.Minute, mw.FailOpen)
	paymentMethodsH.SetCooldown(setupLinkCooldown)

	s.db = db
	s.clk = clk
	s.invoiceStore = invoiceStore
	s.subStore = subStore
	s.usageStore = usageStore
	s.customerStore = customerStore
	s.tenantStripeH = tenantStripeH
	s.pricingSvc = pricingSvc
	s.customerH = customerH
	s.pricingH = pricingH
	s.subH = subH
	s.usageSvc = usageSvc
	s.usageH = usageH
	s.customerUsageH = customerUsageH
	s.providerCostH = providerCostH
	s.creditH = creditH
	s.creditNoteH = creditNoteH
	s.webhookOutH = webhookOutH
	s.auditLogger = auditLogger
	s.auditH = auditH
	s.authSvc = authSvc
	s.authH = authH
	s.settingsH = settingsH
	s.dunningH = dunningH
	s.recipeH = recipeH
	s.webhookH = webhookH
	s.invoiceH = invoiceH
	s.publicPaymentH = publicPaymentH
	s.hostedInvoiceH = hostedInvoiceH
	s.postmarkH = postmarkH
	s.setupLinksEmailable = setupLinksEmailable
	s.billingH = billingH
	s.createPreviewH = createPreviewH
	s.analyticsH = analyticsH
	s.paymentMethodsH = paymentMethodsH
	s.sessionSvc = sessionSvc
	s.userSvc = userSvc
	s.dashboardAuthH = dashboardAuthH
	s.membersSvc = membersSvc
	s.membersH = membersH
	s.emailSettingsH = emailSettingsH
	s.rateLimiter = rateLimiter
	s.bootstrapLimiter = bootstrapLimiter
	s.authLimiter = authLimiter
	s.ingestLimiter = ingestLimiter
	s.publicPageLimiter = publicPageLimiter
	s.opts = o
	s.settings = srv
	return s
}
