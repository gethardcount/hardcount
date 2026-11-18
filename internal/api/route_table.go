package api

import (
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/request"
)

// The route table has one entry per route the router serves: how its time is
// decided, where its audit row comes from, and whether its answer shows a
// secret once (docs/architecture.md#the-route-table).
// TestRouteTable_MatchesLiveRoutes fails on a route with no entry, an entry
// with no route, and a missing or stray audit.

// routeKey is (HTTP method, canonical chi route pattern).
type routeKey struct {
	Method  string
	Pattern string
}

// routeDecl is one route's entry. A clock constructor below (realTime,
// ofCustomer, ofSubscription, ofInvoice, ofCreditNote, ofDunningRun,
// decidedIn) makes it from who may call the route and how its time is
// decided; withAudit adds its audit, and showsSecretOnce marks its answer.
type routeDecl struct {
	who   who
	clock clockDecl
	// audit is zero on a route that neither changes state nor exports.
	audit    auditDecl
	showOnce bool
}

// withAudit is d with its audit declaration.
func (d routeDecl) withAudit(a auditDecl) routeDecl {
	d.audit = a
	return d
}

// showsSecretOnce is d with an answer that carries a secret shown only in
// that answer: a field marked `x-show-once: true` in api/openapi.yaml, which
// TestOpenAPISpec_ShowOnceSecretsAreNeverKept keeps in step with the table.
// /v1/bootstrap's fields are marked but its entry is not: it is mounted
// outside the idempotency layer, and the test fails if it moves inside.
func (d routeDecl) showsSecretOnce() routeDecl {
	d.showOnce = true
	return d
}

// keepsNoKey reports whether Idempotency holds no key for r: a request that
// matched no route, where nothing can run (a 404 or a 405), and one whose
// answer carries a secret shown once, which must never be kept.
func keepsNoKey(r *http.Request) bool {
	return request.Of(r.Context()).Route().Pattern == "" || answersWithShowOnceSecret(r)
}

// answersWithShowOnceSecret reports whether r's route answers with a secret
// shown once, so the idempotency layer leaves it alone.
func answersWithShowOnceSecret(r *http.Request) bool {
	return routeTable[routeKeyOf(r.Context())].showOnce
}

// clockSource is how a test-mode request's time is decided; clockDoor has the
// rules.
type clockSource int

const (
	// clockRealTime: the request names no record on a clock, or reads many
	// (a tenant-wide list or export), each shown by its stored dates.
	clockRealTime clockSource = iota + 1
	// clockOfCustomer, clockOfSubscription and clockOfInvoice: the path
	// parameter names the record. Its clock's time, or real time when it is
	// on none.
	clockOfCustomer
	clockOfSubscription
	clockOfInvoice
	// clockOfCreditNote and clockOfDunningRun: the path parameter names a
	// record of an invoice; that invoice's clock decides.
	clockOfCreditNote
	clockOfDunningRun
	// clockDecidedIn: the record is named in the body or by a token, or the
	// request touches many records each on its own clock. The named function
	// decides, after it reads the record.
	clockDecidedIn
)

// clockDecl is how one route's time is decided.
type clockDecl struct {
	Source clockSource
	// Param is the path parameter that names the record, for the clockOf
	// sources.
	Param string
	// In is the function that decides, as package.Type.Method, for
	// clockDecidedIn.
	In string
}

// The clock constructors: each makes a route's entry from who may call it and
// how its time is decided.
func realTime(w who) routeDecl { return clocked(w, clockDecl{Source: clockRealTime}) }
func ofCustomer(w who, param string) routeDecl {
	return clocked(w, clockDecl{Source: clockOfCustomer, Param: param})
}
func ofSubscription(w who) routeDecl {
	return clocked(w, clockDecl{Source: clockOfSubscription, Param: "id"})
}
func ofInvoice(w who) routeDecl { return clocked(w, clockDecl{Source: clockOfInvoice, Param: "id"}) }
func ofCreditNote(w who) routeDecl {
	return clocked(w, clockDecl{Source: clockOfCreditNote, Param: "id"})
}
func ofDunningRun(w who) routeDecl {
	return clocked(w, clockDecl{Source: clockOfDunningRun, Param: "id"})
}
func decidedIn(w who, function string) routeDecl {
	return clocked(w, clockDecl{Source: clockDecidedIn, In: function})
}

// clocked is a route's entry with who may call it and its clock declared.
func clocked(w who, c clockDecl) routeDecl { return routeDecl{who: w, clock: c} }

// who is who may call a route. Behind authentication, its need is what
// auth.RequirePermission checks; noCaller marks a route outside it.
type who struct {
	need     auth.Need
	noCaller bool
}

func needs(p auth.Permission) who { return who{need: auth.Need{Perm: p}} }
func sessionOnly(task string) who { return who{need: auth.Need{Session: task}} }

