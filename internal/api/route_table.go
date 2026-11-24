package api

import (
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/request"
)

// The route table has one entry per route the router serves: who may call it,
// where its audit row comes from, and whether its answer shows a secret once
// (docs/architecture.md#the-route-table).
// TestRouteTable_MatchesLiveRoutes fails on a route with no entry, an entry
// with no route, and a missing or stray audit.

// routeKey is (HTTP method, canonical chi route pattern).
type routeKey struct {
	Method  string
	Pattern string
}

// routeDecl is one route's entry, built in steps. needs or outsideAuth says
// who may call it, which makes the entry. Then withAudit adds its audit, and
// withShowOnce marks its answer.
type routeDecl struct {
	// access is who may call the route, which auth.RequirePermission checks;
	// it is zero on a route outside authentication.
	access      auth.Access
	outsideAuth bool
	// audit is zero on a route that neither changes state nor exports.
	audit    auditDecl
	showOnce bool
}

// withAudit is d with its audit declaration.
func (d routeDecl) withAudit(a auditDecl) routeDecl {
	d.audit = a
	return d
}

// withShowOnce is d with an answer that carries a secret shown only in that
// answer: a field marked `x-show-once: true` in api/openapi.yaml, which
// TestOpenAPISpec_ShowOnceSecretsAreNeverKept keeps in step with the table.
// /v1/bootstrap's answer has marked fields and its entry is not marked. The
// mark only tells the idempotency layer not to keep an answer, and bootstrap
// is mounted outside that layer; the test would fail if it were moved inside
// without the mark.
func (d routeDecl) withShowOnce() routeDecl {
	d.showOnce = true
	return d
}

// keepsNoKey is the server's ignore for Idempotency (mw.Idempotency): a
// request it matches runs as if no Idempotency-Key was sent. It matches a
// request no route serves, where nothing can run, and one whose answer
// carries a secret shown once, which must never be kept. This is the one
// statement of that rule; other comments point here.
func keepsNoKey(r *http.Request) bool {
	return request.Of(r.Context()).Route().Pattern == "" || isShowOnce(r)
}

// isShowOnce reports whether r's route answers with a secret shown once, so
// the idempotency layer leaves it alone.
func isShowOnce(r *http.Request) bool {
	return routeTable[routeKeyOf(r.Context())].showOnce
}

// needs is the entry of a route behind authentication, which a caller with
// access may call. The table holds entries, so a line that calls neither
// needs nor outsideAuth does not compile.
func needs(access auth.Access) routeDecl {
	return routeDecl{access: access}
}

// outsideAuth is the entry of a route outside authentication: authentication
// does not run, and its handler checks its own credential, if any: a session
// cookie (sign-out, the mode switch), a token, a signature or a password.
func outsideAuth() routeDecl {
	return routeDecl{outsideAuth: true}
}

// permission is the access of a route that needs p.
func permission(p auth.Permission) auth.Access { return auth.Access{Perm: p} }

// sessionOnly is the access of a route only a dashboard session may call.
// Its 403 tells any other caller to sign in to the dashboard to do task.
func sessionOnly(task string) auth.Access { return auth.Access{Session: task} }

// The access values an entry names, one per permission and kind of caller.
//
// Seven areas take one permission for reads and writes alike, so their GET
// entries name a write permission on purpose:
//   - API keys, settings (Stripe and email included), webhook endpoints:
//     apikeyWrite;
//   - price overrides, recipes: pricingWrite;
//   - credit notes: invoiceWrite;
//   - credits: customerWrite.
//
// apikeyRead guards webhook events and their stream, and the audit log and
// its export.
var (
	// anyCaller: any API key or dashboard session.
	// TestRouteTable_AnyCallerStaysSmall lists the routes that take it.
	anyCaller = auth.Access{Any: true}

	membersSession  = sessionOnly("manage team membership")
	settingsSession = sessionOnly("change account settings")

	customerRead      = permission(auth.PermCustomerRead)
	customerWrite     = permission(auth.PermCustomerWrite)
	pricingRead       = permission(auth.PermPricingRead)
	pricingWrite      = permission(auth.PermPricingWrite)
	subscriptionRead  = permission(auth.PermSubscriptionRead)
	subscriptionWrite = permission(auth.PermSubscriptionWrite)
	usageRead         = permission(auth.PermUsageRead)
	usageWrite        = permission(auth.PermUsageWrite)
	usageIngest       = permission(auth.PermUsageIngest)
	invoiceRead       = permission(auth.PermInvoiceRead)
	invoiceWrite      = permission(auth.PermInvoiceWrite)
	dunningRead       = permission(auth.PermDunningRead)
	dunningWrite      = permission(auth.PermDunningWrite)
	apikeyRead        = permission(auth.PermAPIKeyRead)
	apikeyWrite       = permission(auth.PermAPIKeyWrite)
)

