# Self-host Velox

Velox is one Go program that talks to Postgres. In production it also needs Redis.

## Two ways to run it

| | Good for | What you run |
|---|---|---|
| **One VM with Docker Compose** | Trying Velox, and production for a small team where a few minutes of downtime during a restart is fine | Five containers on one machine |
| **Two or more copies behind a load balancer** | High availability: production where a restart or a crashed copy must not cause downtime | Two or more `velox-api` copies, a managed Postgres, a managed Redis |

Start with one VM; move to two or more copies when downtime during a restart is no longer acceptable.

- **One VM:** follow [Production on one VM](production-one-vm.md). To try Velox on your own computer first, use the [quick start](quick-start.md).
- **Two or more copies:** `velox-api` behind a load balancer, with a managed Postgres and Redis. Setup, health checks, shutdown and upgrades: [High availability](ops/high-availability.md).

A Helm chart and a Terraform module are not written yet.

Running Velox from source for development: [CONTRIBUTING](../CONTRIBUTING.md#development-setup).

## Postgres

The Compose stack ships Postgres 16, set up as Velox needs it. Bringing your own Postgres? Follow [Postgres requirements](ops/postgres-requirements.md): Postgres 16, three extensions, two database users and one required timeout.

Backups and the encryption key: [Back up Velox](ops/backup-considerations.md).

## Database upgrades (migrations)

A migration is a versioned script that changes the database schema. Velox applies them with [goose](https://github.com/pressly/goose); they are in [`internal/platform/migrate/sql/`](../internal/platform/migrate/sql/).

- **See the plan first.** `velox migrate status` shows the database's version, the migrations this release would apply, and whether a migration is running now. It changes nothing.
- Set `RUN_MIGRATIONS_ON_BOOT=true` and Velox applies any new ones when it starts. The Compose stack sets it. The default is `false`: then run `velox migrate` before you start a new release, because Velox refuses to start while a migration is missing.
- If several copies start at once, they take turns: one applies the migrations and the others wait, then find nothing left to do.
- **A failed migration leaves nothing to repair.** Fix the cause the error names and run the upgrade again. See [A database upgrade stopped](ops/runbook/migration-failed-midway.md).
- **Never roll back in production.** `velox migrate rollback` runs only with `APP_ENV=local`. To go back after an upgrade, restore the backup you took before it; see [Going back](production-one-vm.md#going-back).
- **Each migration waits at most 5 seconds for a table lock**, then stops and leaves nothing behind. `MIGRATION_LOCK_TIMEOUT` changes the limit. See [It waited 5s for a table lock](ops/runbook/migration-failed-midway.md#it-waited-5s-for-a-table-lock).

## Upgrading

Upgrading from Velox 0.8.x is not supported; install fresh.

Read the changelog notes and take a backup first. Then follow [Upgrade on one VM](production-one-vm.md#upgrade-to-a-new-release) or [Upgrade two or more copies](ops/high-availability.md#upgrades).

## Settings

Velox reads its settings from environment variables. The tables below list the main ones. More settings, each with a comment, are in the repository's [`.env.example`](../.env.example). The Compose stack reads its own [`deploy/compose/.env.example`](../deploy/compose/.env.example).

**Required**

| Variable | What it is |
|---|---|
| `DATABASE_URL` | Postgres connection URL for the admin user |
| `APP_DATABASE_URL` | Postgres connection URL for the limited user. Required in `staging` and `production`. |
| `VELOX_ENCRYPTION_KEY` | Encrypts stored personal data. 64 hex characters. Required in `production`. |
| `REDIS_URL` | Redis, for rate limiting. Required in `production`. If Redis goes down later, sign-in stays rate-limited, but each copy counts on its own. |
| `TRUST_PROXY` | The addresses of your proxy or load balancer, comma-separated. Velox then trusts the client address they forward in `X-Forwarded-For`; it does not read `X-Real-IP`. Use `none` if there is no proxy. Required in `production`. Velox refuses to start if an entry is not an IP address or CIDR. |

Why `TRUST_PROXY` is required: without it, every request behind a load balancer appears to come from the load balancer. All users then share one rate limit, the audit log shows the load balancer's address, and 5 wrong passwords for an email, from anyone, make every new browser wait to sign in with it.

To check it, sign in, switch the dashboard to test mode and open the audit log. The newest `login` entry shows the address Velox saw. It should be yours, not the load balancer's.

**Read once, when creating the first tenant** (by `make bootstrap`, not by the server)

| Variable | What it is |
|---|---|
| `VELOX_BOOTSTRAP_EMAIL` | The dashboard owner's email: one address with a dot in the domain, such as `you@example.com`. Default `admin@velox.local`. |
| `VELOX_BOOTSTRAP_PASSWORD` | The owner's password. If unset, one is generated and printed once. |
| `VELOX_BOOTSTRAP_TENANT` | The tenant's name. Default `Demo Tenant`. |

**Optional**

| Variable | Default | What it does |
|---|---|---|
| `APP_ENV` | `local` | `local`, `staging` or `production`. `staging` and `production` mark the sign-in cookie `Secure`, so sign-in needs HTTPS (or `http://localhost` in Chrome or Firefox), and turn on the start-up checks on this page. Another value that starts with `prod`, such as `prod`, stops start-up; any other value starts with a warning and counts as not production. |
| `RUN_MIGRATIONS_ON_BOOT` | `false` | Apply database upgrades at start-up. With `false`, run `velox migrate` before starting a new release. A value other than `true` or `false` (or `yes`, `no`, `1`, `0`) stops start-up. |
| `MIGRATION_LOCK_TIMEOUT` | `5s` | How long a database upgrade waits for a table lock before it stops. A duration such as `30s`, or `0` for no limit. Any other value stops the upgrade. |
| `DASHBOARD_BASE_URL` | unset | The dashboard's address, for password-reset and invite links. What breaks without it: [Email setup](ops/email-setup.md#what-breaks-when-a-setting-is-missing). For local development use `http://localhost:5173`. |
| `CUSTOMER_PORTAL_URL` | unset | The dashboard's address, with no path. Stripe returns a customer here after they save a card ([Email setup](ops/email-setup.md#what-breaks-when-a-setting-is-missing)). |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:3000`, `:5173` and `:5174` | Other web addresses whose pages may call the API from a browser, comma-separated, each in full, such as `https://app.example.com`. A dashboard at the API's own address, as in the Compose stack, needs none. A listed address may send the sign-in cookie, so a dashboard at another address can sign in. `*` on its own lets any site call the API with an API key, never with the sign-in cookie. Velox refuses to start on an entry with a `*` inside, such as `https://*.example.com`, or on `*` beside other addresses. |
| `SMTP_HOST`, `SMTP_PORT` | unset | Your email server. If unset, Velox sends no email. See [email setup](ops/email-setup.md). |
| `VELOX_BOOTSTRAP_TOKEN` | unset | Allows the `POST /v1/bootstrap` call that creates the first tenant. At least 16 characters outside `local`. |
| `METRICS_TOKEN` | unset | The bearer token a scraper must send to read `/metrics`. If unset, `/metrics` answers 401 in `production` and is open elsewhere. |
| `VELOX_RATE_LIMIT_GENERAL_PER_MIN` | `100` | Requests a minute for each API key, for each tenant's dashboard users together, or for each client address before sign-in. Sign-in attempts from one address use the same limit. A whole number of 1 or more; any other value stops start-up. |
| `VELOX_RATE_LIMIT_INGEST_PER_SEC` | `1000` | Usage events and LiteLLM callbacks a second for each API key. A whole number of 1 or more; any other value stops start-up. |
| `INVOICE_GRACE_PERIOD` | `1h` | How long Velox waits after a billing period ends before it builds the invoice, so usage that arrives late but belongs to the period is billed. A duration from `0` to `72h`, such as `30m` or `24h`. Any other value stops start-up. What the grace delays, and what closes a period at once: [Invoice timing and late usage](ops/runbook/invoice-timing-late-usage.md#why). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | Your OpenTelemetry collector's OTLP/HTTP address, such as `http://otel-collector:4318`. If unset, Velox sends no traces. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | all traces | Which traces to keep, using the standard OpenTelemetry values. To keep 1 in 10, set `parentbased_traceidratio` and `0.1`. |

Stripe is not set here. Each tenant connects its own Stripe account in the dashboard, under Settings → Payments. The Stripe API keys and the webhook signing secret (`whsec_…`) are stored per tenant, in the database.

## Repeated wrong passwords

Velox slows repeated wrong passwords instead of locking the account. The count is kept in Postgres, so every copy shares it. How it works and how to let someone back in: [Sign-in throttled](ops/runbook/sign-in-throttled.md).

## How much load it takes

Measured load, and what to do when you need more: [How much traffic Velox handles](scale.md).

## What to watch

Velox serves Prometheus metrics at `/metrics` and writes structured logs to standard output. In `production`, set `METRICS_TOKEN` and have Prometheus send it as a bearer token; without it, `/metrics` answers 401.

Velox ships one Grafana dashboard, [`ops/grafana-dashboard.json`](ops/grafana-dashboard.json). Import it into Grafana and point it at the Prometheus that scrapes `/metrics`. It shows HTTP traffic, billing runs, payments, payment retries, the email and webhook queues, and tax.

What to alert on, and what to do when an alert fires: [Monitoring](ops/runbook/monitoring.md).

**Next:** [Run Velox in production on one VM](production-one-vm.md).