// The values an entry names, one per permission and kind of caller.
var (
	// noCaller: the route takes no API key or session. Its handler checks its
	// own credential, if any: a token, a signature or a password.
	noCaller = who{noCaller: true}
	// anyCaller: any API key or dashboard session.
	// TestRouteTable_AnyCallerStaysSmall lists the routes that take it.
	anyCaller = who{need: auth.Need{Any: true}}

	membersSession  = sessionOnly("manage team membership")
	settingsSession = sessionOnly("change account settings")

	customerRead      = needs(auth.PermCustomerRead)
	customerWrite     = needs(auth.PermCustomerWrite)
	pricingRead       = needs(auth.PermPricingRead)
	pricingWrite      = needs(auth.PermPricingWrite)
	subscriptionRead  = needs(auth.PermSubscriptionRead)
	subscriptionWrite = needs(auth.PermSubscriptionWrite)
	usageRead         = needs(auth.PermUsageRead)
	usageWrite        = needs(auth.PermUsageWrite)
	usageIngest       = needs(auth.PermUsageIngest)
	invoiceRead       = needs(auth.PermInvoiceRead)
	invoiceWrite      = needs(auth.PermInvoiceWrite)
	dunningRead       = needs(auth.PermDunningRead)
	dunningWrite      = needs(auth.PermDunningWrite)
	apikeyRead        = needs(auth.PermAPIKeyRead)
	apikeyWrite       = needs(auth.PermAPIKeyWrite)
	testclockWrite    = needs(auth.PermTestClockWrite)
)

// routeNeeds gives auth.RequirePermission each request's need, from its
// route's entry. A request with no entry is one no route serves
// (mustListEveryRoute).
type routeNeeds struct{}

func (routeNeeds) Need(r *http.Request) (auth.Need, bool) {
	d, ok := routeTable[routeKeyOf(r.Context())]
	return d.who.need, ok
}

// auditCoverage is where a route's audit row comes from. Every route that can
// change state declares one, and so does each GET under /v1/exports: a bulk
// export is the one read that is audited, and it writes its row before the
// first byte, or fails (ADR-090 §7). The runtime detector (mw.AuditCoverage)
// reports a state-changing 2xx that wrote no row and is not exempt.
type auditCoverage int

const (
	// auditExplicit: the route writes its own audit row.
	auditExplicit auditCoverage = iota + 1
	// auditExempt: the route writes no audit row, on purpose.
	auditExempt
)

// exemptReason is why a route writes no audit row. The list is closed: a new
// reason widens what may go unaudited, so it is added here, in review.
type exemptReason string

const (
	// reasonNonMutatingPreview: a POST only because its input does not fit a
	// query string; it computes a result and writes nothing.
	reasonNonMutatingPreview exemptReason = "non_mutating_preview"
	// reasonMachineIngest: high-volume usage ingest, where the events table is
	// the record.
	reasonMachineIngest exemptReason = "machine_ingest"
	// reasonSystemEndpoint: no tenant to scope a row to.
	reasonSystemEndpoint exemptReason = "system_endpoint"
	// reasonWebhookOwned: an inbound provider webhook, with no operator to
	// attribute; the entry's note says what records its effects.
	reasonWebhookOwned exemptReason = "webhook_owned"
)

// auditDecl is a route's audit declaration.
type auditDecl struct {
	Coverage auditCoverage
	// Reason is set when, and only when, Coverage == auditExempt.
	Reason exemptReason
	// Note names the function that writes the row (explicit), or says why
	// there is none and records any accepted loss (exempt).
	Note string
}

// explicit is an audit row the route writes itself. note names the function
// that writes it, or that calls what does, then how: (LogInTx) is inside the
// change's transaction, so both commit or neither does; (Log) is after the
// change committed, so a failed write loses the row.
func explicit(note string) auditDecl {
	return auditDecl{Coverage: auditExplicit, Note: note}
}

// exempt is no audit row, on purpose.
func exempt(reason exemptReason, note string) auditDecl {
	return auditDecl{Coverage: auditExempt, Reason: reason, Note: note}
}

// auditRoute is the request's route for the runtime detector: its pattern,
// which labels the detector's counter, and whether the table declares it
// exempt. A route not in the table is not exempt, so a gap is reported.
func auditRoute(r *http.Request) (pattern string, exempt bool) {
	key := routeKeyOf(r.Context())
	return key.Pattern, routeTable[key].audit.Coverage == auditExempt
}

// canonicalRoute replaces "/*/" and trims a trailing slash as chi's
// RoutePattern does, and makes an empty pattern "/", so chi.Walk's patterns
// (the tests) and Find's (routeFinder) meet on one key. Walk leaves a "/*/"
// where a Mount("/") sits inside a Route ("/v1/auth/*/login"), and keeps a
// subrouter's trailing slash ("/v1/customers/").
func canonicalRoute(p string) string {
	for strings.Contains(p, "/*/") {
		p = strings.ReplaceAll(p, "/*/", "/")
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "//")
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}

