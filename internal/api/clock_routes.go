package api

// ---------------------------------------------------------------------------
// The clock route table (test-clock step 3: decide the time at the door).
//
// Every route states how a request's time is decided, once, before its
// handler runs. It is real time, or the time of the test clock the request's
// record is on, or, where the request does not name its record by a path
// parameter, the time a named function decides after it reads the record.
// Only test mode has clocks, so a live request is always real time; the
// table says what a test-mode request gets.
//
// The door looks a request up by (method, canonical route pattern), from
// Find on the built router: Find gives every route's pattern and fills its
// path parameters before the mounted sub-router runs
// (TestRouter_FindGivesEveryRoutesPatternAndParams).
//
// TestClockRoutes_MatchLiveRoutes walks the router and compares it with this
// table in both directions: a route with no declaration fails, and so does a
// declaration with no route. So adding a route forces a decision about its
// time at review.
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

// clockDecl is one route's declaration.
type clockDecl struct {
	Source clockSource
	// Param is the path parameter that names the record, for the clockOf
	// sources.
	Param string
	// In is the function that decides, as package.Type.Method, for
	// clockDecidedIn.
	In string
}

func realTime() clockDecl                 { return clockDecl{Source: clockRealTime} }
func ofCustomer(param string) clockDecl   { return clockDecl{Source: clockOfCustomer, Param: param} }
func ofSubscription() clockDecl           { return clockDecl{Source: clockOfSubscription, Param: "id"} }
func ofInvoice() clockDecl                { return clockDecl{Source: clockOfInvoice, Param: "id"} }
func ofCreditNote() clockDecl             { return clockDecl{Source: clockOfCreditNote, Param: "id"} }
func ofDunningRun() clockDecl             { return clockDecl{Source: clockOfDunningRun, Param: "id"} }
func decidedIn(function string) clockDecl { return clockDecl{Source: clockDecidedIn, In: function} }

