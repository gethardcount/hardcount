# Self-host Hardcount

Hardcount is one Go program that talks to Postgres. In production it also needs Redis.

## Two ways to run it

| | Good for | What you run |
|---|---|---|
| **One VM with Docker Compose** | Trying Hardcount, and production for a small team where a few minutes of downtime during a restart is fine | Five containers on one machine |
| **Two or more copies behind a load balancer** | High availability: production where a restart or a crashed copy must not cause downtime | Two or more `hardcount-api` copies, a managed Postgres, a managed Redis |

Start with one VM; move to two or more copies when downtime during a restart is no longer acceptable.

- **One VM:** follow [Production on one VM](production-one-vm.md). To try Hardcount on your own computer first, use the [quick start](quick-start.md).
- **Two or more copies:** `hardcount-api` behind a load balancer, with a managed Postgres and Redis. Setup, health checks, shutdown and upgrades: [High availability](ops/high-availability.md).

A Helm chart and a Terraform module are not written yet.

Running Hardcount from source for development: [CONTRIBUTING](../CONTRIBUTING.md#development-setup).

## Postgres

The Compose stack ships Postgres 16, set up as Hardcount needs it. Bringing your own Postgres? Follow [Postgres requirements](ops/postgres-requirements.md): Postgres 16, three extensions, two database users and one required timeout.

Backups and the encryption key: [Back up Hardcount](ops/backup-considerations.md).

## Database upgrades (migrations)

A migration is a versioned script that changes the database schema. Hardcount applies them with [goose](https://github.com/pressly/goose); they are in [`internal/platform/migrate/sql/`](../internal/platform/migrate/sql/).

- **See the plan first.** `hardcount migrate status` shows the database's version, the migrations this release would apply, and whether a migration is running now. It changes nothing.
- Set `RUN_MIGRATIONS_ON_BOOT=true` and Hardcount applies any new ones when it starts. The Compose stack sets it. The default is `false`: then run `hardcount migrate` before you start a new release, because Hardcount refuses to start while a migration is missing.
- If several copies start at once, they take turns: one applies the migrations and the others wait, then find nothing left to do.
- **A failed migration leaves nothing to repair by hand.** Fix the cause the error names and run the upgrade again. A failed index build can leave an INVALID index, which the next run drops and builds again. See [A database upgrade stopped](ops/runbook/migration-failed-midway.md).
- **Never roll back in production.** `hardcount migrate rollback` runs only with `APP_ENV=local`. To go back after an upgrade, restore the backup you took before it; see [Going back](upgrades.md#going-back).
- **Each migration waits at most 5 seconds for a table lock**, then stops and leaves nothing behind. `MIGRATION_LOCK_TIMEOUT` changes the limit. See [It waited 5s for a table lock](ops/runbook/migration-failed-midway.md#it-waited-5s-for-a-table-lock).

## Upgrading

[Upgrade Hardcount](upgrades.md): what to do before you upgrade, the upgrade on one VM or on several copies, and how to go back. A Velox install cannot be upgraded: see [Coming from Velox](upgrades.md#coming-from-velox).

## Settings

Hardcount reads its settings from environment variables. On the Compose stack, set them in `deploy/compose/.env`, which starts from [`deploy/compose/.env.example`](../deploy/compose/.env.example); from source, start from the repository's [`.env.example`](../.env.example).

Every setting, with its default and what it does: [Settings reference](settings.md).

## Repeated wrong passwords

Hardcount slows repeated wrong passwords instead of locking the account. The count is kept in Postgres, so every copy shares it. How it works and how to let someone back in: [Sign-in throttled](ops/runbook/sign-in-throttled.md).

## How much load it takes

Measured load, and what to do when you need more: [How much traffic Hardcount handles](scale.md).

## What to watch

Hardcount serves Prometheus metrics at `/metrics` and writes structured logs to standard output. In `production`, set `METRICS_TOKEN` and have Prometheus send it as a bearer token; without it, `/metrics` answers 401.

Hardcount ships one Grafana dashboard, [`ops/grafana-dashboard.json`](ops/grafana-dashboard.json). Import it into Grafana and point it at the Prometheus that scrapes `/metrics`. It shows HTTP traffic, billing runs, payments, payment retries, the email and webhook queues, and tax.

What to alert on, and what to do when an alert fires: [Monitoring](ops/runbook/monitoring.md).

**Next:** [Run Hardcount in production on one VM](production-one-vm.md).