// tableAccess is the route table as auth.RequirePermission reads it: each
// request's access, from its route's entry. A request with no entry is one no
// route serves (mustListEveryRoute).
type tableAccess struct{}

func (tableAccess) Lookup(r *http.Request) (auth.Access, bool) {
	d, ok := routeTable[routeKeyOf(r.Context())]
	return d.access, ok
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
	// there is none (exempt). An accepted loss, and what would close it, is a
	// comment above the entry.
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
func auditRoute(r *http.Request) (pattern string, declaredExempt bool) {
	key := routeKeyOf(r.Context())
	return key.Pattern, routeTable[key].audit.Coverage == auditExempt
}

// canonicalRoute gives chi.Walk's patterns and Find's one key, as chi's
// RoutePattern does: it drops each "/*/" (Walk prints one where a Mount("/")
// sits inside a Route: /v1/auth/*/login), trims a trailing slash (a
// sub-router's root: /v1/customers/), and makes an empty pattern "/".
func canonicalRoute(p string) string {
	for strings.Contains(p, "/*/") {
		p = strings.ReplaceAll(p, "/*/", "/")
	}
	if p != "/" {
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
	{"GET", "/health"}:       outsideAuth(),
	{"GET", "/health/ready"}: outsideAuth(),
	// chi expands r.Handle("/metrics") to every method. Each is declared: a
	// walk that skipped them would also skip a future route registered so.
	// The write methods need an audit declaration, though each only returns
	// the scrape.
	{"GET", "/metrics"}:     outsideAuth(),
	{"HEAD", "/metrics"}:    outsideAuth(),
	{"POST", "/metrics"}:    outsideAuth().withAudit(exempt(reasonSystemEndpoint, "Returns the Prometheus scrape and changes nothing.")),
	{"PUT", "/metrics"}:     outsideAuth().withAudit(exempt(reasonSystemEndpoint, "As POST /metrics.")),
	{"PATCH", "/metrics"}:   outsideAuth().withAudit(exempt(reasonSystemEndpoint, "As POST /metrics.")),
	{"DELETE", "/metrics"}:  outsideAuth().withAudit(exempt(reasonSystemEndpoint, "As POST /metrics.")),
	{"CONNECT", "/metrics"}: outsideAuth(),
	{"OPTIONS", "/metrics"}: outsideAuth(),
	{"TRACE", "/metrics"}:   outsideAuth(),
	{"QUERY", "/metrics"}:   outsideAuth(),

	// --- sign-in and setup ----------------------------------------------
	{"POST", "/v1/bootstrap"}:                   outsideAuth().withAudit(explicit("tenant.RunBootstrap (LogInTx, on its own transaction). Never exempt: it mints a live secret key.")),
	{"POST", "/v1/auth/login"}:                  outsideAuth().withAudit(explicit("user.Service.Login (LogInTx)")),
	{"POST", "/v1/auth/logout"}:                 outsideAuth().withAudit(explicit("user.Service.Logout (LogInTx)")),
	{"POST", "/v1/auth/mode"}:                   outsideAuth().withAudit(explicit("user.Service.SetMode (LogInTx)")),
	{"GET", "/v1/auth/password-reset/check"}:    outsideAuth(),
	{"POST", "/v1/auth/password-reset/confirm"}: outsideAuth().withAudit(explicit("user.Service.ConfirmReset (LogInTx)")),
	{"POST", "/v1/auth/password-reset/request"}: outsideAuth().withAudit(explicit("user.Handler.requestPasswordReset (Log)")),
	{"GET", "/v1/auth/invite/{token}"}:          outsideAuth(),
	{"POST", "/v1/auth/accept-invite"}:          outsideAuth().withAudit(explicit("dashmembers.Service.AcceptInvite (LogInTx)")),
	{"GET", "/v1/whoami"}:                       needs(anyCaller),
	{"GET", "/v1/members"}:                      needs(membersSession),
	{"POST", "/v1/members/invite"}:              needs(membersSession).withAudit(explicit("dashmembers.Service.Invite (LogInTx)")),
	{"DELETE", "/v1/members/invitations/{id}"}:  needs(membersSession).withAudit(explicit("dashmembers.Service.Revoke (LogInTx)")),
	{"DELETE", "/v1/members/{userID}"}:          needs(membersSession).withAudit(explicit("dashmembers.Service.RemoveMember (LogInTx)")),
	{"GET", "/v1/api-keys"}:                     needs(apikeyWrite),
	{"POST", "/v1/api-keys"}:                    needs(apikeyWrite).withAudit(explicit("auth.Service.CreateKey (LogInTx)")).withShowOnce(),
	{"DELETE", "/v1/api-keys/{id}"}:             needs(apikeyWrite).withAudit(explicit("auth.Service.RevokeKey (LogInTx)")),
	{"POST", "/v1/api-keys/{id}/rotate"}:        needs(apikeyWrite).withAudit(explicit("auth.Service.RotateKey (LogInTx)")).withShowOnce(),

	// --- inbound webhooks -----------------------------------------------
	// Accepted loss: a card settlement or dispute writes no audit_log row; the
	// invoice (a settlement) or its invoice_disputes row (a dispute), the
	// outbox events and stripe_webhook_events record it. Close it with a row
	// on the settle transaction when an auditor asks audit_log for settlement
	// evidence.
	{"POST", "/v1/webhooks/stripe/{endpoint_id}"}: outsideAuth().withAudit(exempt(reasonWebhookOwned, "Its handlers audit a card saved at checkout, a refund's status and a refund made in Stripe, each in its own transaction.")),
	// Accepted loss: the effects of email reports are not audit_log rows; the
	// outbox row's delivery state, the customer's email status and the outbox
	// events record them.
	{"POST", "/v1/webhooks/postmark"}: outsideAuth().withAudit(exempt(reasonWebhookOwned, "Postmark's delivery reports, behind Basic Auth; the tenant comes from the stored outbox row (ADR-098).")),

	// --- public pages ---------------------------------------------------
	{"GET", "/v1/public/invoices/{token}"}:                  outsideAuth(),
	{"POST", "/v1/public/invoices/{token}/checkout"}:        outsideAuth().withAudit(explicit("api.hostedInvoiceStripeAdapter.CreateInvoicePaymentSession (LogInTx)")),
	{"GET", "/v1/public/invoices/{token}/pdf"}:              outsideAuth(),
	{"GET", "/v1/public/payment-updates/{token}"}:           outsideAuth(),
	{"POST", "/v1/public/payment-updates/{token}/checkout"}: outsideAuth().withAudit(explicit("payment.PublicPaymentHandler.createCheckoutSession (LogInTx)")),
	{"GET", "/v1/public/cost-dashboard/{token}"}:            outsideAuth(),

	// --- tenant-wide reads and streams ------------------------------------
	{"GET", "/v1/webhook_events/stream"}: needs(apikeyRead),
	// The audit log's export needs what reading it does: the file is the
	// same data.
	{"GET", "/v1/exports/audit-log.csv"}:     needs(apikeyRead).withAudit(explicit("api.exportsHandler.exportAuditLog (auditExport, before the first byte)")),
	{"GET", "/v1/exports/customers.csv"}:     needs(customerRead).withAudit(explicit("api.exportsHandler.exportCustomers (auditExport, before the first byte)")),
	{"GET", "/v1/exports/invoices.csv"}:      needs(invoiceRead).withAudit(explicit("api.exportsHandler.exportInvoices (auditExport, before the first byte)")),
	{"GET", "/v1/exports/subscriptions.csv"}: needs(subscriptionRead).withAudit(explicit("api.exportsHandler.exportSubscriptions (auditExport, before the first byte)")),
	{"GET", "/v1/exports/usage-events.csv"}:  needs(usageRead).withAudit(explicit("api.exportsHandler.exportUsageEvents (auditExport, before the first byte)")),
	{"GET", "/v1/analytics/mrr-movement"}:    needs(invoiceRead),
	{"GET", "/v1/analytics/overview"}:        needs(invoiceRead),
	{"GET", "/v1/analytics/revenue-chart"}:   needs(invoiceRead),
	{"GET", "/v1/analytics/usage"}:           needs(invoiceRead),
	{"GET", "/v1/audit-log"}:                 needs(apikeyRead),
	{"GET", "/v1/audit-log/filters"}:         needs(apikeyRead),

	// --- pricing, settings and tooling -----------------------------------
	{"GET", "/v1/meters"}:                                                           needs(pricingRead),
	{"POST", "/v1/meters"}:                                                          needs(pricingWrite).withAudit(explicit("pricing.Handler.createMeter (Log)")),
	{"GET", "/v1/meters/{id}"}:                                                      needs(pricingRead),
	{"PATCH", "/v1/meters/{id}"}:                                                    needs(pricingWrite).withAudit(explicit("pricing.Service.UpdateMeter (LogInTx)")),
	{"GET", "/v1/plans"}:                                                            needs(pricingRead),
	{"POST", "/v1/plans"}:                                                           needs(pricingWrite).withAudit(explicit("pricing.Handler.createPlan (Log)")),
	{"GET", "/v1/plans/{id}"}:                                                       needs(pricingRead),
	{"PATCH", "/v1/plans/{id}"}:                                                     needs(pricingWrite).withAudit(explicit("pricing.Handler.updatePlan (Log)")),
	{"GET", "/v1/plans/{id}/prices"}:                                                needs(pricingRead),
	{"POST", "/v1/plans/{id}/prices"}:                                               needs(pricingWrite).withAudit(explicit("pricing.Service.AddPrice (LogInTx)")),
	{"PATCH", "/v1/plans/{id}/prices/{price_id}"}:                                   needs(pricingWrite).withAudit(explicit("pricing.Service.EditPrice (LogInTx)")),
	{"DELETE", "/v1/plans/{id}/prices/{price_id}"}:                                  needs(pricingWrite).withAudit(explicit("pricing.Service.RemovePrice (LogInTx)")),
	{"GET", "/v1/price-overrides"}:                                                  needs(pricingWrite),
	{"POST", "/v1/price-overrides"}:                                                 needs(pricingWrite).withAudit(explicit("pricing.Handler.createOverride (Log)")),
	{"DELETE", "/v1/price-overrides/{id}"}:                                          needs(pricingWrite).withAudit(explicit("pricing.Handler.deleteOverride (Log)")),
	{"GET", "/v1/provider-costs"}:                                                   needs(usageRead),
	{"PUT", "/v1/provider-costs"}:                                                   needs(usageWrite).withAudit(explicit("usage.ProviderCostHandler.upsert (LogInTx)")),
	{"DELETE", "/v1/provider-costs/{id}"}:                                           needs(usageWrite).withAudit(explicit("usage.ProviderCostHandler.delete (LogInTx)")),
	{"GET", "/v1/recipes"}:                                                          needs(pricingWrite),
	{"GET", "/v1/recipes/instances"}:                                                needs(pricingWrite),
	{"GET", "/v1/recipes/{key}"}:                                                    needs(pricingWrite),
	{"POST", "/v1/recipes/{key}/instantiate"}:                                       needs(pricingWrite).withAudit(explicit("recipe.Service.Instantiate (LogInTx)")),
	{"POST", "/v1/recipes/{key}/preview"}:                                           needs(pricingWrite).withAudit(exempt(reasonNonMutatingPreview, "Renders what instantiate would create and writes nothing (recipe.Handler calls audit.MarkSkip).")),
	{"GET", "/v1/settings"}:                                                         needs(apikeyWrite),
	{"PUT", "/v1/settings"}:                                                         needs(settingsSession).withAudit(explicit("tenant.SettingsHandler.upsert (Log)")),
	{"GET", "/v1/settings/email"}:                                                   needs(apikeyWrite),
	{"POST", "/v1/settings/email/test"}:                                             needs(apikeyWrite).withAudit(explicit("api.emailSettingsHandler.sendTest (Log)")),
	{"GET", "/v1/settings/stripe"}:                                                  needs(apikeyWrite),
	{"POST", "/v1/settings/stripe"}:                                                 needs(apikeyWrite).withAudit(explicit("tenantstripe.Handler.connect (Log)")),
	{"DELETE", "/v1/settings/stripe/{mode}"}:                                        needs(apikeyWrite).withAudit(explicit("tenantstripe.Handler.delete (Log)")),
	{"PATCH", "/v1/settings/stripe/{mode}/webhook"}:                                 needs(apikeyWrite).withAudit(explicit("tenantstripe.Handler.setWebhook (Log)")),
	{"GET", "/v1/dunning/policies"}:                                                 needs(dunningRead),
	{"POST", "/v1/dunning/policies"}:                                                needs(dunningWrite).withAudit(explicit("dunning.Handler.createPolicy (Log)")),
	{"GET", "/v1/dunning/policies/{id}"}:                                            needs(dunningRead),
	{"PATCH", "/v1/dunning/policies/{id}"}:                                          needs(dunningWrite).withAudit(explicit("dunning.Handler.updatePolicy (Log)")),
	{"DELETE", "/v1/dunning/policies/{id}"}:                                         needs(dunningWrite).withAudit(explicit("dunning.Handler.deletePolicy (Log)")),
	{"POST", "/v1/dunning/policies/{id}/set-default"}:                               needs(dunningWrite).withAudit(explicit("dunning.Handler.setDefaultPolicy (Log)")),
	{"GET", "/v1/dunning/stats"}:                                                    needs(dunningRead),
	{"GET", "/v1/webhook-endpoints/endpoints"}:                                      needs(apikeyWrite),
	{"POST", "/v1/webhook-endpoints/endpoints"}:                                     needs(apikeyWrite).withAudit(explicit("webhook.Handler.createEndpoint (Log)")).withShowOnce(),
	{"GET", "/v1/webhook-endpoints/endpoints/stats"}:                                needs(apikeyWrite),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}"}:                                 needs(apikeyWrite),
	{"PATCH", "/v1/webhook-endpoints/endpoints/{id}"}:                               needs(apikeyWrite).withAudit(explicit("webhook.Handler.updateEndpoint (Log)")),
	{"DELETE", "/v1/webhook-endpoints/endpoints/{id}"}:                              needs(apikeyWrite).withAudit(explicit("webhook.Handler.deleteEndpoint (Log)")),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}/deliveries"}:                      needs(apikeyWrite),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/deliveries/{deliveryID}/replay"}: needs(apikeyWrite).withAudit(explicit("webhook.Handler.replayDelivery (Log)")),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/rotate-secret"}:                  needs(apikeyWrite).withAudit(explicit("webhook.Handler.rotateSecret (Log)")).withShowOnce(),
	{"GET", "/v1/webhook-endpoints/events"}:                                         needs(apikeyWrite),
	{"GET", "/v1/webhook_events/{id}/deliveries"}:                                   needs(apikeyRead),
	// A replay needs only apikey:read: it sends again an event already
	// emitted.
	{"POST", "/v1/webhook_events/{id}/replay"}: needs(apikeyRead).withAudit(explicit("webhook.Handler.replayEventV2 (Log)")),
	{"POST", "/v1/billing/run"}:                needs(invoiceWrite).withAudit(explicit("billing.Handler.triggerCycle (Log)")),

	// --- customers --------------------------------------------------------
	{"GET", "/v1/customers"}:                      needs(customerRead),
	{"POST", "/v1/customers"}:                     needs(customerWrite).withAudit(explicit("customer.Service.Create (LogInTx)")),
	{"GET", "/v1/customers/{id}"}:                 needs(customerRead),
	{"PATCH", "/v1/customers/{id}"}:               needs(customerWrite).withAudit(explicit("customer.Handler.update (Log)")),
	{"GET", "/v1/customers/{id}/billing-profile"}: needs(customerRead),
	{"PUT", "/v1/customers/{id}/billing-profile"}: needs(customerWrite).withAudit(explicit("customer.Handler.upsertBillingProfile (Log)")),
	// A customer's usage and margin need usage:read, not the customer
	// permissions.
	{"GET", "/v1/customers/{id}/margin"}:                                     needs(usageRead),
	{"POST", "/v1/customers/{id}/rotate-cost-dashboard-token"}:               needs(customerWrite).withAudit(explicit("customer.Handler.rotateCostDashboardToken (Log)")).withShowOnce(),
	{"GET", "/v1/customers/{id}/sent-emails"}:                                needs(customerRead),
	{"GET", "/v1/customers/{id}/usage"}:                                      needs(usageRead),
	{"GET", "/v1/customers/{customer_id}/overview"}:                          needs(customerRead),
	{"GET", "/v1/customers/{customer_id}/payment-methods"}:                   needs(customerRead),
	{"POST", "/v1/customers/{customer_id}/payment-methods/send-setup-email"}: needs(customerWrite).withAudit(explicit("paymentmethods.Handler.operatorSendSetupEmail (Log)")),
	{"POST", "/v1/customers/{customer_id}/payment-methods/setup-session"}:    needs(customerWrite).withAudit(explicit("paymentmethods.Service.CreateSetupSession (Log)")).withShowOnce(),
	{"DELETE", "/v1/customers/{customer_id}/payment-methods/{pmID}"}:         needs(customerWrite).withAudit(explicit("paymentmethods.Service.Detach (Log)")),
	{"POST", "/v1/customers/{customer_id}/payment-methods/{pmID}/default"}:   needs(customerWrite).withAudit(explicit("paymentmethods.Service.SetDefault (Log)")),

	// --- subscriptions ----------------------------------------------------
	{"GET", "/v1/subscriptions"}:                                       needs(subscriptionRead),
	{"POST", "/v1/subscriptions"}:                                      needs(subscriptionWrite).withAudit(explicit("subscription.Handler.create (Log)")),
	{"GET", "/v1/subscriptions/{id}"}:                                  needs(subscriptionRead),
	{"PUT", "/v1/subscriptions/{id}/billing-thresholds"}:               needs(subscriptionWrite).withAudit(explicit("subscription.Handler.setBillingThresholds (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/billing-thresholds"}:            needs(subscriptionWrite).withAudit(explicit("subscription.Handler.clearBillingThresholds (Log)")),
	{"POST", "/v1/subscriptions/{id}/cancel"}:                          needs(subscriptionWrite).withAudit(explicit("subscription.Service.Cancel (LogInTx)")),
	{"POST", "/v1/subscriptions/{id}/end-trial"}:                       needs(subscriptionWrite).withAudit(explicit("subscription.Handler.endTrial (Log)")),
	{"POST", "/v1/subscriptions/{id}/extend-trial"}:                    needs(subscriptionWrite).withAudit(explicit("subscription.Handler.extendTrial (Log)")),
	{"POST", "/v1/subscriptions/{id}/items"}:                           needs(subscriptionWrite).withAudit(explicit("subscription.Handler.addItem (Log)")),
	{"PATCH", "/v1/subscriptions/{id}/items/{itemID}"}:                 needs(subscriptionWrite).withAudit(explicit("subscription.Handler.updateItem (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}"}:                needs(subscriptionWrite).withAudit(explicit("subscription.Handler.removeItem (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}/pending-change"}: needs(subscriptionWrite).withAudit(explicit("subscription.Handler.cancelPendingItemChange (Log)")),
	{"PUT", "/v1/subscriptions/{id}/pause-collection"}:                 needs(subscriptionWrite).withAudit(explicit("subscription.Service.PauseCollection (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/pause-collection"}:              needs(subscriptionWrite).withAudit(explicit("subscription.Service.ResumeCollection (Log)")),
	{"POST", "/v1/subscriptions/{id}/retry-billing"}:                   needs(subscriptionWrite).withAudit(explicit("subscription.Service.RetryBilling (LogInTx)")),
	{"POST", "/v1/subscriptions/{id}/schedule-cancel"}:                 needs(subscriptionWrite).withAudit(explicit("subscription.Handler.scheduleCancel (Log)")),
	{"DELETE", "/v1/subscriptions/{id}/scheduled-cancel"}:              needs(subscriptionWrite).withAudit(explicit("subscription.Handler.clearScheduledCancel (Log)")),
	{"GET", "/v1/subscriptions/{id}/timeline"}:                         needs(subscriptionRead),

	// --- usage --------------------------------------------------------------
	// Sending usage needs only usage:ingest, the one permission an ingest key
	// holds (ADR-133); backfill, the reads and the held-usage actions need
	// more. Accepted loss, for the three routes that send usage: which key
	// sent an event, since usage_events has no key_id. Close it with that
	// column at the first billing dispute that turns on which key sent usage.
	{"POST", "/v1/usage-events"}:               needs(usageIngest).withAudit(exempt(reasonMachineIngest, "usage_events is the record, and a row per call would double the writes of the busiest path.")),
	{"POST", "/v1/usage-events/batch"}:         needs(usageIngest).withAudit(exempt(reasonMachineIngest, "The batch form of the same ingest.")),
	{"POST", "/v1/integrations/litellm/spend"}: needs(usageIngest).withAudit(exempt(reasonMachineIngest, "LiteLLM's spend callback, which writes usage events like the ingest above.")),
	// Backfill is audited, unlike the ingest above: an operator's backdated
	// usage changes what a customer is billed.
	{"POST", "/v1/usage-events/backfill"}: needs(usageWrite).withAudit(explicit("usage.Service.Backfill (LogInTx)")),
	// Held usage (ADR-122) is what the LiteLLM callback could not ingest.
	// Releasing it changes what a customer is billed; discarding it means it
	// never bills.
	{"POST", "/v1/usage-events/held/release"}: needs(usageWrite).withAudit(explicit("usage.Service.Release (LogInTx)")),
	{"POST", "/v1/usage-events/held/discard"}: needs(usageWrite).withAudit(explicit("usage.Service.Discard (LogInTx)")),
	{"GET", "/v1/usage-events"}:               needs(usageRead),
	{"GET", "/v1/usage-events/aggregate"}:     needs(usageRead),
	{"GET", "/v1/usage-events/held"}:          needs(usageRead),

	// --- invoices -----------------------------------------------------------
	{"GET", "/v1/invoices"}:                                                 needs(invoiceRead),
	{"POST", "/v1/invoices"}:                                                needs(invoiceWrite).withAudit(explicit("invoice.Service.Create (LogInTx)")),
	{"POST", "/v1/invoices/create_preview"}:                                 needs(invoiceRead).withAudit(exempt(reasonNonMutatingPreview, "Computes an estimate and writes nothing (billing.CreatePreviewHandler calls audit.MarkSkip).")),
	{"GET", "/v1/invoices/{id}"}:                                            needs(invoiceRead),
	{"POST", "/v1/invoices/{id}/collect"}:                                   needs(invoiceWrite).withAudit(explicit("invoice.Handler.collectPayment (Log)")),
	{"GET", "/v1/invoices/{id}/disputes"}:                                   needs(invoiceRead),
	{"POST", "/v1/invoices/{id}/finalize"}:                                  needs(invoiceWrite).withAudit(explicit("invoice.Service.Finalize (LogInTx)")),
	{"POST", "/v1/invoices/{id}/line-items"}:                                needs(invoiceWrite).withAudit(explicit("invoice.Service.AddLineItem (LogInTx)")),
	{"POST", "/v1/invoices/{id}/mark-uncollectible"}:                        needs(invoiceWrite).withAudit(explicit("invoice.Service.MarkUncollectible (Log)")),
	{"GET", "/v1/invoices/{id}/payment-timeline"}:                           needs(invoiceRead),
	{"GET", "/v1/invoices/{id}/pdf"}:                                        needs(invoiceRead),
	{"POST", "/v1/invoices/{id}/record-payment"}:                            needs(invoiceWrite).withAudit(explicit("invoice.Service.RecordOfflinePayment (Log)")),
	{"POST", "/v1/invoices/{id}/refund"}:                                    needs(invoiceWrite).withAudit(explicit("invoice.Handler.refund (Log)")),
	{"POST", "/v1/invoices/{id}/resend-setup-link"}:                         needs(invoiceWrite).withAudit(explicit("invoice.Handler.resendSetupLink (Log)")),
	{"POST", "/v1/invoices/{id}/retry-tax"}:                                 needs(invoiceWrite).withAudit(explicit("invoice.Handler.retryTax (Log)")),
	{"POST", "/v1/invoices/{id}/rotate-public-token"}:                       needs(invoiceWrite).withAudit(explicit("invoice.Handler.rotatePublicToken (Log)")),
	{"POST", "/v1/invoices/{id}/send"}:                                      needs(invoiceWrite).withAudit(explicit("invoice.Handler.sendEmail (Log)")),
	{"GET", "/v1/invoices/{id}/stripe-refunds"}:                             needs(invoiceRead),
	{"POST", "/v1/invoices/{id}/stripe-refunds/{stripe_refund_id}/dismiss"}: needs(invoiceWrite).withAudit(explicit("creditnote.Service.DismissStripeRefund (LogInTx)")),
	{"POST", "/v1/invoices/{id}/void"}:                                      needs(invoiceWrite).withAudit(explicit("invoice.Service.Void (LogInTx)")),

	// --- credit notes -------------------------------------------------------
	{"GET", "/v1/credit-notes"}:                            needs(invoiceWrite),
	{"GET", "/v1/credit-notes/stats"}:                      needs(invoiceWrite),
	{"POST", "/v1/credit-notes"}:                           needs(invoiceWrite).withAudit(explicit("creditnote.Service.Create (LogInTx)")),
	{"GET", "/v1/credit-notes/{id}"}:                       needs(invoiceWrite),
	{"POST", "/v1/credit-notes/{id}/attach-stripe-refund"}: needs(invoiceWrite).withAudit(explicit("creditnote.Service.AttachStripeRefund (LogInTx)")),
	{"POST", "/v1/credit-notes/{id}/issue"}:                needs(invoiceWrite).withAudit(explicit("creditnote.Service.Issue (LogInTx)")),
	{"GET", "/v1/credit-notes/{id}/pdf"}:                   needs(invoiceWrite),
	{"POST", "/v1/credit-notes/{id}/retry-refund"}:         needs(invoiceWrite).withAudit(explicit("creditnote.Service.RetryRefund (LogInTx)")),
	{"POST", "/v1/credit-notes/{id}/send"}:                 needs(invoiceWrite).withAudit(explicit("creditnote.Handler.sendEmail (Log)")),
	{"POST", "/v1/credit-notes/{id}/void"}:                 needs(invoiceWrite).withAudit(explicit("creditnote.Service.Void (LogInTx)")),

	// --- credits ------------------------------------------------------------
	{"GET", "/v1/credits/balances"}:                   needs(customerWrite),
	{"POST", "/v1/credits/grant"}:                     needs(customerWrite).withAudit(explicit("credit.Service.Grant (LogInTx)")),
	{"POST", "/v1/credits/adjust"}:                    needs(customerWrite).withAudit(explicit("credit.Service.Adjust (LogInTx)")),
	{"GET", "/v1/credits/balance/{customer_id}"}:      needs(customerWrite),
	{"GET", "/v1/credits/balance/{customer_id}/live"}: needs(customerWrite),
	{"GET", "/v1/credits/grants/{customer_id}"}:       needs(customerWrite),
	{"GET", "/v1/credits/ledger/{customer_id}"}:       needs(customerWrite),

	// --- dunning runs -------------------------------------------------------
	{"GET", "/v1/dunning/runs"}:      needs(dunningRead),
	{"GET", "/v1/dunning/runs/{id}"}: needs(dunningRead),
}