// clockRoutes is the table. Keys are canonical (method, route pattern), as in
// auditRouteRegistry.
var clockRoutes = map[routeKey]clockDecl{
	// --- system ---------------------------------------------------------
	// /metrics is registered with r.Handle, which chi expands to every method.
	{"GET", "/health"}: realTime(), {"GET", "/health/ready"}: realTime(),
	{"GET", "/metrics"}: realTime(), {"HEAD", "/metrics"}: realTime(), {"POST", "/metrics"}: realTime(),
	{"PUT", "/metrics"}: realTime(), {"PATCH", "/metrics"}: realTime(), {"DELETE", "/metrics"}: realTime(),
	{"CONNECT", "/metrics"}: realTime(), {"OPTIONS", "/metrics"}: realTime(), {"TRACE", "/metrics"}: realTime(),
	{"QUERY", "/metrics"}: realTime(),

	// --- sign-in and setup: users and tenants are on no clock ------------
	{"POST", "/v1/bootstrap"}:                   realTime(),
	{"POST", "/v1/auth/login"}:                  realTime(),
	{"POST", "/v1/auth/logout"}:                 realTime(),
	{"POST", "/v1/auth/mode"}:                   realTime(),
	{"GET", "/v1/auth/password-reset/check"}:    realTime(),
	{"POST", "/v1/auth/password-reset/confirm"}: realTime(),
	{"POST", "/v1/auth/password-reset/request"}: realTime(),
	{"GET", "/v1/auth/invite/{token}"}:          realTime(),
	{"POST", "/v1/auth/accept-invite"}:          realTime(),
	{"GET", "/v1/whoami"}:                       realTime(),
	{"GET", "/v1/members"}:                      realTime(),
	{"POST", "/v1/members/invite"}:              realTime(),
	{"DELETE", "/v1/members/invitations/{id}"}:  realTime(),
	{"DELETE", "/v1/members/{userID}"}:          realTime(),
	{"GET", "/v1/api-keys"}:                     realTime(),
	{"POST", "/v1/api-keys"}:                    realTime(),
	{"DELETE", "/v1/api-keys/{id}"}:             realTime(),
	{"POST", "/v1/api-keys/{id}/rotate"}:        realTime(),

	// --- inbound webhooks -----------------------------------------------
	// The endpoint names the tenant and mode; the customer is known only
	// after the event is read. Each event handler decides from the record it
	// names, not the settle, which already-decided paths also reach.
	{"POST", "/v1/webhooks/stripe/{endpoint_id}"}: decidedIn("payment.Stripe.HandleWebhook"),
	{"POST", "/v1/webhooks/postmark"}:             realTime(), // email reports; reads no clock

	// --- public pages: the token names the record, and the mode is not
	// known until it is read ----------------------------------------------
	{"GET", "/v1/public/invoices/{token}"}:                  decidedIn("hostedinvoice.Handler.resolveInvoice"),
	{"POST", "/v1/public/invoices/{token}/checkout"}:        decidedIn("hostedinvoice.Handler.resolveInvoice"),
	{"GET", "/v1/public/invoices/{token}/pdf"}:              decidedIn("hostedinvoice.Handler.resolveInvoice"),
	{"GET", "/v1/public/payment-updates/{token}"}:           realTime(), // writes nothing
	{"POST", "/v1/public/payment-updates/{token}/checkout"}: realTime(), // two audit rows, no money
	{"GET", "/v1/public/cost-dashboard/{token}"}:            decidedIn("usage.CostDashboardAssembler.GetByToken"),

	// --- tenant-wide reads and streams: each row by its stored dates ------
	{"GET", "/v1/webhook_events/stream"}:     realTime(),
	{"GET", "/v1/exports/audit-log.csv"}:     realTime(),
	{"GET", "/v1/exports/customers.csv"}:     realTime(),
	{"GET", "/v1/exports/invoices.csv"}:      realTime(),
	{"GET", "/v1/exports/subscriptions.csv"}: realTime(),
	{"GET", "/v1/exports/usage-events.csv"}:  realTime(),
	{"GET", "/v1/analytics/mrr-movement"}:    realTime(),
	{"GET", "/v1/analytics/overview"}:        realTime(),
	{"GET", "/v1/analytics/revenue-chart"}:   realTime(),
	{"GET", "/v1/analytics/usage"}:           realTime(),
	{"GET", "/v1/audit-log"}:                 realTime(),
	{"GET", "/v1/audit-log/filters"}:         realTime(),

	// --- pricing, settings and tooling: on no clock ----------------------
	{"GET", "/v1/meters"}:            realTime(),
	{"POST", "/v1/meters"}:           realTime(),
	{"GET", "/v1/meters/{id}"}:       realTime(),
	{"PATCH", "/v1/meters/{id}"}:     realTime(),
	{"GET", "/v1/plans"}:             realTime(),
	{"POST", "/v1/plans"}:            realTime(),
	{"GET", "/v1/plans/{id}"}:        realTime(),
	{"PATCH", "/v1/plans/{id}"}:      realTime(),
	{"GET", "/v1/plans/{id}/prices"}: realTime(),
	// A price edit reaches subscriptions on many clocks. Its SQL stamps each
	// clock's own time under a lock that orders it against an advance.
	{"POST", "/v1/plans/{id}/prices"}:              realTime(),
	{"PATCH", "/v1/plans/{id}/prices/{price_id}"}:  realTime(),
	{"DELETE", "/v1/plans/{id}/prices/{price_id}"}: realTime(),
	// An override reads its customer's clock in its own SQL.
	{"GET", "/v1/price-overrides"}:            realTime(),
	{"POST", "/v1/price-overrides"}:           realTime(),
	{"DELETE", "/v1/price-overrides/{id}"}:    realTime(),
	{"GET", "/v1/provider-costs"}:             realTime(),
	{"PUT", "/v1/provider-costs"}:             realTime(),
	{"DELETE", "/v1/provider-costs/{id}"}:     realTime(),
	{"GET", "/v1/recipes"}:                    realTime(),
	{"GET", "/v1/recipes/instances"}:          realTime(),
	{"GET", "/v1/recipes/{key}"}:              realTime(),
	{"POST", "/v1/recipes/{key}/instantiate"}: realTime(), // plans, meters and a dunning policy
	{"POST", "/v1/recipes/{key}/preview"}:     realTime(),
	{"GET", "/v1/settings"}:                   realTime(),
	{"PUT", "/v1/settings"}:                   realTime(),
	{"GET", "/v1/settings/email"}:             realTime(),
	{"POST", "/v1/settings/email/test"}:       realTime(),
	{"GET", "/v1/settings/stripe"}:            realTime(),
	// Connecting Stripe retries every invoice stuck on the missing key, each
	// on its own clock.
	{"POST", "/v1/settings/stripe"}:                                                 decidedIn("invoice.Service.RetryProviderConfigErrors"),
	{"DELETE", "/v1/settings/stripe/{mode}"}:                                        realTime(),
	{"PATCH", "/v1/settings/stripe/{mode}/webhook"}:                                 realTime(),
	{"GET", "/v1/dunning/policies"}:                                                 realTime(),
	{"POST", "/v1/dunning/policies"}:                                                realTime(),
	{"GET", "/v1/dunning/policies/{id}"}:                                            realTime(),
	{"PATCH", "/v1/dunning/policies/{id}"}:                                          realTime(),
	{"DELETE", "/v1/dunning/policies/{id}"}:                                         realTime(),
	{"POST", "/v1/dunning/policies/{id}/set-default"}:                               realTime(),
	{"GET", "/v1/dunning/stats"}:                                                    realTime(),
	{"GET", "/v1/webhook-endpoints/endpoints"}:                                      realTime(),
	{"POST", "/v1/webhook-endpoints/endpoints"}:                                     realTime(),
	{"GET", "/v1/webhook-endpoints/endpoints/stats"}:                                realTime(),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}"}:                                 realTime(),
	{"PATCH", "/v1/webhook-endpoints/endpoints/{id}"}:                               realTime(),
	{"DELETE", "/v1/webhook-endpoints/endpoints/{id}"}:                              realTime(),
	{"GET", "/v1/webhook-endpoints/endpoints/{id}/deliveries"}:                      realTime(),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/deliveries/{deliveryID}/replay"}: realTime(),
	{"POST", "/v1/webhook-endpoints/endpoints/{id}/rotate-secret"}:                  realTime(),
	{"GET", "/v1/webhook-endpoints/events"}:                                         realTime(),
	{"GET", "/v1/webhook_events/{id}/deliveries"}:                                   realTime(),
	{"POST", "/v1/webhook_events/{id}/replay"}:                                      realTime(),
	// A clock itself: the advance decides each pass's time per clock.
	{"GET", "/v1/test-clocks"}:                     realTime(),
	{"POST", "/v1/test-clocks"}:                    realTime(),
	{"GET", "/v1/test-clocks/{id}"}:                realTime(),
	{"DELETE", "/v1/test-clocks/{id}"}:             realTime(),
	{"POST", "/v1/test-clocks/{id}/advance"}:       realTime(),
	{"POST", "/v1/test-clocks/{id}/retry-advance"}: realTime(),
	{"GET", "/v1/test-clocks/{id}/customers"}:      realTime(),
	// Bills the tenant's due subscriptions, which never include a clock's.
	{"POST", "/v1/billing/run"}: realTime(),

	// --- customers --------------------------------------------------------
	{"GET", "/v1/customers"}:                                                 realTime(),
	{"POST", "/v1/customers"}:                                                decidedIn("customer.Service.Create"), // the body names the clock
	{"GET", "/v1/customers/{id}"}:                                            ofCustomer("id"),
	{"PATCH", "/v1/customers/{id}"}:                                          ofCustomer("id"),
	{"GET", "/v1/customers/{id}/billing-profile"}:                            ofCustomer("id"),
	{"PUT", "/v1/customers/{id}/billing-profile"}:                            ofCustomer("id"),
	{"GET", "/v1/customers/{id}/margin"}:                                     ofCustomer("id"),
	{"POST", "/v1/customers/{id}/rotate-cost-dashboard-token"}:               ofCustomer("id"),
	{"GET", "/v1/customers/{id}/sent-emails"}:                                ofCustomer("id"),
	{"GET", "/v1/customers/{id}/usage"}:                                      ofCustomer("id"),
	{"GET", "/v1/customers/{customer_id}/overview"}:                          ofCustomer("customer_id"),
	{"GET", "/v1/customers/{customer_id}/payment-methods"}:                   ofCustomer("customer_id"),
	{"POST", "/v1/customers/{customer_id}/payment-methods/send-setup-email"}: ofCustomer("customer_id"),
	{"POST", "/v1/customers/{customer_id}/payment-methods/setup-session"}:    ofCustomer("customer_id"),
	{"DELETE", "/v1/customers/{customer_id}/payment-methods/{pmID}"}:         ofCustomer("customer_id"),
	{"POST", "/v1/customers/{customer_id}/payment-methods/{pmID}/default"}:   ofCustomer("customer_id"),

	// --- subscriptions ----------------------------------------------------
	{"GET", "/v1/subscriptions"}:                                       realTime(),
	{"POST", "/v1/subscriptions"}:                                      decidedIn("subscription.Handler.create"), // the body names the customer
	{"GET", "/v1/subscriptions/{id}"}:                                  ofSubscription(),
	{"POST", "/v1/subscriptions/{id}/activate"}:                        ofSubscription(),
	{"PUT", "/v1/subscriptions/{id}/billing-thresholds"}:               ofSubscription(),
	{"DELETE", "/v1/subscriptions/{id}/billing-thresholds"}:            ofSubscription(),
	{"POST", "/v1/subscriptions/{id}/cancel"}:                          ofSubscription(),
	{"POST", "/v1/subscriptions/{id}/end-trial"}:                       ofSubscription(),
	{"POST", "/v1/subscriptions/{id}/extend-trial"}:                    ofSubscription(),
	{"POST", "/v1/subscriptions/{id}/items"}:                           ofSubscription(),
	{"PATCH", "/v1/subscriptions/{id}/items/{itemID}"}:                 ofSubscription(),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}"}:                ofSubscription(),
	{"DELETE", "/v1/subscriptions/{id}/items/{itemID}/pending-change"}: ofSubscription(),
	{"PUT", "/v1/subscriptions/{id}/pause-collection"}:                 ofSubscription(),
	{"DELETE", "/v1/subscriptions/{id}/pause-collection"}:              ofSubscription(),
	{"POST", "/v1/subscriptions/{id}/retry-billing"}:                   ofSubscription(),
	{"POST", "/v1/subscriptions/{id}/schedule-cancel"}:                 ofSubscription(),
	{"DELETE", "/v1/subscriptions/{id}/scheduled-cancel"}:              ofSubscription(),
	{"GET", "/v1/subscriptions/{id}/timeline"}:                         ofSubscription(),

	// --- usage --------------------------------------------------------------
	// The body names the customer by its external id; a batch or a LiteLLM
	// callback names many, each decided per event.
	{"POST", "/v1/usage-events"}:               decidedIn("usage.Service.Ingest"),
	{"POST", "/v1/usage-events/backfill"}:      decidedIn("usage.Service.Backfill"),
	{"POST", "/v1/usage-events/batch"}:         decidedIn("usage.Service.BatchIngest"),
	{"POST", "/v1/usage-events/held/release"}:  decidedIn("usage.Service.Release"),
	{"POST", "/v1/integrations/litellm/spend"}: decidedIn("usage.Service.IngestCall"),
	{"POST", "/v1/usage-events/held/discard"}:  realTime(), // reads no clock
	{"GET", "/v1/usage-events"}:                realTime(),
	{"GET", "/v1/usage-events/aggregate"}:      realTime(),
	{"GET", "/v1/usage-events/held"}:           realTime(),

	// --- invoices -----------------------------------------------------------
	{"GET", "/v1/invoices"}:                                                 realTime(),
	{"POST", "/v1/invoices"}:                                                decidedIn("invoice.Service.Create"),               // the body names the customer
	{"POST", "/v1/invoices/create_preview"}:                                 decidedIn("billing.PreviewService.CreatePreview"), // the body names the customer
	{"GET", "/v1/invoices/{id}"}:                                            ofInvoice(),
	{"POST", "/v1/invoices/{id}/collect"}:                                   ofInvoice(),
	{"GET", "/v1/invoices/{id}/disputes"}:                                   ofInvoice(),
	{"POST", "/v1/invoices/{id}/finalize"}:                                  ofInvoice(),
	{"POST", "/v1/invoices/{id}/line-items"}:                                ofInvoice(),
	{"POST", "/v1/invoices/{id}/mark-uncollectible"}:                        ofInvoice(),
	{"GET", "/v1/invoices/{id}/payment-timeline"}:                           ofInvoice(),
	{"GET", "/v1/invoices/{id}/pdf"}:                                        ofInvoice(),
	{"POST", "/v1/invoices/{id}/record-payment"}:                            ofInvoice(),
	{"POST", "/v1/invoices/{id}/refund"}:                                    ofInvoice(),
	{"POST", "/v1/invoices/{id}/resend-setup-link"}:                         ofInvoice(),
	{"POST", "/v1/invoices/{id}/retry-tax"}:                                 ofInvoice(),
	{"POST", "/v1/invoices/{id}/rotate-public-token"}:                       ofInvoice(),
	{"POST", "/v1/invoices/{id}/send"}:                                      ofInvoice(),
	{"GET", "/v1/invoices/{id}/stripe-refunds"}:                             ofInvoice(),
	{"POST", "/v1/invoices/{id}/stripe-refunds/{stripe_refund_id}/dismiss"}: ofInvoice(),
	{"POST", "/v1/invoices/{id}/void"}:                                      ofInvoice(),

	// --- credit notes -------------------------------------------------------
	{"GET", "/v1/credit-notes"}:                            realTime(),
	{"GET", "/v1/credit-notes/stats"}:                      realTime(),
	{"POST", "/v1/credit-notes"}:                           decidedIn("creditnote.Handler.create"), // the body names the invoice
	{"GET", "/v1/credit-notes/{id}"}:                       ofCreditNote(),
	{"POST", "/v1/credit-notes/{id}/attach-stripe-refund"}: ofCreditNote(),
	{"POST", "/v1/credit-notes/{id}/issue"}:                ofCreditNote(),
	{"GET", "/v1/credit-notes/{id}/pdf"}:                   ofCreditNote(),
	{"POST", "/v1/credit-notes/{id}/retry-refund"}:         ofCreditNote(),
	{"POST", "/v1/credit-notes/{id}/send"}:                 ofCreditNote(),
	{"POST", "/v1/credit-notes/{id}/void"}:                 ofCreditNote(),

	// --- credits ------------------------------------------------------------
	{"GET", "/v1/credits/balances"}:                   realTime(),
	{"POST", "/v1/credits/grant"}:                     decidedIn("credit.Service.Grant"),  // the body names the customer
	{"POST", "/v1/credits/adjust"}:                    decidedIn("credit.Service.Adjust"), // the body names the customer
	{"GET", "/v1/credits/balance/{customer_id}"}:      ofCustomer("customer_id"),
	{"GET", "/v1/credits/balance/{customer_id}/live"}: ofCustomer("customer_id"),
	{"GET", "/v1/credits/grants/{customer_id}"}:       ofCustomer("customer_id"),
	{"GET", "/v1/credits/ledger/{customer_id}"}:       ofCustomer("customer_id"),

	// --- dunning runs -------------------------------------------------------
	{"GET", "/v1/dunning/runs"}:               realTime(),
	{"GET", "/v1/dunning/runs/{id}"}:          ofDunningRun(),
	{"POST", "/v1/dunning/runs/{id}/resolve"}: ofDunningRun(),
}
