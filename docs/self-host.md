# Self-host Velox

This page is for the engineer who installs and runs Velox. It covers the two ways to run it, what Postgres needs, the settings, how much load it takes, and what to watch.

Velox is one Go program that talks to Postgres. In production it also needs Redis, which it uses only for rate limiting.

## Two ways to run it

| | Good for | What you run |
|---|---|---|
| **One VM with Docker Compose** | Trying Velox, or a small install where a few minutes of downtime during a restart is fine | Five containers on one machine |
| **Two or more copies behind a load balancer** | Production, where a restart must not cause downtime | Two or more `velox-api` copies, a managed Postgres, a managed Redis |

A Helm chart and a Terraform module are not written yet. They will be when an early customer tells us which platform they run.

## One VM with Docker Compose

Follow the step-by-step guide: [`deploy/compose/README.md`](../deploy/compose/README.md).

In short:

1. Set four secrets.
2. Run `docker compose up -d`.
3. Make one `POST /v1/bootstrap` call. It returns your dashboard login and your API keys.

That gives you a working [tenant](glossary.md): one business account with its own data, dashboard users and API keys. One install can hold several tenants.

**Signing in before you add TLS.** The stack serves plain HTTP on port 80. Until you put TLS in front, sign in to the dashboard at `http://localhost/` on the server, or through an SSH tunnel. Sign-in does not work at `http://<server-ip>/`. The [compose guide](../deploy/compose/README.md#tls) covers both.

**What to expect from one VM:**

- There is one copy of the API. While it restarts, Velox is down.
- Postgres runs on the same machine, unless you point `DATABASE_URL` at one elsewhere.
- There is no load balancer.

## Two or more copies behind a load balancer

This is the supported production setup:

- Two or more `velox-api` copies behind a load balancer.
- A managed Postgres with automatic failover and point-in-time restore.
- A managed Redis.

Things to know:

- **Background jobs run once, not once per copy.** Billing runs, payment retries and webhook delivery are background jobs. The copies agree, through a row in Postgres, on which one runs each job. The others stand by, and one takes over within about 15 seconds if the first dies.
- **Connection poolers work.** PgBouncer in transaction mode and RDS Proxy both work for the server. Run database upgrades (migrations) over a direct connection.
- **Give each copy 120 seconds to shut down.** On a stop signal, a copy first tells the load balancer it is leaving, then finishes its work. The worst case is 90 seconds.
- **A few features still assume one copy.** The live event stream in the dashboard is one. None of them touch money. The full list is in the [readiness review](dev/ha-readiness-2026-07-06.md).

Rollouts, upgrades and scaling: [`deploy/README.md`](../deploy/README.md).

## Working on Velox locally

This runs the API from source on your machine. It is for development, not for hosting.

```bash
git clone https://github.com/getvelox/velox.git
cd velox

cp .env.example .env   # make dev reads it; the defaults work as they are
docker compose up -d postgres redis mailpit
VELOX_BOOTSTRAP_EMAIL=you@example.com VELOX_BOOTSTRAP_PASSWORD=change-me-please1 \
  make bootstrap
make dev
```

The email and password are optional. Without them, the owner is `admin@velox.local` and bootstrap prints a generated password. A password needs at least 12 characters.

You now have:

| Service | Where | Notes |
|---|---|---|
| Postgres | `:5432` | Password `velox`. Data is kept in a Docker volume. |
| Redis | `:6379` | Used for rate limiting. Optional outside production. |
| Mailpit | `:1025` (SMTP), `:8025` (web) | Catches every email Velox sends, so nothing leaves your machine. |
| The API | `:8080` | Started by `make dev`. Not a container. |

The dashboard runs separately:

```bash
cd web-v2 && npm install && npm run dev
# → http://localhost:5173
```

## Postgres

The Compose stack ships Postgres 16 with default settings, which is enough for trying Velox. For your own Postgres:

- **Version:** 16.
- **Extensions:** `pgcrypto`, `citext` and `btree_gist`. Velox creates them during database upgrades, so the role that runs upgrades needs permission to create extensions. If your managed Postgres forbids that, create them first: [Postgres requirements](ops/postgres-requirements.md).
- **Backups:** take regular `pg_dump` snapshots. How often depends on how much data you can afford to lose. See [backups](ops/backup-considerations.md).

### The two database users

Velox uses two database users, and the second one is what keeps tenants apart.

| Setting | User | Used for |
|---|---|---|
| `DATABASE_URL` | The admin user | Database upgrades |
| `APP_DATABASE_URL` | A limited user, usually `velox_app` | Every API request |

Postgres hides each tenant's rows from the others (a feature called Row-Level Security). An admin user can bypass that, so requests must never run as the admin.

The Compose stack creates `velox_app` for you from `VELOX_APP_DB_PASSWORD`. On your own Postgres, create it yourself:

```sql
-- use psql -v pw='...' and :'pw' quoting, or substitute a literal
CREATE ROLE velox_app WITH LOGIN PASSWORD :'pw';
GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
```

With `APP_ENV` set to `staging` or `production`, Velox refuses to start when `APP_DATABASE_URL`:

- is missing,
- uses the default password `velox_app`, or an empty one,
- cannot connect, or
- points at a user that can bypass Row-Level Security. This catches a copied `DATABASE_URL`.

With `APP_ENV=local` it builds `velox_app:velox_app` from `DATABASE_URL` and logs a warning instead.

## Database upgrades (migrations)

A migration is a versioned script that changes the database schema. Velox ships them in [`internal/platform/migrate/sql/`](../internal/platform/migrate/sql/).

- Set `RUN_MIGRATIONS_ON_BOOT=true` and Velox applies any new ones when it starts. The Compose stack and the local `.env.example` both set it. The default is `false`.
- If several copies start at once, only one applies them and the others wait.
- **Never roll back in production.** The "down" scripts exist for development. To undo a change in production, write a new migration.

## Settings

Velox reads its settings from environment variables.

**Required**

| Variable | What it is |
|---|---|
| `DATABASE_URL` | Postgres connection URL for the admin user |
| `APP_DATABASE_URL` | Postgres connection URL for the limited user. Required in `staging` and `production`. |
| `VELOX_ENCRYPTION_KEY` | Encrypts stored personal data. 64 hex characters. Required in `production`. |
| `REDIS_URL` | Redis, for rate limiting. Required in `production`. |
| `TRUST_PROXY` | The addresses of your proxy or load balancer, comma-separated. Velox then trusts the client address they forward. Use `none` if there is no proxy. Required in `production`. |

Why `TRUST_PROXY` is required: without it, every request behind a load balancer appears to come from the load balancer, so all users would share one rate limit.

**Read once, when creating the first tenant** (by `make bootstrap`, not by the server)

| Variable | What it is |
|---|---|
| `VELOX_BOOTSTRAP_EMAIL` | The dashboard owner's email. Default `admin@velox.local`. |
| `VELOX_BOOTSTRAP_PASSWORD` | The owner's password. If unset, one is generated and printed once. |
| `VELOX_BOOTSTRAP_TENANT` | The tenant's name. Default `Demo Tenant`. |

**Optional**

| Variable | Default | What it does |
|---|---|---|
| `APP_ENV` | `local` | `local`, `staging` or `production`. Outside `local`, cookies are marked secure and the start-up checks on this page apply. |
| `RUN_MIGRATIONS_ON_BOOT` | `false` | Apply database upgrades at start-up. |
| `DASHBOARD_BASE_URL` | unset | The dashboard's address, used in password-reset links. If unset, no password-reset emails are sent. For local development use `http://localhost:5173`. |
| `SMTP_HOST`, `SMTP_PORT` | unset | Your email server. If unset, Velox sends no email. The local `.env.example` points these at Mailpit; the Compose stack leaves them unset. See [email setup](ops/email-setup.md). |
| `VELOX_BOOTSTRAP_TOKEN` | unset | Allows the `POST /v1/bootstrap` call that creates the first tenant. At least 16 characters outside `local`. |

Stripe is not set here. Each tenant connects its own Stripe account in the dashboard, under Settings.

## How much load it takes

Measured on AWS, counting usage events stored per second, with every event checked against Postgres afterwards:

- **1,000 events per second**, held for five 10-minute runs, on a `db.m7g.2xlarge` database (10 events per request). All five runs passed.
- **15,000 events per second** on a `db.m7g.4xlarge` (100 events per request). Four of five runs passed.

The database ran out of memory first, then disk write speed. The Velox process was never the limit. The method and every caveat: [sustained throughput](benchmarks/sustained-throughput.md).

One small VM will take far less. To raise the limit, in this order:

1. Send events in batches. One request can carry up to 1,000 events.
2. Give the database more memory.
3. Give the database faster disks.
4. Split the `usage_events` table by month.

## What to watch

Velox serves Prometheus metrics at `/metrics` and writes structured logs to standard output. Connect both to the monitoring you already run. Velox does not ship dashboards or alert rules.

The metrics worth an alert:

| Metric | What it tells you |
|---|---|
| `velox_billing_cycle_duration_seconds` | How long a billing run takes |
| `velox_tax_outcome_total` | Tax calculations that failed, and why |
| `velox_audit_write_errors_total` | Audit log entries that failed to save |
| `velox_audit_uncovered_mutation_total` | Changes made with no audit log entry. Should stay at zero. |
| `velox_stripe_breaker_state` | Whether Velox has paused calls to Stripe after repeated failures. 0 is normal, 2 is paused. |

What to do when one fires: the [operations runbook](ops/runbook.md).

## Related

- [Tax calculation](ops/tax-calculation.md): tax providers and what happens when one fails.
- [Stripe end-to-end test](ops/stripe-end-to-end-test.md): check a real Stripe account before going live.
- [Decision records](adr/README.md): why Velox is built this way. The ones behind this page are the in-process scheduler (ADR-006), tenant isolation (ADR-003), the limited database user (ADR-073) and how copies share background jobs (ADR-114).
