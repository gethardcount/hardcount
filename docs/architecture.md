# Architecture

Hardcount is one Go server program (`cmd/hardcount`) on one PostgreSQL database, with a React dashboard in `web-v2/`. Many accounts share the database. The code calls an account a **tenant**.

CI and `docker compose` run PostgreSQL 16. Versions 15 to 18 are expected to work ([Postgres requirements](ops/postgres-requirements.md#which-postgres-works)). Production also needs Redis (`REDIS_URL`), only for rate limiting.

## From a usage event to a paid invoice

1. **`usage`** stores each event from `POST /v1/usage-events` and answers `201`.
2. **The billing scheduler** closes a billing period on its first run after the period's end plus `INVOICE_GRACE_PERIOD` (1 hour by default).
3. **`billing`** gets the period's usage totals from `usage` and the prices from `pricing`, adds the plan's flat fees, and builds the invoice through `invoice`.
4. **`invoice`** finalizes it with tax from the tenant's [tax provider](#tax-providers). If tax cannot be worked out yet, the invoice stays a draft until a retry succeeds ([details](ops/tax-calculation.md#when-tax-cannot-be-worked-out)).
5. **`payment`** charges what credit did not cover, with a Stripe PaymentIntent on the saved card.
6. **`dunning`** retries a failed charge on the tenant's dunning policy, and takes the policy's final action if every retry fails.

Each step queues its webhook events and emails for the [background jobs](#background-work). Every invoice status is in [Invoices](concepts/invoices.md#statuses).

## The path of a request

Every request passes the root chain, then its group's chain, then its handler. Inside `/v1`, authentication and the route's permission check come before the group's steps. `internal/api/router.go` builds all of it, and this section lists each step and says why the record (the one object per request that holds its route, client address and status), the metrics, the access log and the door (the clock door, which decides the instant the request runs on) sit where they do. In a chain, *above* means earlier: a step wraps every step below it, so it sees the request before them and their answer after.

**The root chain**, in order:

1. **Trace span** (`mw.Tracing`) starts the request's span.
2. **Request id** (`mw.RequestID`) is minted by the server, never taken from the client. The access log line and the request's audit rows carry it.
3. **Client address** (`mw.TrustedClientIP`) reads `X-Forwarded-For` only from a proxy listed in `TRUST_PROXY`; otherwise it is the TCP peer.
4. **The record** (`request.Begin`), [below](#the-record). It sits below the client address, which it takes once; every later step reads it.
5. **Span correction** (`mw.TraceRequestAttrs`) names the span by the route and keeps a link token (the secret in a public page's link, such as a hosted invoice's) out of `url.path`.
6. **Metrics** come above the Recoverer, so a recovered panic is still counted as its 500, and above CORS, so a preflight is counted.
7. **The access log** sits there for the same reason.
8. **Recoverer** answers a panic with the JSON 500 and logs its stack.
9. **CORS** answers a preflight itself.
10. **Body cap** (`limitRequestBody`): 1 MB, or the LiteLLM adapter's larger cap.
11. **Security headers.**
12. **Audit detector** (`mw.AuditCoverage`) only observes: it reports a mutating 2xx that wrote no audit row and is not declared exempt.

**The groups.** chi then routes the request into one group, which adds its own steps. Each group ends with **the clock door**. It decides the instant the request runs on, from the route's entry in the [route table](#the-route-table). The handler reads that instant from the context.

| Group | Steps, in order |
|---|---|
| `/health`, `/metrics` | the clock door (`/metrics` also checks its token) |
| `/v1/bootstrap` | bootstrap rate limit, 30 s, the clock door |
| `/v1/auth` | CSRF guard, sign-in rate limit, 30 s, the clock door |
| `/v1/webhooks` | 30 s, the clock door; each provider's own check (Stripe's signature, Postmark's credentials) runs in its handler |
| `/v1/public/…` (hosted invoice, payment update, cost dashboard) | public-page rate limit, 30 s, the clock door |
| every other `/v1` route: | CSRF guard, authentication, the route's permission, then one of: |
| — ordinary | general rate limit, Idempotency, 30 s, the clock door |
| — usage ingest | ingest rate limit, 30 s, the clock door |
| — exports | general rate limit, 5 minutes, the clock door |
| — the event stream | general rate limit, the clock door |

The door is last for three reasons:
- after authentication, so it looks up that invoice or customer in the caller's tenant and mode (test or live);
- after Idempotency, so a replayed request never reaches it;
- inside the time limit, so its lookups are bounded.

### The record

`request.Begin` makes one record per request, at the root (`internal/platform/request`). It holds the request's route (found once; `unmatched` when no route in the route table matches), its client address, and the writer that records its status. Authentication adds the caller, and an audit row or `audit.MarkSkip` (called by a request that rightly writes no row, such as a replayed answer) sets its audit mark, which says the request's audit is accounted for.

- **One accessor:** every layer reads the record through `request.Of`, which panics when the root made none, as only a mis-wired server can.
- **What belongs on it:** a fact about the request that more than one layer reads. What a callee needs to do its work (the tenant, the mode, the instant) stays on the context.
- **The audit exception:** a job or the CLI writes audit rows with no request, so the audit package alone uses two calls that accept a missing record (`request.MarkAudited`, `request.ClientIPIfAny`).

### The route table

`internal/api/route_table.go` has one line per route, saying:
- who may call it: `.needs(…)` with a permission, a dashboard session only or any caller, or `.outsideAuth()` for a route outside authentication (the permission check reads this);
- how its time is decided (the clock door reads this);
- where its audit evidence comes from, when it changes state or exports (the audit detector);
- whether its answer carries a secret shown once (Idempotency never keeps that answer).

`TestRouteTable_MatchesLiveRoutes` walks the router and fails on a route with no entry, an entry with no route, or a missing audit. A server that would serve a route with no entry does not start, so a request with no entry is one no route serves.

### Adding a route

1. **Register it** in its handler's `Routes()`, such as `invoice.Handler.Routes`, which `router.go` mounts at `/v1/invoices`. A new handler also needs a mount in `router.go`, in the group whose rate limit, Idempotency and time limit fit it ([the groups](#the-path-of-a-request)): the ordinary group, unless the route sends usage, streams or exports.
2. **Add its entry** to `routeTable`, such as:
   ```go
   {"POST", "/v1/invoices/{id}/finalize"}: ofInvoice().needs(invoiceWrite).withAudit(explicit("invoice.Service.Finalize (LogInTx)")),
   ```
   - how its time is decided: one of the clock constructors in `route_table.go` (`realTime()`, `ofCustomer("id")`, `ofInvoice()`, …, `decidedIn("package.Type.Method")`);
   - who may call it: `.needs(…)` with a permission value (`invoiceRead`, `invoiceWrite`, …), `membersSession`, `settingsSession` or `anyCaller`; or `.outsideAuth()` for a route outside `/v1`'s authentication;
   - where its audit row comes from, if it changes state or exports: `.withAudit(explicit("…"))`, naming the function that writes it, or `.withAudit(exempt(reason, "…"))`, saying why it writes none;
   - `.withShowOnce()`, when its answer carries a secret shown once.
3. **Describe it in `api/openapi.yaml`** ([OpenAPI workflow](dev/openapi-workflow.md)).
4. **Read or write is your choice.** A route that changes data takes its area's write permission. `TestRouteTable_WritesNeedAWritePermission` fails on a `POST`, `PUT`, `PATCH` or `DELETE` that takes a read one, unless the test lists it with its reason.
5. **What fails when a part is forgotten:**
   - no entry: the server does not start, and `TestRouteTable_MatchesLiveRoutes` fails;
   - no `.needs(…)` or `.outsideAuth()`: the entry does not compile;
   - no audit on a route that changes state: `TestRouteTable_MatchesLiveRoutes` fails;
   - an explicit note naming a function that does not exist, or a clock parameter (`ofCustomer("…")`) that is not in the pattern: `TestRouteTable_DeclarationsAreWhole` fails;
   - a handler that never writes the audit row its entry declares: `internal/api`'s `TestMain` fails the end-to-end run, naming the route, when a test drives it;
   - no `api/openapi.yaml` entry: `TestOpenAPISpec_MatchesRouter` fails;
   - a new any-caller or session-only route: `TestRouteTable_AnyCallerStaysSmall` or `TestRouteTable_SessionOnlyRoutesAreListed` fails;
   - a new route an ingest key may call (`usageIngest`): `TestIngestKey_RefusedOnEveryRouteButUsage` fails until the route is in its `ingestKeyRoutes`.

### Where a refusal comes from

The access log and the metrics name each request by its route's pattern when it matched a route. Otherwise they name it `unmatched`, and the log keeps only the path's first segment.

| Answer | From | When |
|---|---|---|
| `404`, `405` | the router; inside `/v1`, the route's permission check | no route matches the path or the method. Inside `/v1` the permission check answers it right after authentication, before any group step. A `405`'s `Allow` lists the methods a route serves at the path |
| `413` | the body cap | a body over its cap, when it is read: by Idempotency first, on a request it guards (a write with an `Idempotency-Key`), else by the handler |
| `403` cross-site request blocked | the CSRF guard | a write from another site, unless it carries a Bearer key and no session cookie |
| `401` | authentication | `missing credentials`: no key, and no session cookie that is still signed in (an expired or signed-out session included); `invalid or expired API key`: a key that is not valid |
| `503 authentication_unavailable` | authentication | the session or key store could not be read |
| `403`, `500` | the route's permission check | `403`: the caller lacks the route's permission, or the route takes only a dashboard session; it comes before the rate limit, Idempotency and the door. `500`: the route's entry names no caller, a fault in the table |
| `429` | the group's rate limit | over the bucket, or any request while a fail-closed limiter cannot reach Redis |
| `409 conflict_idempotency` | Idempotency | the first request with the key is still running after the wait |
| `409 conflict_idempotency_unresolved` | Idempotency | the first request's owner died before it stored an answer, so the outcome is unknown |
| `422 idempotency_error` | Idempotency | the key reused for another request, or by another caller |
| `503 idempotency_unavailable` | Idempotency | the key could not be recorded; its other answers are a `400` (a body it cannot read), the `413` above, and a `500` when no caller is on the request (a mis-wired router) |
| `504 timeout` | the time limit | the handler wrote nothing in time |
| `500` | the clock door | a route entry the door does not know |
| `500 internal_error` | the Recoverer | a panic before anything was written |

### The answer

- **Writing:** a handler answers through `respond`, which writes every body and sets `Hardcount-Version`. `Hardcount-Request-Id` is set at the root.
- **Errors:** an error becomes an answer by its kind, in one place, `respond.FromError`; its doc holds the table. An error with no kind answers `500` and is logged.
- **Audit:** each explicit audit note in the route table marks how its route writes its row, `(LogInTx)` or `(Log)`; an exempt route writes none.
  - **`(LogInTx)`, the rule ([ADR-090](adr/090-audit-in-tx-emission.md)):** the row is in the same transaction as the change, so the two commit together, and a failed write rolls the change back.
  - **`(Log)`:** the row is written just after the change commits, so a failed write leaves the change without its row; it is counted in `hardcount_audit_write_errors_total` and logged.
  - The audit detector at the root reports a change that wrote no row.

## Design rules

**One package per domain.** A business domain is one part of billing, such as invoices or payments. Its package under `internal/` usually holds a handler (`handler.go`), a service (`service.go`) and a store (`store.go`, `postgres.go`). The handler decodes the request, calls the service and writes the response. Transactions, money math and state changes live in the service, and SQL lives in the store.

**A domain does not call another domain's service or store.** It declares a small interface for what it needs, and `internal/api` passes in the real one at start-up. `billing` is the coordinator: it calls the other domains through interfaces it defines.

[`boundaries_test.go`](../internal/arch/boundaries_test.go) fails on an import between two packages under `internal/` that it does not list. The test file is the list. Every package counts, including `auth` and `audit`, except the directories listed in `nonDomainDirs` (shared code such as `domain` and `errs`). A new entry needs a comment that says why. Do not copy the older entries that have none.

**Postgres keeps tenants apart.** Most work opens its transaction with `postgres.TxTenant`, and row-level security (RLS) then shows only that tenant's rows ([`rls_isolation_test.go`](../internal/platform/postgres/rls_isolation_test.go) proves it).

**Work that skips RLS is listed.** `postgres.TxBypass` turns RLS off, so the query must filter by tenant itself. No test checks that filter. A reviewer must check it. [`txbypass_allowlist_test.go`](../internal/arch/txbypass_allowlist_test.go) fails on a function that uses `TxBypass` without being on its list. A new kind of work goes in its `bypassKinds` table and in a bullet below.

- background jobs, and the metrics that count their queues;
- sign-in, sessions, API key lookup and creating an account;
- public links;
- incoming webhooks;
- reading an account's Stripe keys to call Stripe, and recording whether Stripe accepted them (`internal/tenantstripe`);
- one signed-in area: team members and invites (`internal/dashmembers`);
- the restore commands (`hardcount restore begin` and `catch-up`);
- `hardcount-doctor`, which reads every account's rows in a read-only transaction;
- `hardcount-bench-seed`.

**Every tenant-scoped request and job states its mode.** Test mode and live mode are separate data in one tenant. Each entry point sets the mode with `postgres.WithLivemode`, and nothing guesses a missing one:

- `BeginTx` refuses a `TxTenant` without a mode (`ErrLivemodeUnset`) or a tenant ID (`ErrTenantUnset`);
- the database refuses an insert when `app.livemode` is not `on` or `off`, and RLS then shows no row.

**A job that must run once takes a leader lease.** Every copy of Hardcount runs the same loops. A leader lease is a row in `leader_leases` that lets one copy run one tick (one pass of the job's loop). It expires 10 seconds after its last renewal, so another copy takes over when a leader stops. Each job takes the rows it will work on with a claim query. That query checks the lease again, so a copy that lost the lease takes none ([`leader_fence_test.go`](../internal/arch/leader_fence_test.go), [ADR-114](adr/114-leader-leases-tick-scoped-fencing.md)).

**Stripe charges the card. Hardcount owns the invoice.** Hardcount uses Stripe PaymentIntents, and Checkout Sessions for hosted pages that take a payment or save a card, never Stripe Billing or Stripe Invoices.

**Two ways to sign in.** Dashboard users sign in with an email and password and get a session cookie. API callers send a Bearer key. When a request carries both, a cookie for a live session wins, and a cookie for an ended session falls through to the key ([ADR-011](adr/011-email-password-auth-and-clean-api-keys.md)). A session has a secret key's permissions. Every dashboard member holds that full set: a role is recorded but not enforced, and removing a member ends their sessions in that workspace ([ADR-081](adr/081-minimal-team-invites.md)).

**The audit log only grows.** Triggers refuse any update, delete or truncate, and the limited database user (`hardcount_app`) has no right to make one. A database owner could still remove the triggers.

### To add a domain

1. Create `internal/<name>/`, laid out like `internal/customer`.
2. Add a migration, following [Writing a database migration](dev/migrations.md).
3. Wire the domain into `internal/api/wire.go`, and add its routes in `internal/api/router.go`.
4. List its imports from `internal/` in `boundaries_test.go`.

## Package layout

Under `cmd/`:

- `hardcount`: the server (API and background jobs), plus `hardcount migrate` and `hardcount restore`.
- `hardcount-bootstrap`: runs the migrations, then creates an account with its API keys and owner login.
- `hardcount-doctor`: a read-only check of the money rules.
- `hardcount-bench-seed`: sets up a benchmark database.
- `hardcount-docs-run`: runs a docs page's commands, so CI tests the tutorial.

Under `internal/`:

- **Accounts and sign-in:** `tenant` (accounts and settings), `user`, `session`, `dashmembers`, `auth` (API keys of two types, `secret` and `ingest`, and per-route permissions).
- **What is sold:** `customer`, `pricing` (meters, plans, prices), `recipe` (ready-made pricing setups), `subscription`.
- **Usage:** `usage`, `integrations/litellm` (turns LiteLLM logs into usage events).
- **Billing:** `billing` (engine, scheduler, invoice preview), `invoice`, `tax`, `credit` (one ledger of grants, usage, expiry and adjustments; append-only, except a grant's `consumed_cents` and `cn_retired_cents`), `creditnote` (credit notes and refunds).
- **Getting paid:** `payment` (Stripe charges and Stripe's webhooks), `paymentmethods` (saved cards), `tenantstripe`, `dunning`, `hostedinvoice` (the public invoice page).
- **Telling people:** `webhook` (outbound, HMAC-signed), `email`, `audit`, `analytics` (dashboard numbers).
- **Checking:** `doctor`, `arch` (tests for the rules on this page), `testutil`.
- **Shared:** `api` (builds every store, service and handler, and mounts the routes), `domain` (shared data types, in the package `internal/domain`; it imports no store, service or other business package), `errs`, `config`, `pdffonts`, `version`.
- **`platform/`:** `postgres` (transactions and RLS), `migrate`, `leader`, `scheduler` (the loop for leased jobs), `clock` (the current time, real or fake), `money` (integer-cents math), `crypto`, `telemetry`, `logging`, `request` (one request's record: [The record](#the-record)), `listorder`, `activity`, `timeline`.

## Background work

Five jobs take a [leader lease](#design-rules):

- **`billing`**, every hour (5 minutes when `APP_ENV=local`). Runs for live mode, then test mode. Retries stuck charges and tax. Charges invoices waiting for payment. Checks billing thresholds (spend or usage) and ends trials. Closes billing periods into invoices. Resumes paused collection and expires credits.
- **`dunning`**, at the same interval. Runs the payment retries that are due, queues their emails and takes final actions.
- **`webhook_outbox`**, every 2 seconds. Sends queued webhook events.
- **`email_outbox`**, every 5 seconds. Sends queued emails.
- **`webhook_delivery`**, every 30 seconds. Retries failed deliveries.

An **outbox** is a table of messages waiting to be sent (`webhook_outbox`, `email_outbox`). Write to one in the same transaction as the change that needs the message: `OutboxStore.Enqueue(ctx, tx, ...)`.

## Tax providers

Tax comes from one of three providers in `internal/tax`: `none`, `manual` (one flat rate) and `stripe_tax`. Each tenant picks one in `tenant_settings.tax_provider`, and an unknown value is an error, not zero tax ([ADR-041](adr/041-tax-fallback-manual-removed.md)). Setup and failure handling are in [Tax calculation](ops/tax-calculation.md).
