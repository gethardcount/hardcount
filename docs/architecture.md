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

## Design rules

**One package per domain.** A business domain is one part of billing, such as invoices or payments. Its package under `internal/` usually holds a handler (`handler.go`), a service (`service.go`) and a store (`store.go`, `postgres.go`). The handler decodes the request, calls the service and writes the response. Transactions, money math and state changes live in the service, and SQL lives in the store.

**A domain does not call another domain's service or store.** It declares a small interface for what it needs, and `internal/api` passes in the real one at start-up. `billing` is the coordinator: it calls the other domains through interfaces it defines.

[`boundaries_test.go`](../internal/arch/boundaries_test.go) fails on an import between two packages under `internal/` that it does not list. The test file is the list. Every package counts, including `auth` and `audit`, except the directories listed in `nonDomainDirs` (shared code such as `domain` and `errs`). A new entry needs a comment that says why. Do not copy the older entries that have none.

**Postgres keeps tenants apart.** Most work opens its transaction with `postgres.TxTenant`, and row-level security (RLS) then shows only that tenant's rows ([`rls_isolation_test.go`](../internal/platform/postgres/rls_isolation_test.go) proves it).

**Work that skips RLS is listed.** `postgres.TxBypass` turns RLS off, so the query must filter by tenant itself. No test checks that filter. A reviewer must check it. [`txbypass_allowlist_test.go`](../internal/arch/txbypass_allowlist_test.go) fails on a function that uses `TxBypass` without being on its list. It also fails if a bullet below loses the exact words its `bypassKinds` table holds. So add a new kind of work to both, and reword a bullet only with the table.

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

**Two ways to sign in.** Dashboard users sign in with an email and password and get a session cookie. API callers send a Bearer key, and the cookie wins when a request carries both ([ADR-011](adr/011-email-password-auth-and-clean-api-keys.md)). A session has a secret key's permissions. Every dashboard member holds that full set: a role is recorded but not enforced, and removing a member ends their sessions ([ADR-081](adr/081-minimal-team-invites.md)).

**The audit log only grows.** Triggers refuse any update, delete or truncate, and the limited database user (`hardcount_app`) has no right to make one. A database owner could still remove the triggers.

### To add a domain

1. Create `internal/<name>/`, laid out like `internal/customer`.
2. Add a migration, following [Writing a database migration](dev/migrations.md).
3. Wire the domain into `internal/api/router.go`.
4. List its imports from `internal/` in `boundaries_test.go`.

## Package layout

Under `cmd/`:

- `hardcount`: the server (API and background jobs), plus `hardcount migrate` and `hardcount restore`.
- `hardcount-bootstrap`: runs the migrations, then creates an account with its API keys and owner login.
- `hardcount-doctor`: a read-only check of the money rules.
- `hardcount-bench-seed`: sets up a benchmark database.
- `hardcount-docs-run`: runs a docs page's commands, so CI tests the tutorial.

Under `internal/`:

- **Accounts and sign-in:** `tenant` (accounts and settings), `user`, `session`, `dashmembers`, `auth` (API keys of two stored types, `secret` and `ingest`, and per-route permissions).
- **What is sold:** `customer`, `pricing` (meters, plans, prices), `recipe` (ready-made pricing setups), `subscription`.
- **Usage:** `usage`, `integrations/litellm` (turns LiteLLM logs into usage events).
- **Billing:** `billing` (engine, scheduler, invoice preview), `invoice`, `tax`, `credit` (one ledger of grants, usage, expiry and adjustments; append-only, except a grant's `consumed_cents` and `cn_retired_cents`), `creditnote` (credit notes and refunds).
- **Getting paid:** `payment` (Stripe charges and Stripe's webhooks), `paymentmethods` (saved cards), `tenantstripe`, `dunning`, `hostedinvoice` (the public invoice page).
- **Telling people:** `webhook` (outbound, HMAC-signed), `email`, `audit`, `analytics` (dashboard numbers).
- **Checking:** `testclock`, `doctor`, `arch` (tests for the rules on this page), `testutil`.
- **Shared:** `api` (builds every store, service and handler, and mounts the routes), `domain` (shared data types, in the package `internal/domain`; it imports no store, service or other business package), `errs`, `config`, `pdffonts`, `version`.
- **`platform/`:** `postgres` (transactions and RLS), `migrate`, `leader`, `scheduler` (the loop for leased jobs), `clock` (real or test-clock time), `money` (integer-cents math), `crypto`, `telemetry`, `logging`, `listorder`, `activity`, `timeline`.

## Background work

Six jobs take a [leader lease](#design-rules):

- **`billing`**, every hour (5 minutes when `APP_ENV=local`). Runs for live mode, then test mode. Retries stuck charges and tax. Charges invoices waiting for payment. Checks billing thresholds (spend or usage) and ends trials. Closes billing periods into invoices. Resumes paused collection and expires credits.
- **`dunning`**, at the same interval. Runs the payment retries that are due, queues their emails and takes final actions.
- **`webhook_outbox`**, every 2 seconds. Sends queued webhook events.
- **`email_outbox`**, every 5 seconds. Sends queued emails.
- **`webhook_delivery`**, every 30 seconds. Retries failed deliveries.
- **`testclock_catchup`**, every 2 seconds. Bills the skipped time for each [test clock](glossary.md#testing-with-simulated-time) that is being moved forward, one clock after another.

An **outbox** is a table of messages waiting to be sent (`webhook_outbox`, `email_outbox`). Write to one in the same transaction as the change that needs the message: `OutboxStore.Enqueue(ctx, tx, ...)`.

A request to move a test clock forward only marks the clock. If the copy running the catch-up stops, the next leader runs it again.

## Tax providers

Tax comes from one of three providers in `internal/tax`: `none`, `manual` (one flat rate) and `stripe_tax`. Each tenant picks one in `tenant_settings.tax_provider`, and an unknown value is an error, not zero tax ([ADR-041](adr/041-tax-fallback-manual-removed.md)). Setup and failure handling are in [Tax calculation](ops/tax-calculation.md).
