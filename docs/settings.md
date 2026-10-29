# Settings reference

This page lists every environment variable Hardcount and its commands read, with what each one does and what happens when it is unset.
The code is the source: [`internal/config/config.go`](../internal/config/config.go), [`internal/api/router.go`](../internal/api/router.go), [`internal/email/sender.go`](../internal/email/sender.go), [`cmd/hardcount/main.go`](../cmd/hardcount/main.go) and the other `cmd/` programs. A test, `internal/arch/settings_doc_test.go`, fails when the code reads a setting this page does not list.

**How to read the Default column.** "Unset:" is followed by what Hardcount does without the setting. "Stops start-up" means the program logs the reason and exits. Values are trimmed, so spaces around a value do not matter. `APP_ENV` decides which checks run: "production" below means `APP_ENV=production`, and "outside local" means any value except `local`.

**Which program reads what.** Every setting below is read by the server, `hardcount` (also `hardcount serve`), unless its row names another program.

| Program | Reads |
|---|---|
| `hardcount migrate`, `migrate status`, `migrate rollback` | `DATABASE_URL` or the `DB_HOST` set, the pool settings and `MIGRATION_LOCK_TIMEOUT`, all in [Database](#database). `migrate rollback` also reads `APP_ENV` and runs only when it is `local`. |
| `hardcount-bootstrap` (`make bootstrap`) | Every setting the server checks at start-up (`config.Load`), so the production checks apply to it too. It always applies migrations first. It also reads the three [first-account](#first-account) settings. |
| `hardcount-doctor` | `DATABASE_URL` only. Required: without it the doctor exits with status 2. It does not read the `DB_HOST` set. |
| `hardcount-bench-seed` | Every setting the server checks at start-up (`config.Load`), plus the two `BENCH_*` settings in [Development and tests](#development-and-tests-only). |
| `hardcount-docs-run` | No environment variables; only its command-line flags. |

Stripe has no environment variable. Each account connects its own Stripe keys in the dashboard, under Settings → Payments, and Hardcount stores them in the database.

## Database

Hardcount uses two database users. The admin user runs migrations; the limited user (`hardcount_app`) runs every request, so row-level security (RLS) keeps accounts apart. Setup: [Postgres requirements](ops/postgres-requirements.md).

| Setting | What it does | Default |
|---|---|---|
| `DATABASE_URL` | Postgres URL of the admin user. Migrations and start-up checks use it. | Required, unless the `DB_HOST` set below is given. Without either, start-up stops. |
| `DB_HOST`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` | The admin connection given in parts instead of `DATABASE_URL`. When any of the four is set, all four are required and `DATABASE_URL` is ignored. | Unset. Setting only some of them stops start-up. |
| `DB_PORT` | Port for the `DB_HOST` set. | `5432` |
| `DB_SSLMODE` | Postgres `sslmode` for the `DB_HOST` set. | `require` |
| `APP_DATABASE_URL` | Postgres URL of the limited user that requests run as. Used exactly as written. If it is set and cannot connect, start-up stops. | Local: built from `DATABASE_URL` with user and password `hardcount_app`; if that fails, the server runs on the admin connection with a warning and RLS is not enforced. Outside local: required. Start-up also stops if its password is empty, is `hardcount_app` or equals the user name, or if the user can bypass RLS. |
| `DB_MAX_OPEN_CONNS` | Most open connections in each pool. The server opens two pools, admin and limited, so one copy can hold twice this number. | `20`. Below 1 logs a warning. |
| `DB_MAX_IDLE_CONNS` | Most idle connections kept in each pool. | `5`. Above `DB_MAX_OPEN_CONNS` logs a warning. |
| `DB_CONN_MAX_LIFETIME_MIN` | Minutes before a connection is closed and replaced. | `30` |
| `DB_CONN_MAX_IDLE_TIME_SEC` | Seconds an idle connection is kept. | `120` |
| `DB_PING_TIMEOUT_SEC` | Seconds the start-up connection check waits for Postgres. | `5` |
| `DB_QUERY_TIMEOUT_MS` | Milliseconds a request-path query may run. | `5000`. Below 100 logs a warning. |
| `RUN_MIGRATIONS_ON_BOOT` | Apply new migrations when the server starts. Accepts `true`, `false`, `yes`, `no`, `1`, `0`. | `false`: then run `hardcount migrate` first, because the server refuses to start while a migration is missing. Any other value stops start-up. |
| `MIGRATION_LOCK_TIMEOUT` | How long one migration waits for a table lock before it stops. A Go duration such as `30s`, or `0` for no limit. Read by `hardcount migrate`, `hardcount-bootstrap`, and the server when `RUN_MIGRATIONS_ON_BOOT` is on. | `5s`. Any other value stops the migration. |

Every whole-number setting above stops start-up when its value is not a whole number, such as `5s`.

## Server and network

| Setting | What it does | Default |
|---|---|---|
| `APP_ENV` | `local`, `staging` or `production`. Outside local: the sign-in cookie is `Secure` (sign in over HTTPS), responses carry HSTS, `APP_DATABASE_URL` is required, the bootstrap token needs 16 characters, and `SHUTDOWN_DRAIN_DELAY` defaults to `5s`. Production adds the checks marked "production" on this page. In local, background billing runs every 5 minutes instead of every hour. | `local`. A value that starts with `prod` but is not `production`, such as `prod`, stops start-up. Any other unknown value logs a warning and runs as not local and not production. |
| `PORT` | The HTTP port the server listens on. | `8080`. A value that is not a port number logs a warning. |
| `TRUST_PROXY` | Addresses of your proxy or load balancer: IPs or CIDRs, comma-separated, or `none` for no proxy. Hardcount then takes the client address from their `X-Forwarded-For`. It does not read `X-Real-IP`. Why it matters: [Self-host](self-host.md#settings). | Unset: the direct peer is the client, with a warning. Production: required. An entry that is not an IP or CIDR stops start-up in every environment. |
| `CORS_ALLOWED_ORIGINS` | Browser origins allowed to call the API, comma-separated and each in full, such as `https://app.example.com`. A listed origin may send the sign-in cookie. `*` alone allows any site, without the cookie. | `http://localhost:3000,http://localhost:5173,http://localhost:5174`, in every environment. An entry with a `*` inside it, or `*` beside other origins, stops start-up. |
| `HARDCOUNT_COOKIE_DOMAIN` | The `Domain` of the sign-in cookie, such as `.example.com` when the API and dashboard are on sibling subdomains. | Unset: the cookie is for the API's host only. |
| `HARDCOUNT_API_BASE_URL` | The public address of the API, so a copied cost-dashboard link is a full address. | Unset: the link is a path, and start-up logs a warning. |
| `REDIS_URL` | Redis, used for rate limiting only. | Production: required, and an unreachable Redis stops start-up. Elsewhere: unset or unreachable logs a warning; rate limits are then off, except sign-in's, which each copy counts on its own. |
| `HARDCOUNT_RATE_LIMIT_GENERAL_PER_MIN` | Requests a minute for each API key, for each account's dashboard users together, or for each client address before sign-in. Sign-in attempts from one address use the same limit. | `100`. A value that is not a whole number of 1 or more stops start-up. |
| `HARDCOUNT_RATE_LIMIT_INGEST_PER_SEC` | Usage events and LiteLLM callbacks a second, for each API key. | `1000`. A value that is not a whole number of 1 or more stops start-up. |
| `SHUTDOWN_DRAIN_DELAY` | On shutdown, how long a copy keeps serving after `/health/ready` turns to 503, so a load balancer stops sending it traffic first. A Go duration. See [High availability](ops/high-availability.md). | Outside local: `5s`. Local: `0`. A value that does not parse, or is negative, stops start-up. |

Public addresses (`DASHBOARD_BASE_URL`, `CUSTOMER_PORTAL_URL`, `HOSTED_INVOICE_BASE_URL`, `PAYMENT_UPDATE_URL`, `PAYMENT_UPDATE_RETURN_URL`, `HARDCOUNT_API_BASE_URL`): in staging and production, one that starts with `http://` logs a warning, because the sign-in cookie there needs HTTPS.

## Security and keys

| Setting | What it does | Default |
|---|---|---|
| `HARDCOUNT_ENCRYPTION_KEY` | Encrypts stored personal data, webhook signing secrets, Stripe keys and hosted-invoice tokens (AES-256-GCM). It also keys the sign-in throttle. 64 hex characters: `openssl rand -hex 32`. Keep a copy: [Back up Hardcount](ops/backup-considerations.md). | Unset: that data is stored in plain text, with a warning. Production: required. A set value that is not 64 hex characters stops start-up in every environment. |
| `HARDCOUNT_BOOTSTRAP_TOKEN` | Allows the `POST /v1/bootstrap` call that creates the first account. | Unset: that call is turned off. Outside local, a value shorter than 16 characters stops start-up. |
| `HARDCOUNT_EMAIL_BIDX_KEY` | A separate key that lets Hardcount find a customer by email address, for bounce reports and for not sending to an address that bounced. 64 hex characters, not the encryption key: `openssl rand -hex 32`. | Unset: bounces are not recorded against customers, and bounced addresses are still sent to. Start-up logs a warning. A value that is not 64 hex characters logs an error and works as unset. |

## Email

Without `SMTP_HOST`, Hardcount sends no email: every send fails with "SMTP not configured" and the dashboard says so. Setup for each provider: [Email setup](ops/email-setup.md).

| Setting | What it does | Default |
|---|---|---|
| `SMTP_HOST` | Your email server's host name. | Unset: no email is sent, with a warning at start-up. |
| `SMTP_PORT` | Your email server's port. | `587` |
| `SMTP_TLS` | `starttls` (port 587), `implicit` (port 465) or `none`. | `starttls`. An unknown value logs a warning and uses `starttls`. In production, `none` makes every send fail. |
| `SMTP_USERNAME`, `SMTP_PASSWORD` | Login for the email server. | Unset: Hardcount sends without logging in. |
| `SMTP_FROM` | The sender address on every email. | `billing@hardcount.local` |
| `HOSTED_INVOICE_BASE_URL` | Address of the hosted invoice page: the "View & pay invoice" link in emails, and where Stripe Checkout returns a customer who paid there. | Unset: emails have no invoice link, with a warning. Checkout returns to `http://localhost:5173`. |
| `PAYMENT_UPDATE_URL` | The "add a payment method" link in emails. Hardcount adds `?token=…`. | Unset: those emails are not sent, with a warning. |
| `DASHBOARD_BASE_URL` | The dashboard's address, for password-reset and team-invite links. | Unset: password-reset emails are not sent and invites are refused, with a warning. |
| `POSTMARK_WEBHOOK_USER`, `POSTMARK_WEBHOOK_PASS` | User name and password Postmark must send with bounce reports to `POST /v1/webhooks/postmark`. Postmark only. | Unset (either one): that route answers 401 to every post, with a warning. |

## Stripe and payments

| Setting | What it does | Default |
|---|---|---|
| `CUSTOMER_PORTAL_URL` | The dashboard's address, with no path. Stripe returns a customer here after they save a card. | Unset: Stripe returns them to `http://localhost:5173/payment-method-added`, with a warning. The card is still saved. |
| `PAYMENT_UPDATE_RETURN_URL` | Where a customer lands after adding a card from the "add a payment method" email. Hardcount adds `?status=success` or `?status=cancel`. | Unset: `http://localhost:5173/payment-method-added`. |

## Background jobs

| Setting | What it does | Default |
|---|---|---|
| `INVOICE_GRACE_PERIOD` | How long after a billing period ends Hardcount waits before it builds the invoice, so late usage stamped inside the period is billed. A Go duration from `0` to `72h`. See [Invoice timing and late usage](ops/runbook/invoice-timing-late-usage.md). | `1h`. Any other value stops start-up. |

How often background billing runs is not a setting: every hour, or every 5 minutes with `APP_ENV=local`.

## Observability

| Setting | What it does | Default |
|---|---|---|
| `LOG_LEVEL` | `debug`, `info`, `warn` (or `warning`) or `error`. Logs are JSON on standard output. | `info`. An unknown value also means `info`, without a warning. |
| `METRICS_TOKEN` | The bearer token a scraper must send to read `/metrics`. | Unset: `/metrics` is open, except in production, where it answers 401 and start-up logs a warning. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Your OpenTelemetry collector's OTLP/HTTP address, such as `http://otel-collector:4318`. Setting it turns tracing on. | Unset: no traces are sent. |
| `OTEL_SERVICE_NAME` | The service name on every trace. | `hardcount` |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Which traces to keep, with the standard OpenTelemetry values. To keep 1 in 10: `parentbased_traceidratio` and `0.1`. | `parentbased_always_on`: every trace. |

The OpenTelemetry library reads these itself, and also its other standard `OTEL_*` settings, such as `OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_RESOURCE_ATTRIBUTES`.

## First account

`hardcount-bootstrap` (`make bootstrap`) reads these once, when it creates an account. The server does not read them.

| Setting | What it does | Default |
|---|---|---|
| `HARDCOUNT_BOOTSTRAP_EMAIL` | The dashboard owner's email. | `admin@hardcount.local` |
| `HARDCOUNT_BOOTSTRAP_PASSWORD` | The owner's password. | Unset: one is generated and printed once. |
| `HARDCOUNT_BOOTSTRAP_TENANT` | The account's name. | Unset: the command-line arguments, or `Demo Tenant` without any. |

## Compose only

These are read by Docker Compose from [`deploy/compose/.env`](../deploy/compose/.env.example), by [`docker-compose.yml`](../deploy/compose/docker-compose.yml), [`postgres-init.sh`](../deploy/compose/postgres-init.sh) or [`quickstart.sh`](../deploy/compose/quickstart.sh). Hardcount itself does not read them.

| Setting | What it does | Default |
|---|---|---|
| `POSTGRES_PASSWORD` | Password of the database admin user. Compose builds `DATABASE_URL` from it. | Required: Compose will not start without it. |
| `HARDCOUNT_APP_DB_PASSWORD` | Password of the limited `hardcount_app` user. `postgres-init.sh` creates the user with it on the first start, and Compose builds `APP_DATABASE_URL` from it. Keep it URL-safe: `openssl rand -hex 24`. | Required: Compose will not start without it. |
| `POSTGRES_DB`, `POSTGRES_USER` | Database name and admin user name. | `hardcount` and `hardcount` |
| `HARDCOUNT_IMAGE` | The API image to run. | `hardcount-api:local`, built from the checkout |
| `HARDCOUNT_DASHBOARD_IMAGE` | The dashboard image to run. | `hardcount-dashboard:local`, built from the checkout |
| `NGINX_HTTP_PORT` | The host port nginx listens on, for the dashboard and API. | `80` |
| `HARDCOUNT_PORT` | Read by `quickstart.sh`: the port it writes into `.env` as `NGINX_HTTP_PORT`. | `8080` |
| `COMPOSE_PROJECT_NAME` | Docker Compose's own project name. `quickstart.sh` writes `hardcount-quickstart`. | The directory name |

How Compose differs from running the binary:

- **Compose passes only the names `docker-compose.yml` lists.** `HARDCOUNT_RATE_LIMIT_GENERAL_PER_MIN`, `HARDCOUNT_RATE_LIMIT_INGEST_PER_SEC`, `SHUTDOWN_DRAIN_DELAY`, `DB_SSLMODE` and the `DB_HOST` set are not listed, so setting them in `.env` does nothing. Add them to the `hardcount-api` service's `environment` to use them.
- **Some values are fixed.** `PORT` is `8080`, `RUN_MIGRATIONS_ON_BOOT` is `true`, and `DATABASE_URL` and `APP_DATABASE_URL` point at the in-stack Postgres. A `DATABASE_URL` in `.env` is ignored.
- **Some defaults differ.** `APP_ENV` is `production`, `REDIS_URL` is `redis://redis:6379`, `TRUST_PROXY` is `172.16.0.0/12,192.168.0.0/16`, and `SMTP_FROM` is `billing@example.com`. `quickstart.sh` sets `APP_ENV=local`.

## Development and tests only

| Setting | What it does | Default |
|---|---|---|
| `TEST_ADMIN_DATABASE_URL` | Admin URL of the integration-test database (`go test -short=false`). | `postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable` |
| `TEST_DATABASE_URL` | Limited-user URL of the integration-test database. | `postgres://hardcount_test_app:hardcount_test_app@localhost:5432/hardcount_test?sslmode=disable` |
| `TEST_RUNTIME_ROLE_DATABASE_URL` | The `hardcount_app` URL the audit-table grants test connects with. | `TEST_DATABASE_URL` with user and password `hardcount_app` |
| `HARDCOUNT_LIVE_BALANCE_TIMING_EVENTS` | Event counts for a timing measurement in `internal/billing`, such as `100000,1000000`. | Unset: that test is skipped. |
| `HARDCOUNT_TEST_CLOCK_INJECT_FAILURE` | Read by the server: makes the next test-clock advance fail with this text, once, for manual tests. Never set it in production. | Unset |
| `BENCH_LIVEMODE` | Read by `hardcount-bench-seed`: seed live mode (`true`) or test mode (`false`). | `true`. Any other value stops it. |
| `BENCH_CUSTOMERS` | Read by `hardcount-bench-seed`: how many customers to create. | `200`. A value that is not a whole number of 1 or more stops it. |
| `VITE_API_URL`, `VITE_HOSTED_INVOICE_BASE_URL` | Read when the dashboard (`web-v2`) is built, not by the server: the API address and the hosted invoice address. | Unset: the dashboard's own address. |

Some tests start a copy of themselves and pass it variables such as `HARDCOUNT_CLI_HELPER`. Those are internal to the test and are not settings. The load-test stub in `scripts/bench-rig/` reads its own variables, listed in that folder.
