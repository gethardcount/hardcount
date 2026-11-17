package api

import (
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// The route table: what each route declares, in one place.
//
// Every route the router serves has one entry, keyed by its method and
// canonical pattern. The entry says:
//   - how a request's time is decided (the clock door reads it);
//   - for a route that can change state, and for a bulk export, where its
//     audit evidence comes from (the audit detector reads it);
//   - whether its answer carries a secret shown only once (the idempotency
//     layer never keeps that answer for a replay).
//
// Each request's key is found at the root, on its record (routeFinder).
// TestRouteTable_MatchesLiveRoutes walks the router and compares it with the
// table in both directions, and reports every gap in one run: a route with
// no entry, an entry with no route, a mutating route or an export with no
// audit, and an audit on any other route. A forgotten showsSecretOnce()
// fails TestOpenAPISpec_ShowOnceSecretsAreNeverKept, once the field that
// carries the secret is marked x-show-once in api/openapi.yaml. So adding a
// route forces each decision at review.
// ---------------------------------------------------------------------------

// routeKey is (HTTP method, canonical chi route pattern).
type routeKey struct {
	Method  string
	Pattern string
}

// routeDecl is one route's entry. A clock constructor below (realTime,
// ofCustomer, ofSubscription, ofInvoice, ofCreditNote, ofDunningRun,
// decidedIn) makes it; with adds its audit, and showsSecretOnce marks its
// answer.
type routeDecl struct {
	clock clockDecl
	// audit is zero on a route that neither changes state nor exports.
	audit    auditDecl
	showOnce bool
}

// with is d with its audit declaration.
func (d routeDecl) with(a auditDecl) routeDecl {
	d.audit = a
	return d
}

// showsSecretOnce is d with an answer that carries a secret shown only in
// that answer: the fields marked `x-show-once: true` in api/openapi.yaml.
// TestOpenAPISpec_ShowOnceSecretsAreNeverKept keeps the marks and the table
// in step. /v1/bootstrap's fields are marked too, but its entry is not: it
// is mounted outside the idempotency layer, and the test asserts it stays
// there.
func (d routeDecl) showsSecretOnce() routeDecl {
	d.showOnce = true
	return d
}

// answersWithShowOnceSecret reports whether r's route answers with a secret
// shown once, so the idempotency layer leaves it alone.
func answersWithShowOnceSecret(r *http.Request) bool {
	return routeTable[routeKeyOf(r.Context())].showOnce
}

// ---------------------------------------------------------------------------
// The clock: how the door decides each request's time.
//
// Every route states how a request's time is decided, once, before its
// handler runs. It is one read of the server's clock, or the time of the test
// clock the request's record is on, or, where the request does not name its
// record by a path parameter, the time a named function decides after it
// reads the record. Only test mode has clocks: a live request on a real-time
// or a record route gets one read of the server's clock and no record is
// read, and a live request on a decidedIn route gets nothing from the door
// (api.clockDoor says the rest).
// ---------------------------------------------------------------------------

// clockSource is how a test-mode request's time is decided.
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

// The clock constructors: each makes a route's entry, with how its time is
// decided.
func realTime() routeDecl { return clocked(clockDecl{Source: clockRealTime}) }
func ofCustomer(param string) routeDecl {
	return clocked(clockDecl{Source: clockOfCustomer, Param: param})
}
func ofSubscription() routeDecl { return clocked(clockDecl{Source: clockOfSubscription, Param: "id"}) }
func ofInvoice() routeDecl      { return clocked(clockDecl{Source: clockOfInvoice, Param: "id"}) }
func ofCreditNote() routeDecl   { return clocked(clockDecl{Source: clockOfCreditNote, Param: "id"}) }
func ofDunningRun() routeDecl   { return clocked(clockDecl{Source: clockOfDunningRun, Param: "id"}) }
func decidedIn(function string) routeDecl {
	return clocked(clockDecl{Source: clockDecidedIn, In: function})
}

// clocked is a route's entry with only its clock declared.
func clocked(c clockDecl) routeDecl { return routeDecl{clock: c} }

// ---------------------------------------------------------------------------
// The audit (ADR-090).
//
// The table states, for every mutating route, where its audit evidence comes
// from. Coverage is declared, not inferred by a catch-all middleware. Such a
// middleware would infer the action and resource from the URL, and so write
// false permanent rows into an append-only compliance log (ADR-090 RC2). It
// would also miss every route outside the /v1 block it is mounted on (RC1).
//
// Each route has one of two kinds of coverage:
//
//   - explicit: the route emits a real, typed audit row. That is a LogInTx
//     emission on the business transaction, or a residual Logger.Log on its
//     own transaction, on a route not yet moved to LogInTx (ADR-090). Every
//     entry below was checked by reading the emitting code.
//   - exempt: the route writes no audit row, on purpose. The reason comes
//     from a closed enum, and the note records why, plus any accepted loss.
//
// At runtime the detector (mw.AuditCoverage), which only observes, reports
// any mutating 2xx that wrote no audit row and is not declared exempt here.
//
// Read egress (ADR-090 §7). Mutations are only half of a chain of custody. A
// full-tenant export can take every customer and their PII, every invoice,
// every subscription, a year of usage events, or the audit log itself. A tamper-evidence system
// that cannot show who copied the evidence has a hole in its chain of
// custody. Stripe and AWS CloudTrail both log data-export and read events,
// and so does Hardcount.
//
// These five GET routes emit action=export before the first byte streams.
// If the row cannot be written, they fail closed (exportsHandler.auditExport).
//
// The scope, and the loss this does not close on purpose, are written down.
// A note that implies coverage it does not have is worse than no note.
//
//   - This covers the bulk export routes, /v1/exports/*.csv, where one
//     request returns the whole table.
//   - It does not audit ordinary paged list reads
//     (GET /v1/customers?limit=100&offset=…). An operator who walks those
//     pages copies the same data, and audit_log will not show it. audit_log
//     can answer "did anyone take the one-click bulk export, and when?".
//     That is the question an auditor asks, and the action a departing
//     employee takes.
//   - A row per list call would put audit_log on the busiest read path in
//     the dashboard (every page load of every table). The noise would make
//     the audit log unreadable. The same volume argument keeps usage ingest
//     exempt below.
//   - Close this loss when a design partner or an auditor asks for evidence
//     of PII reads (SOC 2 CC6.x-style "who accessed customer records"). The
//     answer then is a read trail built from the access log, grouped per
//     session or actor, not one audit_log row per GET.
//
// On purpose, the GET routes under /v1/exports are the only reads that carry
// an audit. Five routes do not justify an audit story for every read endpoint
// in the product, and a table nobody can keep current is a table that lies.
// ---------------------------------------------------------------------------

// auditCoverage is how a route's audit evidence is produced.
type auditCoverage int

const (
	// auditExplicit: the route emits its own typed audit row.
	auditExplicit auditCoverage = iota + 1
	// auditExempt: the route writes no audit row, on purpose. Needs a reason.
	auditExempt
)

// exemptReason is a closed enum. A new reason widens, on purpose, what
// "un-audited" may mean. So it is added here, in review, with a note, and
// never as a free-text excuse at the call site.
type exemptReason string

const (
	// reasonNonMutatingPreview: a read-only POST, a POST only because the
	// input does not fit a query string. It computes a result and returns
	// it, and writes nothing.
	reasonNonMutatingPreview exemptReason = "non_mutating_preview"

	// reasonMachineIngest: high-volume machine ingest, where the events table
	// is the record. See the accepted loss recorded on each entry.
	reasonMachineIngest exemptReason = "machine_ingest"

	// reasonSystemEndpoint: not an operator action on tenant data (no tenant to
	// scope a row to).
	reasonSystemEndpoint exemptReason = "system_endpoint"

	// reasonWebhookOwned: an inbound provider webhook. Its handlers audit its
	// effects, with emissions inside their transactions. The delivery itself
	// is recorded in stripe_webhook_events.
	reasonWebhookOwned exemptReason = "webhook_owned"
)

// There is no `bootstrap` reason, on purpose. Bootstrap mints a live secret
// key and the owner account. By the time it can emit, the tenant it just
// created exists, so its rows have a tenant to belong to. POST /v1/bootstrap
// is `explicit`, and writes its provisioning rows into the new tenant's own
// log. A bootstrap reason would un-audit the creation of a credential that
// can move money.

// auditDecl is a route's audit declaration.
type auditDecl struct {
	Coverage auditCoverage
	// Reason is set when, and only when, Coverage == auditExempt.
	Reason exemptReason
	// Note names the emission site (explicit) or justifies the exemption and
	// records any accepted loss (exempt).
	Note string
}

// explicit is an audit the route emits itself; note names where.
func explicit(note string) auditDecl {
	return auditDecl{Coverage: auditExplicit, Note: note}
}

// exempt is no audit row, on purpose: reason is from the closed enum, and
// note says why and records any accepted loss.
func exempt(reason exemptReason, note string) auditDecl {
	return auditDecl{Coverage: auditExempt, Reason: reason, Note: note}
}

// auditRoute is the request's route for the runtime detector: its pattern,
// which labels the uncovered-mutation counter, and whether the table
// declares it exempt. The detector asks before it reports an uncovered
// mutation. Anything not in the table is not exempt, so a gap is reported
// rather than hidden.
func auditRoute(r *http.Request) (pattern string, exempt bool) {
	key := routeKeyOf(r.Context())
	return key.Pattern, routeTable[key].audit.Coverage == auditExempt
}

// canonicalRoute normalizes a chi route pattern, so the two sources of
// patterns agree on one key. One is chi.Walk's pattern string, which the
// table tests see. The other is Find's, which routeFinder gives each
// request's record. Raw, they can differ in two ways:
//   - Walk does a single strings.Replace("/*/", "/") pass. So a Mount("/", …)
//     inside a Route(…) leaves a "/*/" behind ("/v1/auth/*/login").
//   - A subrouter's r.Post("/") arrives with a trailing slash
//     ("/v1/customers/").
//
// chi's RoutePattern() loops the replace and trims; applying the same
// normalization to both sides makes them meet
// (TestRouteFinder_FindsEveryRouteAndItsParams).
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

// routeTable is the table. Keys are canonical (method, route pattern).
//
// The audit notes come from walking the live router and reading the emitting
// code for every mutating route, not from guessing. Each explicit note names
// the emitter, so a reviewer can check the claim with one grep.
var routeTable = map[routeKey]routeDecl{
	// --- system ---------------------------------------------------------
	{"GET", "/health"}:       realTime(),
	{"GET", "/health/ready"}: realTime(),
	// /metrics is registered with r.Handle, which chi expands to every method
	// (its mALL). So the route walk rightly yields every method of /metrics,
	// and each is declared, not filtered out: skipping r.Handle routes without
	// a word would hide any future route that chi registers for every method.
	{"GET", "/metrics"}:     realTime(),
	{"HEAD", "/metrics"}:    realTime(),
	{"POST", "/metrics"}:    realTime().with(exempt(reasonSystemEndpoint, "Prometheus scrape endpoint; r.Handle expands to every method. No tenant, no operator action.")),
	{"PUT", "/metrics"}:     realTime().with(exempt(reasonSystemEndpoint, "same r.Handle expansion as POST /metrics.")),
	{"PATCH", "/metrics"}:   realTime().with(exempt(reasonSystemEndpoint, "same r.Handle expansion as POST /metrics.")),
	{"DELETE", "/metrics"}:  realTime().with(exempt(reasonSystemEndpoint, "same r.Handle expansion as POST /metrics.")),
	{"CONNECT", "/metrics"}: realTime(),
	{"OPTIONS", "/metrics"}: realTime(),
	{"TRACE", "/metrics"}:   realTime(),
	{"QUERY", "/metrics"}:   realTime(),

	// --- sign-in and setup: users and tenants are on no clock ------------
	{"POST", "/v1/bootstrap"}:                   realTime().with(explicit("tenant.RunBootstrap emits on its own tx into the NEW tenant's log: create/tenant (action=bootstrap_provisioned) + one create/api_key row per minted credential (never the key material) + create/user for the owner. actor=system — there is genuinely no authenticated identity here. It mints a LIVE secret key, so \"who created this key?\" must be answerable.")),
	{"POST", "/v1/auth/login"}:                  realTime().with(explicit("user.Service.Login → LogInTx(login, user), on the transaction that creates the session, ends the session the request's cookie named and stamps last_login_at. Failed logins are pre-auth: no tenant to scope a row to, so they stay in the security log.")),
	{"POST", "/v1/auth/logout"}:                 realTime().with(explicit("user.Service.Logout → LogInTx(logout, user), on the transaction that ends the session. An absent cookie, or one for an unknown, ended or expired session, ends nothing and declares MarkSkip.")),
	{"POST", "/v1/auth/mode"}:                   realTime().with(explicit("user.Service.SetMode → LogInTx(mode_changed, user), on the transaction that switches the session between test and live mode.")),
	{"GET", "/v1/auth/password-reset/check"}:    realTime(),
	{"POST", "/v1/auth/password-reset/confirm"}: realTime().with(explicit("user.Service.ConfirmReset → LogInTx(password_reset_completed, user), on the transaction that sets the password and ends every session.")),
	{"POST", "/v1/auth/password-reset/request"}: realTime().with(explicit("user.Handler.requestPasswordReset → Log(password_reset_requested) on a matched account. A non-match (and the per-address throttle) writes nothing and declares MarkSkip — the fixed 200 is the enumeration defence.")),
	{"GET", "/v1/auth/invite/{token}"}:          realTime(),
	{"POST", "/v1/auth/accept-invite"}:          realTime().with(explicit("dashmembers.Service.AcceptInvite → LogInTx(member.joined, user), on the transaction that claims the invitation, writes the account or the membership and creates a new account's session.")),
	{"GET", "/v1/whoami"}:                       realTime(),
	{"GET", "/v1/members"}:                      realTime(),
	{"POST", "/v1/members/invite"}:              realTime().with(explicit("dashmembers.Service.Invite → LogInTx(member.invited, user), on the transaction that revokes the address's expired invitation, creates the invitation and queues its email.")),
	{"DELETE", "/v1/members/invitations/{id}"}:  realTime().with(explicit("dashmembers.Service.Revoke → LogInTx(member.invite_revoked, user), on the transaction that revokes the invitation.")),
	{"DELETE", "/v1/members/{userID}"}:          realTime().with(explicit("dashmembers.Service.RemoveMember → LogInTx(member.removed, user), on the transaction that deletes the membership, revokes the invitations the member sent that are not yet accepted, and ends their sessions in the workspace.")),
	{"GET", "/v1/api-keys"}:                     realTime(),
	{"POST", "/v1/api-keys"}:                    realTime().with(explicit("auth.Service.CreateKey → LogInTx(create, api_key) on the key's own tx.")).showsSecretOnce(),
	{"DELETE", "/v1/api-keys/{id}"}:             realTime().with(explicit("auth.Service.RevokeKey → LogInTx(revoke, api_key) on the revocation's tx.")),
	{"POST", "/v1/api-keys/{id}/rotate"}:        realTime().with(explicit("auth.Service.RotateKey → LogInTx(rotate, api_key) on the rotation's one tx.")).showsSecretOnce(),

	// --- inbound webhooks -----------------------------------------------
	// The endpoint names the tenant and mode; the customer is known only
	// after the event is read. Each event handler decides from the record it
	// names, not the settle, which already-decided paths also reach.
	{"POST", "/v1/webhooks/stripe/{endpoint_id}"}: decidedIn("payment.Stripe.HandleWebhook").with(exempt(reasonWebhookOwned, "Inbound Stripe delivery. Some effects DO audit in-tx (payment-method flips → payment.Stripe.handleCheckoutCompleted; refund-status flips → creditnote.ApplyRefundWebhook; refunds made in Stripe outside Hardcount → creditnote.RecordStripeRefund, which writes an update row on the invoice (action refunded_in_stripe) and, for a recorded note, its create and issued rows, ADR-134). ACCEPTED LOSS, recorded rather than glossed: card SETTLEMENT (payment_intent.succeeded/failed → payment.SettleSucceeded/SettleFailed) writes NO audit_log row. The settle is not evidence-free — invoices.paid_at/status, the in-tx invoice.paid + payment.succeeded outbox events, and stripe_webhook_events all record it, and it is machine-driven with no operator to attribute (the 2026-06-12 audit ruled webhooks+timeline own that record). But audit_log itself cannot answer \"when was INV-1042 settled?\". Closure trigger: an auditor or DP asking for settlement evidence FROM the audit log → emit on the settle tx (invoice.MarkPaidCardSettlementTransition would need an emit hook, and domain would need a settle action). Same ruling for card DISPUTES (charge.dispute.* → invoice.RecordStripeDispute, #979): no audit_log row; the invoice_disputes row, the in-tx payment.dispute_opened/closed outbox events and stripe_webhook_events record it.")),
	// email reports; reads no clock
	{"POST", "/v1/webhooks/postmark"}: realTime().with(exempt(reasonWebhookOwned, "Inbound Postmark delivery/bounce/complaint report (ADR-098). Basic-Auth gated; tenant resolves from the stamped outbox row, never the blind POST. Its effects are provider-truth STATE (email_outbox.delivery_state, customers.email_status) with the customer.email_bounced/email_complained outbox events as the observable record — machine-driven with no operator to attribute, same ruling as the Stripe settle path above. ACCEPTED LOSS: audit_log cannot answer 'when did the provider report INV-1042's email bounced' — the timeline (delivery_state) and webhook events own that record.")),

	// --- public pages: the token names the record, and the mode is not
	// known until it is read ----------------------------------------------
	{"GET", "/v1/public/invoices/{token}"}:           decidedIn("hostedinvoice.Handler.resolveInvoice"),
	{"POST", "/v1/public/invoices/{token}/checkout"}: decidedIn("hostedinvoice.Handler.resolveInvoice").with(explicit("hostedInvoiceStripeAdapter (api/adapters.go) → LogInTx on the checkout-claim tx; actor is the CUSTOMER (auth.WithCustomerActor). Outside the old catch-all entirely.")),
	{"GET", "/v1/public/invoices/{token}/pdf"}:       decidedIn("hostedinvoice.Handler.resolveInvoice"),
	{"GET", "/v1/public/payment-updates/{token}"}:    realTime(), // writes nothing
	// two audit rows, no money
	{"POST", "/v1/public/payment-updates/{token}/checkout"}: realTime().with(explicit("payment.PublicPaymentHandler.createCheckoutSession → LogInTx on the token consume/restore txs; customer actor.")),
	{"GET", "/v1/public/cost-dashboard/{token}"}:            decidedIn("usage.CostDashboardAssembler.GetByToken"),

	// --- tenant-wide reads and streams: each row by its stored dates ------
	{"GET", "/v1/webhook_events/stream"}:     realTime(),
	{"GET", "/v1/exports/audit-log.csv"}:     realTime().with(explicit("exportsHandler.exportAuditLog → auditExport(action=export, resource_type=audit_log) BEFORE the stream, fail-closed. Exporting the audit log is itself an audited event. The row is written first, so it is always in the LOG; an UNFILTERED export therefore also contains it in the FILE, but a filtered one need not (a clock-scoped export never does — the export row has NULL sim columns).")),
	{"GET", "/v1/exports/customers.csv"}:     realTime().with(explicit("exportsHandler.exportCustomers → auditExport(action=export, resource_type=customer) BEFORE the stream; a failed row means 500 and NO bytes. Metadata carries the date scope + filename; no row count (unknowable pre-stream).")),
	{"GET", "/v1/exports/invoices.csv"}:      realTime().with(explicit("exportsHandler.exportInvoices → auditExport(action=export, resource_type=invoice) BEFORE the stream, fail-closed.")),
	{"GET", "/v1/exports/subscriptions.csv"}: realTime().with(explicit("exportsHandler.exportSubscriptions → auditExport(action=export, resource_type=subscription) BEFORE the stream, fail-closed.")),
	{"GET", "/v1/exports/usage-events.csv"}:  realTime().with(explicit("exportsHandler.exportUsageEvents → auditExport(action=export, resource_type=usage_event) BEFORE the stream, fail-closed. The from/to range is required by the route and lands in the row's metadata.")),
	{"GET", "/v1/analytics/mrr-movement"}:    realTime(),
	{"GET", "/v1/analytics/overview"}:        realTime(),
	{"GET", "/v1/analytics/revenue-chart"}:   realTime(),
	{"GET", "/v1/analytics/usage"}:           realTime(),
	{"GET", "/v1/audit-log"}:                 realTime(),
	{"GET", "/v1/audit-log/filters"}:         realTime(),

	// --- pricing, settings and tooling: on no clock ----------------------
	{"GET", "/v1/meters"}:            realTime(),
	{"POST", "/v1/meters"}:           realTime().with(explicit("pricing.Handler.createMeter → Log(create, meter).")),
	{"GET", "/v1/meters/{id}"}:       realTime(),
	{"PATCH", "/v1/meters/{id}"}:     realTime().with(explicit("pricing.Service.UpdateMeter → LogInTx(update, meter) on the meter's own tx.")),
	{"GET", "/v1/plans"}:             realTime(),
	{"POST", "/v1/plans"}:            realTime().with(explicit("pricing.Handler.createPlan → Log(create, plan).")),
	{"GET", "/v1/plans/{id}"}:        realTime(),
	{"PATCH", "/v1/plans/{id}"}:      realTime().with(explicit("pricing.Handler.updatePlan → Log(update, plan).")),
	{"GET", "/v1/plans/{id}/prices"}: realTime(),
	// A price edit reaches subscriptions on many clocks. Its SQL stamps each
	// clock's own time under a lock that orders it against an advance.
	{"POST", "/v1/plans/{id}/prices"}:              realTime().with(explicit("pricing.Service.AddPrice → LogInTx(create, plan_price) on the price write's tx, with the content and when it applies.")),
	{"PATCH", "/v1/plans/{id}/prices/{price_id}"}:  realTime().with(explicit("pricing.Service.EditPrice → LogInTx(update, plan_price) on the price write's tx, with the content before and after.")),
	{"DELETE", "/v1/plans/{id}/prices/{price_id}"}: realTime().with(explicit("pricing.Service.RemovePrice → LogInTx(delete, plan_price) on the price write's tx, with the overrides it ended.")),
	// An override reads its customer's clock in its own SQL.
	{"GET", "/v1/price-overrides"}:         realTime(),
	{"POST", "/v1/price-overrides"}:        realTime().with(explicit("pricing.Handler.createOverride → Log(create, price_override).")),
	{"DELETE", "/v1/price-overrides/{id}"}: realTime().with(explicit("pricing.Handler.deleteOverride → Log(delete, price_override).")),
	{"GET", "/v1/provider-costs"}:          realTime(),
	{"PUT", "/v1/provider-costs"}:          realTime().with(explicit("usage.ProviderCostHandler.upsert → LogInTx(update, provider_cost) on the upsert tx.")),
	{"DELETE", "/v1/provider-costs/{id}"}:  realTime().with(explicit("usage.ProviderCostHandler.delete → LogInTx(delete, provider_cost) on the delete tx.")),
	{"GET", "/v1/recipes"}:                 realTime(),
	{"GET", "/v1/recipes/instances"}:       realTime(),
	{"GET", "/v1/recipes/{key}"}:           realTime(),
	// plans, meters and a dunning policy
	{"POST", "/v1/recipes/{key}/instantiate"}: realTime().with(explicit("recipe.Service.Instantiate → LogInTx(create, recipe) on the generate-plan tx (ADR-085 born-unique install). An idempotent re-apply installs nothing, emits nothing, and declares audit.MarkSkip on the badge-exists branch — a no-op deserves no record (the catch-all fabricated a 'created recipe' row for it).")),
	{"POST", "/v1/recipes/{key}/preview"}:     realTime().with(exempt(reasonNonMutatingPreview, "Read-only recipe render — returns the objects an instantiate WOULD create. Writes nothing (recipe/handler.go calls audit.MarkSkip).")),
	{"GET", "/v1/settings"}:                   realTime(),
	{"PUT", "/v1/settings"}:                   realTime().with(explicit("tenant.SettingsHandler.upsert → Log(update, setting) with a FIELD-LEVEL diff. A save that changes no field mutates nothing semantically and declares MarkSkip.")),
	{"GET", "/v1/settings/email"}:             realTime(),
	{"POST", "/v1/settings/email/test"}:       realTime().with(explicit("api.emailSettingsHandler.sendTest → Log(send, setting: smtp; metadata action=test_email_sent; no address) after a successful SMTP send to the signed-in member.")),
	{"GET", "/v1/settings/stripe"}:            realTime(),
	// Connecting Stripe retries every invoice stuck on the missing key, each
	// on its own clock.
	{"POST", "/v1/settings/stripe"}:                                                 decidedIn("invoice.Service.RetryProviderConfigErrors").with(explicit("tenantstripe.Handler.connect → Log(create, stripe_credentials) — never records the keys.")),
	{"DELETE", "/v1/settings/stripe/{mode}"}:                                        realTime().with(explicit("tenantstripe.Handler.delete → Log(delete, stripe_credentials).")),
	{"PATCH", "/v1/settings/stripe/{mode}/webhook"}:                                 realTime().with(explicit("tenantstripe.Handler.setWebhook → Log(rotate, stripe_credentials: webhook_secret_set) — never records the secret.")),
	{"GET", "/v1/dunning/policies"}:                                                 realTime(),
	{"POST", "/v1/dunning/policies"}:                                                realTime().with(explicit("dunning.Handler.createPolicy → Log(create, dunning_policy).")),
	{"GET", "/v1/dunning/policies/{id}"}:                                            realTime(),
	{"PATCH", "/v1/dunning/policies/{id}"}:                                          realTime().with(explicit("dunning.Handler.updatePolicy → Log(update, dunning_policy).")),
	{"DELETE", "/v1/dunning/policies/{id}"}:                                         realTime().with(explicit("dunning.Handler.deletePolicy → Log(delete, dunning_policy).")),
	{"POST", "/v1/dunning/policies/{id}/set-default"}:                               realTime().with(explicit("dunning.Handler.setDefaultPolicy → Log(update, dunning_policy: set_default).")),
	{"GET", "/v1/dunning/stats"}:                                                    realTime(),
	{"GET", "/v1/webhook-endpoints/endpoints"}:                                      realTime(),
	{"POST", "/v1/webhook-endpoints/endpoints"}:                                     realTime().with(explicit("webhook.Handler.createEndpoint → Log(create, webhook_endpoint).")).showsSecretOnce(),
	{"GET", "/v1/webhook-endpoints/endpoints/stats"}:                                realTime(),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}"}:                                 realTime(),
	{"PATCH", "/v1/webhook-endpoints/endpoints/{id}"}:                               realTime().with(explicit("webhook.Handler.updateEndpoint → Log(update, webhook_endpoint).")),
	{"DELETE", "/v1/webhook-endpoints/endpoints/{id}"}:                              realTime().with(explicit("webhook.Handler.deleteEndpoint → Log(delete, webhook_endpoint).")),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}/deliveries"}:                      realTime(),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/deliveries/{deliveryID}/replay"}: realTime().with(explicit("webhook.Handler.replayDelivery → Log(update, webhook_delivery: replayed_to_endpoint) — records target endpoint + replay clone ids.")),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/rotate-secret"}:                  realTime().with(explicit("webhook.Handler.rotateSecret → Log(rotate, webhook_endpoint) — never records the secret.")).showsSecretOnce(),
	{"GET", "/v1/webhook-endpoints/events"}:                                         realTime(),
	{"GET", "/v1/webhook_events/{id}/deliveries"}:                                   realTime(),
	{"POST", "/v1/webhook_events/{id}/replay"}:                                      realTime().with(explicit("webhook.Handler.replayEventV2 → Log(update, webhook_event: replayed).")),
	// A clock itself: the advance decides each pass's time per clock.
	//
	// The clock routes stamp their audit rows on their own clock's simulated
	// time (the sim axis, ADR-090 §5). So the clock's lifecycle sits inside the
	// timeline it explains. Otherwise, after teardown (ADR-086), a
	// clock-scoped query would return the effects (invoices finalized,
	// subscriptions canceled) with no cause. It would show no "created", no
	// "advanced to 2027-03-01" and no "deleted". The "deleted" row is the one
	// that explains why every other trace of the simulation is gone.
	{"GET", "/v1/test-clocks"}:                     realTime(),
	{"POST", "/v1/test-clocks"}:                    realTime().with(explicit("testclock.Service.Create → LogInTx(create, test_clock) on the clock's insert tx.")),
	{"GET", "/v1/test-clocks/{id}"}:                realTime(),
	{"DELETE", "/v1/test-clocks/{id}"}:             realTime().with(explicit("testclock.Service.Delete → LogInTx(delete, test_clock: teardown) on the teardown tx, named from the clock row it deletes. ADR-086: the audit row is the simulation's only record after teardown.")),
	{"POST", "/v1/test-clocks/{id}/advance"}:       realTime().with(explicit("testclock.Service.Advance → LogInTx(update, test_clock; metadata action=advanced, advanced_from read by the same statement) on the tx that marks the clock advancing.")),
	{"POST", "/v1/test-clocks/{id}/retry-advance"}: realTime().with(explicit("testclock.Service.RetryAdvance → LogInTx(update, test_clock; metadata action=retry_advance) on the tx that marks the clock advancing again.")),
	{"GET", "/v1/test-clocks/{id}/customers"}:      realTime(),
	// Bills the tenant's due subscriptions, which never include a clock's.
	{"POST", "/v1/billing/run"}: realTime().with(explicit("billing.Handler.triggerCycle → Log(run, billing) — the operator's TRIGGER row: it records WHO ran a cycle and how many invoices it produced, including a run that produced ZERO (which leaves no finalize rows at all). NOT because the finalize rows are anonymous — triggerCycle runs synchronously on the request ctx, so those rows do carry the operator's actor/IP/request_id, unlike the scheduler's. Residual own-tx by design: the route owns no tx. A failed emission is surfaced in the response and left unmarked, so the detector reports it.")),

	// --- customers --------------------------------------------------------
	{"GET", "/v1/customers"}: realTime(),
	// the body names the clock
	{"POST", "/v1/customers"}:                                                decidedIn("customer.Service.Create").with(explicit("customer.Service.Create → LogInTx(create, customer) on the customer's own tx.")),
	{"GET", "/v1/customers/{id}"}:                                            ofCustomer("id"),
	{"PATCH", "/v1/customers/{id}"}:                                          ofCustomer("id").with(explicit("customer.Handler.update → Log(update, customer) with a field diff.")),
	{"GET", "/v1/customers/{id}/billing-profile"}:                            ofCustomer("id"),
	{"PUT", "/v1/customers/{id}/billing-profile"}:                            ofCustomer("id").with(explicit("customer.Handler.upsertBillingProfile → Log(update, customer).")),
	{"GET", "/v1/customers/{id}/margin"}:                                     ofCustomer("id"),
	{"POST", "/v1/customers/{id}/rotate-cost-dashboard-token"}:               ofCustomer("id").with(explicit("customer.Handler.rotateCostDashboardToken → Log(rotate, customer) — never records the plaintext token.")).showsSecretOnce(),
	{"GET", "/v1/customers/{id}/sent-emails"}:                                ofCustomer("id"),
	{"GET", "/v1/customers/{id}/usage"}:                                      ofCustomer("id"),
	{"GET", "/v1/customers/{customer_id}/overview"}:                          ofCustomer("customer_id"),
	{"GET", "/v1/customers/{customer_id}/payment-methods"}:                   ofCustomer("customer_id"),
	{"POST", "/v1/customers/{customer_id}/payment-methods/send-setup-email"}: ofCustomer("customer_id").with(explicit("paymentmethods.Handler.operatorSendSetupEmail → Log(update, customer: setup_link_sent). No recipient address on the row (GDPR erasure) — email_outbox is the delivery record.")),
	{"POST", "/v1/customers/{customer_id}/payment-methods/setup-session"}:    ofCustomer("customer_id").with(explicit("paymentmethods.Service.CreateSetupSession → Log(update, customer: setup_session_created).")).showsSecretOnce(),
	{"DELETE", "/v1/customers/{customer_id}/payment-methods/{pmID}"}:         ofCustomer("customer_id").with(explicit("paymentmethods.Service.Detach → Log(delete, payment_method) + Log(update) on any promoted new default.")),
	{"POST", "/v1/customers/{customer_id}/payment-methods/{pmID}/default"}:   ofCustomer("customer_id").with(explicit("paymentmethods.Service.SetDefault → Log(update, payment_method).")),

	// --- subscriptions ----------------------------------------------------
	{"GET", "/v1/subscriptions"}: realTime(),
	// the body names the customer
	{"POST", "/v1/subscriptions"}:                                      decidedIn("subscription.Handler.create").with(explicit("subscription.Handler.create → Log(create, subscription).")),
	{"GET", "/v1/subscriptions/{id}"}:                                  ofSubscription(),
	{"PUT", "/v1/subscriptions/{id}/billing-thresholds"}:               ofSubscription().with(explicit("subscription.Handler.setBillingThresholds → Log(update, subscription).")),
	{"DELETE", "/v1/subscriptions/{id}/billing-thresholds"}:            ofSubscription().with(explicit("subscription.Handler.clearBillingThresholds → Log(update, subscription).")),
	{"POST", "/v1/subscriptions/{id}/cancel"}:                          ofSubscription().with(explicit("subscription.Service.Cancel → LogInTx(cancel, subscription) on the cancel tx (single writer: operator route AND dunning's terminal cancel).")),
	{"POST", "/v1/subscriptions/{id}/end-trial"}:                       ofSubscription().with(explicit("subscription.Handler.endTrial → Log(update, subscription: trial_ended).")),
	{"POST", "/v1/subscriptions/{id}/extend-trial"}:                    ofSubscription().with(explicit("subscription.Handler.extendTrial → Log(update, subscription: trial_extended).")),
	{"POST", "/v1/subscriptions/{id}/items"}:                           ofSubscription().with(explicit("subscription.Handler.addItem → Log(update, subscription).")),
	{"PATCH", "/v1/subscriptions/{id}/items/{itemID}"}:                 ofSubscription().with(explicit("subscription.Handler.updateItem → Log(subscription.item_updated).")),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}"}:                ofSubscription().with(explicit("subscription.Handler.removeItem → Log(update, subscription).")),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}/pending-change"}: ofSubscription().with(explicit("subscription.Handler.cancelPendingItemChange → Log(update, subscription; metadata action=cancel_pending_item_plan_change). A call that canceled no change calls audit.MarkSkip.")),
	{"PUT", "/v1/subscriptions/{id}/pause-collection"}:                 ofSubscription().with(explicit("subscription.Service.PauseCollection → Log(update, subscription; metadata action=collection_paused). The HANDLER emits nothing — the row is the service's.")),
	{"DELETE", "/v1/subscriptions/{id}/pause-collection"}:              ofSubscription().with(explicit("subscription.Service.ResumeCollection → Log(update, subscription; metadata action=collection_resumed). The HANDLER emits nothing — the row is the service's.")),
	{"POST", "/v1/subscriptions/{id}/retry-billing"}:                   ofSubscription().with(explicit("subscription.Service.RetryBilling → LogInTx(update, subscription; metadata action=billing_retried) on the transaction that clears the failure record; a subscription with no record answers 409 and emits nothing.")),
	{"POST", "/v1/subscriptions/{id}/schedule-cancel"}:                 ofSubscription().with(explicit("subscription.Handler.scheduleCancel → Log(update, subscription: cancel_scheduled).")),
	{"DELETE", "/v1/subscriptions/{id}/scheduled-cancel"}:              ofSubscription().with(explicit("subscription.Handler.clearScheduledCancel → Log(update, subscription; metadata action=cancel_cleared).")),
	{"GET", "/v1/subscriptions/{id}/timeline"}:                         ofSubscription(),

	// --- usage --------------------------------------------------------------
	// The body names the customer by its external id; a batch or a LiteLLM
	// callback names many, each decided per event.
	//
	// Accepted loss, for the three exempt ingest routes: "which credential
	// ingested this event" cannot be recovered, because usage_events has no
	// key_id column. The events tables record what was ingested, but not by
	// whom. Close this loss at the first billing dispute that turns on
	// provenance. The fix is a key_id column on usage_events, so each event
	// records the credential that sent it, not a per-request audit row. At
	// ingest volumes, one audit row per call would outweigh the events
	// themselves.
	{"POST", "/v1/usage-events"}:               decidedIn("usage.Service.Ingest").with(exempt(reasonMachineIngest, "High-volume metering ingest (~1000/s); usage_events IS the record, and a row per event would double the write volume of the hottest path. ACCEPTED LOSS, stated precisely: until this PR the retired catch-all wrote a generic 'create usage_event' row per REQUEST, so these routes DO lose audit_log rows they were getting — what is lost with them is (a) WHICH CREDENTIAL ingested an event (usage_events carries no key_id) and (b) the request-level trail. The events themselves, their timestamps and their origin tag remain. Closure trigger: a billing dispute or an injected-usage incident → stamp key_id at ingest (plumb the fact; no heuristics).")),
	{"POST", "/v1/usage-events/batch"}:         decidedIn("usage.Service.BatchIngest").with(exempt(reasonMachineIngest, "Batch form of the same ingest. Same accepted loss.")),
	{"POST", "/v1/integrations/litellm/spend"}: decidedIn("usage.Service.IngestCall").with(exempt(reasonMachineIngest, "LiteLLM proxy spend callback — one POST per LLM call, funnels into the same usage_events writer. Same accepted loss.")),
	// Backfill is not exempt, on purpose: it is an operator action on the
	// money path. Inserting backdated usage changes what a customer is billed
	// for a period that may already have closed, unlike the machine metering
	// above. usage.Service.Backfill emits on the ingest transaction
	// (ADR-090).
	{"POST", "/v1/usage-events/backfill"}: decidedIn("usage.Service.Backfill").with(explicit("usage.Service.Backfill → IngestAudited (in-tx): action=create/usage_event, metadata.action=usage_backfilled with the backdated timestamp.")),
	// Held usage (#1044, ADR-122): operator actions on rows the LiteLLM
	// callback could not ingest. Release changes what a customer is billed,
	// as backfill does. Discard decides that the usage will never bill.
	{"POST", "/v1/usage-events/held/release"}: decidedIn("usage.Service.Release").with(explicit("usage.Service.Release → PostgresStore.ReleaseHeld (in-tx): action=create/usage_event, metadata.action=held_usage_released with the counts and the event time range. A call that changed no row calls audit.MarkSkip.")),
	// Discard reads no clock.
	{"POST", "/v1/usage-events/held/discard"}: realTime().with(explicit("usage.Service.Discard → PostgresStore.DiscardHeld (in-tx): action=delete/usage_event, metadata.action=held_usage_discarded with the row count. A call that changed no row calls audit.MarkSkip.")),
	{"GET", "/v1/usage-events"}:               realTime(),
	{"GET", "/v1/usage-events/aggregate"}:     realTime(),
	{"GET", "/v1/usage-events/held"}:          realTime(),

	// --- invoices -----------------------------------------------------------
	{"GET", "/v1/invoices"}: realTime(),
	// the body names the customer
	{"POST", "/v1/invoices"}: decidedIn("invoice.Service.Create").with(explicit("invoice.Service.Create → LogInTx(create, invoice) on the invoice's own tx.")),
	// the body names the customer
	{"POST", "/v1/invoices/create_preview"}:                                 decidedIn("billing.PreviewService.CreatePreview").with(exempt(reasonNonMutatingPreview, "Read-only estimate; POST only because the composer payload doesn't fit a query string. Writes nothing (billing/create_preview_handler.go calls audit.MarkSkip).")),
	{"GET", "/v1/invoices/{id}"}:                                            ofInvoice(),
	{"POST", "/v1/invoices/{id}/collect"}:                                   ofInvoice().with(explicit("invoice.Handler.collectPayment → Log(collect, invoice) — the money-movement row.")),
	{"GET", "/v1/invoices/{id}/disputes"}:                                   ofInvoice(),
	{"POST", "/v1/invoices/{id}/finalize"}:                                  ofInvoice().with(explicit("invoice.Service.Finalize → LogInTx(finalize, invoice) on the finalize's own tx (finalizeHook).")),
	{"POST", "/v1/invoices/{id}/line-items"}:                                ofInvoice().with(explicit("invoice.Service.AddLineItem → LogInTx(line_item_added) on the invoice tx. The catch-all wrote 'create invoice {id}' here — a row asserting an invoice the operator never created.")),
	{"POST", "/v1/invoices/{id}/mark-uncollectible"}:                        ofInvoice().with(explicit("invoice.Service.MarkUncollectible → Log(update, invoice). Single service entry for operator + dunning terminal + resolve paths.")),
	{"GET", "/v1/invoices/{id}/payment-timeline"}:                           ofInvoice(),
	{"GET", "/v1/invoices/{id}/pdf"}:                                        ofInvoice(),
	{"POST", "/v1/invoices/{id}/record-payment"}:                            ofInvoice().with(explicit("invoice.Service.RecordOfflinePayment → Log(update, invoice: payment_recorded).")),
	{"POST", "/v1/invoices/{id}/refund"}:                                    ofInvoice().with(explicit("invoice.Handler.refund → Log(refund, invoice); the credit note carries its own issued row.")),
	{"POST", "/v1/invoices/{id}/resend-setup-link"}:                         ofInvoice().with(explicit("invoice.Handler.resendSetupLink → Log(send, invoice; metadata action=resend_setup_link).")),
	{"POST", "/v1/invoices/{id}/retry-tax"}:                                 ofInvoice().with(explicit("invoice.Handler.retryTax → Log(retry_tax, invoice).")),
	{"POST", "/v1/invoices/{id}/rotate-public-token"}:                       ofInvoice().with(explicit("invoice.Handler.rotatePublicToken → Log(rotate, invoice) — never records the token.")),
	{"POST", "/v1/invoices/{id}/send"}:                                      ofInvoice().with(explicit("invoice.Handler.sendEmail → Log(send, invoice).")),
	{"GET", "/v1/invoices/{id}/stripe-refunds"}:                             ofInvoice(),
	{"POST", "/v1/invoices/{id}/stripe-refunds/{stripe_refund_id}/dismiss"}: ofInvoice().with(explicit("creditnote.Service.DismissStripeRefund → LogInTx(update, invoice, action stripe_refund_dismissed) on the dismissal's tx (ADR-134).")),
	{"POST", "/v1/invoices/{id}/void"}:                                      ofInvoice().with(explicit("invoice.Service.Void → LogInTx(void, invoice) on the void coordinator tx (tax reversal + credit reversal share it).")),

	// --- credit notes -------------------------------------------------------
	{"GET", "/v1/credit-notes"}:       realTime(),
	{"GET", "/v1/credit-notes/stats"}: realTime(),
	// the body names the invoice
	{"POST", "/v1/credit-notes"}:                           decidedIn("creditnote.Handler.create").with(explicit("creditnote.Service.Create → LogInTx(create, credit_note) on the draft tx.")),
	{"GET", "/v1/credit-notes/{id}"}:                       ofCreditNote(),
	{"POST", "/v1/credit-notes/{id}/attach-stripe-refund"}: ofCreditNote().with(explicit("creditnote.Service.AttachStripeRefund → LogInTx(update, credit_note, action stripe_refund_attached) on the attach's tx (ADR-134).")),
	{"POST", "/v1/credit-notes/{id}/issue"}:                ofCreditNote().with(explicit("creditnote.Service.Issue → LogInTx(credit_note.issued) on the coordinator tx (+ the grant's own row). A DEFERRED outcome mutates nothing and declares MarkSkip.")),
	{"GET", "/v1/credit-notes/{id}/pdf"}:                   ofCreditNote(),
	{"POST", "/v1/credit-notes/{id}/retry-refund"}:         ofCreditNote().with(explicit("creditnote.Service.RetryRefund → LogInTx on the refund-status persist tx, UNCONDITIONALLY: the audit-worthy fact is the operator action (a real cash-back request against the customer's payment), not the state transition, so a converged re-drive still records a row — carrying status_changed=false. Deliberately the OPPOSITE of ApplyRefundWebhook, where a no-op redelivery must record nothing.")),
	{"POST", "/v1/credit-notes/{id}/send"}:                 ofCreditNote().with(explicit("creditnote.Handler.sendEmail → Log(send, credit_note).")),
	{"POST", "/v1/credit-notes/{id}/void"}:                 ofCreditNote().with(explicit("creditnote.Service.Void → LogInTx(void, credit_note) on the draft→voided CAS tx.")),

	// --- credits ------------------------------------------------------------
	{"GET", "/v1/credits/balances"}: realTime(),
	// the body names the customer
	{"POST", "/v1/credits/grant"}: decidedIn("credit.Service.Grant").with(explicit("credit.Service.Grant → LogInTx(grant, credit) on the ledger tx (ADR-090's first domain).")),
	// the body names the customer
	{"POST", "/v1/credits/adjust"}:                    decidedIn("credit.Service.Adjust").with(explicit("credit.Service.Adjust → LogInTx(resource=credit) on the ledger tx. Action is credit.adjustment for a positive amount and credit.deduction for a negative one (the split the dashboard's severity styling keys on). There is no 'adjust' action in the vocabulary.")),
	{"GET", "/v1/credits/balance/{customer_id}"}:      ofCustomer("customer_id"),
	{"GET", "/v1/credits/balance/{customer_id}/live"}: ofCustomer("customer_id"),
	{"GET", "/v1/credits/grants/{customer_id}"}:       ofCustomer("customer_id"),
	{"GET", "/v1/credits/ledger/{customer_id}"}:       ofCustomer("customer_id"),

	// --- dunning runs -------------------------------------------------------
	{"GET", "/v1/dunning/runs"}:      realTime(),
	{"GET", "/v1/dunning/runs/{id}"}: ofDunningRun(),
}