// routeTable is the table, keyed by canonical method and pattern.
var routeTable = map[routeKey]routeDecl{
	// --- system ---------------------------------------------------------
	{"GET", "/health"}:       realTime(noCaller),
	{"GET", "/health/ready"}: realTime(noCaller),
	// chi expands r.Handle("/metrics") to every method. Each is declared: a
	// walk that skipped them would also skip a future route registered so.
	{"GET", "/metrics"}:     realTime(noCaller),
	{"HEAD", "/metrics"}:    realTime(noCaller),
	{"POST", "/metrics"}:    realTime(noCaller).withAudit(exempt(reasonSystemEndpoint, "The Prometheus scrape: no tenant, no operator action.")),
	{"PUT", "/metrics"}:     realTime(noCaller).withAudit(exempt(reasonSystemEndpoint, "As POST /metrics.")),
	{"PATCH", "/metrics"}:   realTime(noCaller).withAudit(exempt(reasonSystemEndpoint, "As POST /metrics.")),
	{"DELETE", "/metrics"}:  realTime(noCaller).withAudit(exempt(reasonSystemEndpoint, "As POST /metrics.")),
	{"CONNECT", "/metrics"}: realTime(noCaller),
	{"OPTIONS", "/metrics"}: realTime(noCaller),
	{"TRACE", "/metrics"}:   realTime(noCaller),
	{"QUERY", "/metrics"}:   realTime(noCaller),

	// --- sign-in and setup: users and tenants are on no clock ------------
	{"POST", "/v1/bootstrap"}:                   realTime(noCaller).withAudit(explicit("tenant.RunBootstrap (on its own transaction). Never exempt: it mints a live secret key.")),
	{"POST", "/v1/auth/login"}:                  realTime(noCaller).withAudit(explicit("user.Service.Login (LogInTx)")),
	{"POST", "/v1/auth/logout"}:                 realTime(noCaller).withAudit(explicit("user.Service.Logout (LogInTx)")),
	{"POST", "/v1/auth/mode"}:                   realTime(noCaller).withAudit(explicit("user.Service.SetMode (LogInTx)")),
	{"GET", "/v1/auth/password-reset/check"}:    realTime(noCaller),
	{"POST", "/v1/auth/password-reset/confirm"}: realTime(noCaller).withAudit(explicit("user.Service.ConfirmReset (LogInTx)")),
	{"POST", "/v1/auth/password-reset/request"}: realTime(noCaller).withAudit(explicit("user.Handler.requestPasswordReset (Log)")),
	{"GET", "/v1/auth/invite/{token}"}:          realTime(noCaller),
	{"POST", "/v1/auth/accept-invite"}:          realTime(noCaller).withAudit(explicit("dashmembers.Service.AcceptInvite (LogInTx)")),
	{"GET", "/v1/whoami"}:                       realTime(anyCaller),
	{"GET", "/v1/members"}:                      realTime(membersSession),
	{"POST", "/v1/members/invite"}:              realTime(membersSession).withAudit(explicit("dashmembers.Service.Invite (LogInTx)")),
	{"DELETE", "/v1/members/invitations/{id}"}:  realTime(membersSession).withAudit(explicit("dashmembers.Service.Revoke (LogInTx)")),
	{"DELETE", "/v1/members/{userID}"}:          realTime(membersSession).withAudit(explicit("dashmembers.Service.RemoveMember (LogInTx)")),
	{"GET", "/v1/api-keys"}:                     realTime(apikeyWrite),
	{"POST", "/v1/api-keys"}:                    realTime(apikeyWrite).withAudit(explicit("auth.Service.CreateKey (LogInTx)")).showsSecretOnce(),
	{"DELETE", "/v1/api-keys/{id}"}:             realTime(apikeyWrite).withAudit(explicit("auth.Service.RevokeKey (LogInTx)")),
	{"POST", "/v1/api-keys/{id}/rotate"}:        realTime(apikeyWrite).withAudit(explicit("auth.Service.RotateKey (LogInTx)")).showsSecretOnce(),

	// --- inbound webhooks -----------------------------------------------
	// The endpoint names the tenant and mode; the record is known only once
	// the event is read.
	{"POST", "/v1/webhooks/stripe/{endpoint_id}"}: decidedIn(noCaller, "payment.Stripe.HandleWebhook").withAudit(exempt(reasonWebhookOwned, "Its handlers audit a card saved at checkout, a refund's status and a refund made in Stripe, each in its own transaction. Accepted loss: a card settlement or dispute writes no audit_log row; the invoice (a settlement) or its invoice_disputes row (a dispute), the outbox events and stripe_webhook_events record it. Close it with a row on the settle transaction when an auditor asks audit_log for settlement evidence.")),
	// email reports; reads no clock
	{"POST", "/v1/webhooks/postmark"}: realTime(noCaller).withAudit(exempt(reasonWebhookOwned, "Postmark's delivery reports, behind Basic Auth; the tenant comes from the stored outbox row (ADR-098). Accepted loss: their effects are not audit_log rows; the outbox row's delivery state, the customer's email status and the outbox events record them.")),

	// --- public pages: the token names the record, and the mode is not
	// known until it is read ----------------------------------------------
	{"GET", "/v1/public/invoices/{token}"}:                  decidedIn(noCaller, "hostedinvoice.Handler.resolveInvoice"),
	{"POST", "/v1/public/invoices/{token}/checkout"}:        decidedIn(noCaller, "hostedinvoice.Handler.resolveInvoice").withAudit(explicit("api.hostedInvoiceStripeAdapter.CreateInvoicePaymentSession (LogInTx)")),
	{"GET", "/v1/public/invoices/{token}/pdf"}:              decidedIn(noCaller, "hostedinvoice.Handler.resolveInvoice"),
	{"GET", "/v1/public/payment-updates/{token}"}:           realTime(noCaller),
	{"POST", "/v1/public/payment-updates/{token}/checkout"}: realTime(noCaller).withAudit(explicit("payment.PublicPaymentHandler.createCheckoutSession (LogInTx)")),
	{"GET", "/v1/public/cost-dashboard/{token}"}:            decidedIn(noCaller, "usage.CostDashboardAssembler.GetByToken"),

	// --- tenant-wide reads and streams: each row by its stored dates ------
	{"GET", "/v1/webhook_events/stream"}:     realTime(apikeyRead),
	{"GET", "/v1/exports/audit-log.csv"}:     realTime(apikeyRead).withAudit(explicit("api.exportsHandler.exportAuditLog (auditExport, before the first byte)")),
	{"GET", "/v1/exports/customers.csv"}:     realTime(customerRead).withAudit(explicit("api.exportsHandler.exportCustomers (auditExport, before the first byte)")),
	{"GET", "/v1/exports/invoices.csv"}:      realTime(invoiceRead).withAudit(explicit("api.exportsHandler.exportInvoices (auditExport, before the first byte)")),
	{"GET", "/v1/exports/subscriptions.csv"}: realTime(subscriptionRead).withAudit(explicit("api.exportsHandler.exportSubscriptions (auditExport, before the first byte)")),
	{"GET", "/v1/exports/usage-events.csv"}:  realTime(usageRead).withAudit(explicit("api.exportsHandler.exportUsageEvents (auditExport, before the first byte)")),
	{"GET", "/v1/analytics/mrr-movement"}:    realTime(invoiceRead),
	{"GET", "/v1/analytics/overview"}:        realTime(invoiceRead),
	{"GET", "/v1/analytics/revenue-chart"}:   realTime(invoiceRead),
	{"GET", "/v1/analytics/usage"}:           realTime(invoiceRead),
	{"GET", "/v1/audit-log"}:                 realTime(apikeyRead),
	{"GET", "/v1/audit-log/filters"}:         realTime(apikeyRead),

	// --- pricing, settings and tooling: on no clock ----------------------
	{"GET", "/v1/meters"}:            realTime(pricingRead),
	{"POST", "/v1/meters"}:           realTime(pricingWrite).withAudit(explicit("pricing.Handler.createMeter (Log)")),
	{"GET", "/v1/meters/{id}"}:       realTime(pricingRead),
	{"PATCH", "/v1/meters/{id}"}:     realTime(pricingWrite).withAudit(explicit("pricing.Service.UpdateMeter (LogInTx)")),
	{"GET", "/v1/plans"}:             realTime(pricingRead),
	{"POST", "/v1/plans"}:            realTime(pricingWrite).withAudit(explicit("pricing.Handler.createPlan (Log)")),
	{"GET", "/v1/plans/{id}"}:        realTime(pricingRead),
	{"PATCH", "/v1/plans/{id}"}:      realTime(pricingWrite).withAudit(explicit("pricing.Handler.updatePlan (Log)")),
	{"GET", "/v1/plans/{id}/prices"}: realTime(pricingRead),
	// A price edit reaches subscriptions on many clocks, so no one clock
	// decides it.
	{"POST", "/v1/plans/{id}/prices"}:              realTime(pricingWrite).withAudit(explicit("pricing.Service.AddPrice (LogInTx)")),
	{"PATCH", "/v1/plans/{id}/prices/{price_id}"}:  realTime(pricingWrite).withAudit(explicit("pricing.Service.EditPrice (LogInTx)")),
	{"DELETE", "/v1/plans/{id}/prices/{price_id}"}: realTime(pricingWrite).withAudit(explicit("pricing.Service.RemovePrice (LogInTx)")),
	// An override reads its customer's clock in its own SQL.
	{"GET", "/v1/price-overrides"}:            realTime(pricingWrite),
	{"POST", "/v1/price-overrides"}:           realTime(pricingWrite).withAudit(explicit("pricing.Handler.createOverride (Log)")),
	{"DELETE", "/v1/price-overrides/{id}"}:    realTime(pricingWrite).withAudit(explicit("pricing.Handler.deleteOverride (Log)")),
	{"GET", "/v1/provider-costs"}:             realTime(usageRead),
	{"PUT", "/v1/provider-costs"}:             realTime(usageWrite).withAudit(explicit("usage.ProviderCostHandler.upsert (LogInTx)")),
	{"DELETE", "/v1/provider-costs/{id}"}:     realTime(usageWrite).withAudit(explicit("usage.ProviderCostHandler.delete (LogInTx)")),
	{"GET", "/v1/recipes"}:                    realTime(pricingWrite),
	{"GET", "/v1/recipes/instances"}:          realTime(pricingWrite),
	{"GET", "/v1/recipes/{key}"}:              realTime(pricingWrite),
	{"POST", "/v1/recipes/{key}/instantiate"}: realTime(pricingWrite).withAudit(explicit("recipe.Service.Instantiate (LogInTx)")),
	{"POST", "/v1/recipes/{key}/preview"}:     realTime(pricingWrite).withAudit(exempt(reasonNonMutatingPreview, "Renders what instantiate would create and writes nothing (recipe.Handler calls audit.MarkSkip).")),
	{"GET", "/v1/settings"}:                   realTime(apikeyWrite),
	{"PUT", "/v1/settings"}:                   realTime(settingsSession).withAudit(explicit("tenant.SettingsHandler.upsert (Log)")),
	{"GET", "/v1/settings/email"}:             realTime(apikeyWrite),
	{"POST", "/v1/settings/email/test"}:       realTime(apikeyWrite).withAudit(explicit("api.emailSettingsHandler.sendTest (Log)")),
	{"GET", "/v1/settings/stripe"}:            realTime(apikeyWrite),
	// Connecting Stripe retries every invoice stuck on the missing key, each
	// on its own clock.
	{"POST", "/v1/settings/stripe"}:                                                 decidedIn(apikeyWrite, "invoice.Service.RetryProviderConfigErrors").withAudit(explicit("tenantstripe.Handler.connect (Log)")),
	{"DELETE", "/v1/settings/stripe/{mode}"}:                                        realTime(apikeyWrite).withAudit(explicit("tenantstripe.Handler.delete (Log)")),
	{"PATCH", "/v1/settings/stripe/{mode}/webhook"}:                                 realTime(apikeyWrite).withAudit(explicit("tenantstripe.Handler.setWebhook (Log)")),
	{"GET", "/v1/dunning/policies"}:                                                 realTime(dunningRead),
	{"POST", "/v1/dunning/policies"}:                                                realTime(dunningWrite).withAudit(explicit("dunning.Handler.createPolicy (Log)")),
	{"GET", "/v1/dunning/policies/{id}"}:                                            realTime(dunningRead),
	{"PATCH", "/v1/dunning/policies/{id}"}:                                          realTime(dunningWrite).withAudit(explicit("dunning.Handler.updatePolicy (Log)")),
	{"DELETE", "/v1/dunning/policies/{id}"}:                                         realTime(dunningWrite).withAudit(explicit("dunning.Handler.deletePolicy (Log)")),
	{"POST", "/v1/dunning/policies/{id}/set-default"}:                               realTime(dunningWrite).withAudit(explicit("dunning.Handler.setDefaultPolicy (Log)")),
	{"GET", "/v1/dunning/stats"}:                                                    realTime(dunningRead),
	{"GET", "/v1/webhook-endpoints/endpoints"}:                                      realTime(apikeyWrite),
	{"POST", "/v1/webhook-endpoints/endpoints"}:                                     realTime(apikeyWrite).withAudit(explicit("webhook.Handler.createEndpoint (Log)")).showsSecretOnce(),
	{"GET", "/v1/webhook-endpoints/endpoints/stats"}:                                realTime(apikeyWrite),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}"}:                                 realTime(apikeyWrite),
	{"PATCH", "/v1/webhook-endpoints/endpoints/{id}"}:                               realTime(apikeyWrite).withAudit(explicit("webhook.Handler.updateEndpoint (Log)")),
	{"DELETE", "/v1/webhook-endpoints/endpoints/{id}"}:                              realTime(apikeyWrite).withAudit(explicit("webhook.Handler.deleteEndpoint (Log)")),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}/deliveries"}:                      realTime(apikeyWrite),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/deliveries/{deliveryID}/replay"}: realTime(apikeyWrite).withAudit(explicit("webhook.Handler.replayDelivery (Log)")),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/rotate-secret"}:                  realTime(apikeyWrite).withAudit(explicit("webhook.Handler.rotateSecret (Log)")).showsSecretOnce(),
	{"GET", "/v1/webhook-endpoints/events"}:                                         realTime(apikeyWrite),
	{"GET", "/v1/webhook_events/{id}/deliveries"}:                                   realTime(apikeyRead),
	{"POST", "/v1/webhook_events/{id}/replay"}:                                      realTime(apikeyRead).withAudit(explicit("webhook.Handler.replayEventV2 (Log)")),
	// A clock itself: the advance decides each pass's time per clock.
	{"GET", "/v1/test-clocks"}:                     realTime(testclockWrite),
	{"POST", "/v1/test-clocks"}:                    realTime(testclockWrite).withAudit(explicit("testclock.Service.Create (LogInTx)")),
	{"GET", "/v1/test-clocks/{id}"}:                realTime(testclockWrite),
	{"DELETE", "/v1/test-clocks/{id}"}:             realTime(testclockWrite).withAudit(explicit("testclock.Service.Delete (LogInTx)")),
	{"POST", "/v1/test-clocks/{id}/advance"}:       realTime(testclockWrite).withAudit(explicit("testclock.Service.Advance (LogInTx)")),
	{"POST", "/v1/test-clocks/{id}/retry-advance"}: realTime(testclockWrite).withAudit(explicit("testclock.Service.RetryAdvance (LogInTx)")),
	{"GET", "/v1/test-clocks/{id}/customers"}:      realTime(testclockWrite),
	// Bills the tenant's due subscriptions, which never include a clock's.
	{"POST", "/v1/billing/run"}: realTime(invoiceWrite).withAudit(explicit("billing.Handler.triggerCycle (Log)")),

	// --- customers --------------------------------------------------------
	{"GET", "/v1/customers"}: realTime(customerRead),
	// the body names the clock
	{"POST", "/v1/customers"}:                                                decidedIn(customerWrite, "customer.Service.Create").withAudit(explicit("customer.Service.Create (LogInTx)")),
	{"GET", "/v1/customers/{id}"}:                                            ofCustomer(customerRead, "id"),
	{"PATCH", "/v1/customers/{id}"}:                                          ofCustomer(customerWrite, "id").withAudit(explicit("customer.Handler.update (Log)")),
	{"GET", "/v1/customers/{id}/billing-profile"}:                            ofCustomer(customerRead, "id"),
	{"PUT", "/v1/customers/{id}/billing-profile"}:                            ofCustomer(customerWrite, "id").withAudit(explicit("customer.Handler.upsertBillingProfile (Log)")),
	{"GET", "/v1/customers/{id}/margin"}:                                     ofCustomer(usageRead, "id"),
	{"POST", "/v1/customers/{id}/rotate-cost-dashboard-token"}:               ofCustomer(customerWrite, "id").withAudit(explicit("customer.Handler.rotateCostDashboardToken (Log)")).showsSecretOnce(),
	{"GET", "/v1/customers/{id}/sent-emails"}:                                ofCustomer(customerRead, "id"),
	{"GET", "/v1/customers/{id}/usage"}:                                      ofCustomer(usageRead, "id"),
	{"GET", "/v1/customers/{customer_id}/overview"}:                          ofCustomer(customerRead, "customer_id"),
	{"GET", "/v1/customers/{customer_id}/payment-methods"}:                   ofCustomer(customerRead, "customer_id"),
	{"POST", "/v1/customers/{customer_id}/payment-methods/send-setup-email"}: ofCustomer(customerWrite, "customer_id").withAudit(explicit("paymentmethods.Handler.operatorSendSetupEmail (Log)")),
	{"POST", "/v1/customers/{customer_id}/payment-methods/setup-session"}:    ofCustomer(customerWrite, "customer_id").withAudit(explicit("paymentmethods.Service.CreateSetupSession (Log)")).showsSecretOnce(),
	{"DELETE", "/v1/customers/{customer_id}/payment-methods/{pmID}"}:         ofCustomer(customerWrite, "customer_id").withAudit(explicit("paymentmethods.Service.Detach (Log)")),
	{"POST", "/v1/customers/{customer_id}/payment-methods/{pmID}/default"}:   ofCustomer(customerWrite, "customer_id").withAudit(explicit("paymentmethods.Service.SetDefault (Log)")),

	// --- subscriptions ----------------------------------------------------
	{"GET", "/v1/subscriptions"}: realTime(subscriptionRead),
	// the body names the customer
	{"POST", "/v1/subscriptions"}:                                      decidedIn(subscriptionWrite, "subscription.Handler.create").withAudit(explicit("subscription.Handler.create (Log)")),
	{"GET", "/v1/subscriptions/{id}"}:                                  ofSubscription(subscriptionRead),
	{"PUT", "/v1/subscriptions/{id}/billing-thresholds"}:               ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.setBillingThresholds (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/billing-thresholds"}:            ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.clearBillingThresholds (Log)")),
	{"POST", "/v1/subscriptions/{id}/cancel"}:                          ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Service.Cancel (LogInTx)")),
	{"POST", "/v1/subscriptions/{id}/end-trial"}:                       ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.endTrial (Log)")),
	{"POST", "/v1/subscriptions/{id}/extend-trial"}:                    ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.extendTrial (Log)")),
	{"POST", "/v1/subscriptions/{id}/items"}:                           ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.addItem (Log)")),
	{"PATCH", "/v1/subscriptions/{id}/items/{itemID}"}:                 ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.updateItem (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}"}:                ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.removeItem (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}/pending-change"}: ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.cancelPendingItemChange (Log)")),
	{"PUT", "/v1/subscriptions/{id}/pause-collection"}:                 ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Service.PauseCollection (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/pause-collection"}:              ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Service.ResumeCollection (Log)")),
	{"POST", "/v1/subscriptions/{id}/retry-billing"}:                   ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Service.RetryBilling (LogInTx)")),
	{"POST", "/v1/subscriptions/{id}/schedule-cancel"}:                 ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.scheduleCancel (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/scheduled-cancel"}:              ofSubscription(subscriptionWrite).withAudit(explicit("subscription.Handler.clearScheduledCancel (Log)")),
	{"GET", "/v1/subscriptions/{id}/timeline"}:                         ofSubscription(subscriptionRead),

	// --- usage --------------------------------------------------------------
	// The body names the customer by its external id; a batch or a LiteLLM
	// callback names many, each decided per event.
	{"POST", "/v1/usage-events"}:               decidedIn(usageIngest, "usage.Service.Ingest").withAudit(exempt(reasonMachineIngest, "usage_events is the record, and a row per call would double the writes of the busiest path. Accepted loss: which key sent an event, since usage_events has no key_id. Close it with that column at the first billing dispute that turns on which key sent usage.")),
	{"POST", "/v1/usage-events/batch"}:         decidedIn(usageIngest, "usage.Service.BatchIngest").withAudit(exempt(reasonMachineIngest, "The batch form of the same ingest; the same accepted loss.")),
	{"POST", "/v1/integrations/litellm/spend"}: decidedIn(usageIngest, "usage.Service.IngestCall").withAudit(exempt(reasonMachineIngest, "LiteLLM's spend callback, which writes usage events like the ingest above; the same accepted loss.")),
	// Backfill is audited, unlike the ingest above: an operator's backdated
	// usage changes what a customer is billed.
	{"POST", "/v1/usage-events/backfill"}: decidedIn(usageWrite, "usage.Service.Backfill").withAudit(explicit("usage.Service.Backfill (in the change's transaction)")),
	// Held usage (ADR-122) is what the LiteLLM callback could not ingest.
	// Releasing it changes what a customer is billed; discarding it means it
	// never bills.
	{"POST", "/v1/usage-events/held/release"}: decidedIn(usageWrite, "usage.Service.Release").withAudit(explicit("usage.Service.Release (in the change's transaction)")),
	// Discard reads no clock.
	{"POST", "/v1/usage-events/held/discard"}: realTime(usageWrite).withAudit(explicit("usage.Service.Discard (in the change's transaction)")),
	{"GET", "/v1/usage-events"}:               realTime(usageRead),
	{"GET", "/v1/usage-events/aggregate"}:     realTime(usageRead),
	{"GET", "/v1/usage-events/held"}:          realTime(usageRead),

	// --- invoices -----------------------------------------------------------
	{"GET", "/v1/invoices"}: realTime(invoiceRead),
	// the body names the customer
	{"POST", "/v1/invoices"}: decidedIn(invoiceWrite, "invoice.Service.Create").withAudit(explicit("invoice.Service.Create (LogInTx)")),
	// the body names the customer
	{"POST", "/v1/invoices/create_preview"}:                                 decidedIn(invoiceRead, "billing.PreviewService.CreatePreview").withAudit(exempt(reasonNonMutatingPreview, "Computes an estimate and writes nothing (billing.CreatePreviewHandler calls audit.MarkSkip).")),
	{"GET", "/v1/invoices/{id}"}:                                            ofInvoice(invoiceRead),
	{"POST", "/v1/invoices/{id}/collect"}:                                   ofInvoice(invoiceWrite).withAudit(explicit("invoice.Handler.collectPayment (Log)")),
	{"GET", "/v1/invoices/{id}/disputes"}:                                   ofInvoice(invoiceRead),
	{"POST", "/v1/invoices/{id}/finalize"}:                                  ofInvoice(invoiceWrite).withAudit(explicit("invoice.Service.Finalize (LogInTx)")),
	{"POST", "/v1/invoices/{id}/line-items"}:                                ofInvoice(invoiceWrite).withAudit(explicit("invoice.Service.AddLineItem (LogInTx)")),
	{"POST", "/v1/invoices/{id}/mark-uncollectible"}:                        ofInvoice(invoiceWrite).withAudit(explicit("invoice.Service.MarkUncollectible (Log)")),
	{"GET", "/v1/invoices/{id}/payment-timeline"}:                           ofInvoice(invoiceRead),
	{"GET", "/v1/invoices/{id}/pdf"}:                                        ofInvoice(invoiceRead),
	{"POST", "/v1/invoices/{id}/record-payment"}:                            ofInvoice(invoiceWrite).withAudit(explicit("invoice.Service.RecordOfflinePayment (Log)")),
	{"POST", "/v1/invoices/{id}/refund"}:                                    ofInvoice(invoiceWrite).withAudit(explicit("invoice.Handler.refund (Log)")),
	{"POST", "/v1/invoices/{id}/resend-setup-link"}:                         ofInvoice(invoiceWrite).withAudit(explicit("invoice.Handler.resendSetupLink (Log)")),
	{"POST", "/v1/invoices/{id}/retry-tax"}:                                 ofInvoice(invoiceWrite).withAudit(explicit("invoice.Handler.retryTax (Log)")),
	{"POST", "/v1/invoices/{id}/rotate-public-token"}:                       ofInvoice(invoiceWrite).withAudit(explicit("invoice.Handler.rotatePublicToken (Log)")),
	{"POST", "/v1/invoices/{id}/send"}:                                      ofInvoice(invoiceWrite).withAudit(explicit("invoice.Handler.sendEmail (Log)")),
	{"GET", "/v1/invoices/{id}/stripe-refunds"}:                             ofInvoice(invoiceRead),
	{"POST", "/v1/invoices/{id}/stripe-refunds/{stripe_refund_id}/dismiss"}: ofInvoice(invoiceWrite).withAudit(explicit("creditnote.Service.DismissStripeRefund (LogInTx)")),
	{"POST", "/v1/invoices/{id}/void"}:                                      ofInvoice(invoiceWrite).withAudit(explicit("invoice.Service.Void (LogInTx)")),

	// --- credit notes -------------------------------------------------------
	{"GET", "/v1/credit-notes"}:       realTime(invoiceWrite),
	{"GET", "/v1/credit-notes/stats"}: realTime(invoiceWrite),
	// the body names the invoice
	{"POST", "/v1/credit-notes"}:                           decidedIn(invoiceWrite, "creditnote.Handler.create").withAudit(explicit("creditnote.Service.Create (LogInTx)")),
	{"GET", "/v1/credit-notes/{id}"}:                       ofCreditNote(invoiceWrite),
	{"POST", "/v1/credit-notes/{id}/attach-stripe-refund"}: ofCreditNote(invoiceWrite).withAudit(explicit("creditnote.Service.AttachStripeRefund (LogInTx)")),
	{"POST", "/v1/credit-notes/{id}/issue"}:                ofCreditNote(invoiceWrite).withAudit(explicit("creditnote.Service.Issue (LogInTx)")),
	{"GET", "/v1/credit-notes/{id}/pdf"}:                   ofCreditNote(invoiceWrite),
	{"POST", "/v1/credit-notes/{id}/retry-refund"}:         ofCreditNote(invoiceWrite).withAudit(explicit("creditnote.Service.RetryRefund (LogInTx)")),
	{"POST", "/v1/credit-notes/{id}/send"}:                 ofCreditNote(invoiceWrite).withAudit(explicit("creditnote.Handler.sendEmail (Log)")),
	{"POST", "/v1/credit-notes/{id}/void"}:                 ofCreditNote(invoiceWrite).withAudit(explicit("creditnote.Service.Void (LogInTx)")),

	// --- credits ------------------------------------------------------------
	{"GET", "/v1/credits/balances"}: realTime(customerWrite),
	// the body names the customer
	{"POST", "/v1/credits/grant"}: decidedIn(customerWrite, "credit.Service.Grant").withAudit(explicit("credit.Service.Grant (LogInTx)")),
	// the body names the customer
	{"POST", "/v1/credits/adjust"}:                    decidedIn(customerWrite, "credit.Service.Adjust").withAudit(explicit("credit.Service.Adjust (LogInTx)")),
	{"GET", "/v1/credits/balance/{customer_id}"}:      ofCustomer(customerWrite, "customer_id"),
	{"GET", "/v1/credits/balance/{customer_id}/live"}: ofCustomer(customerWrite, "customer_id"),
	{"GET", "/v1/credits/grants/{customer_id}"}:       ofCustomer(customerWrite, "customer_id"),
	{"GET", "/v1/credits/ledger/{customer_id}"}:       ofCustomer(customerWrite, "customer_id"),

	// --- dunning runs -------------------------------------------------------
	{"GET", "/v1/dunning/runs"}:      realTime(dunningRead),
	{"GET", "/v1/dunning/runs/{id}"}: ofDunningRun(dunningRead),
}
