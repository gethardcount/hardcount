# Run two or more copies of Hardcount (high availability)

Run two or more copies of the Hardcount API behind a load balancer. While one copy restarts or crashes, the others keep serving.

Downtime during a restart is fine for you? Use [Production on one VM](../production-one-vm.md).

## What you need

- Two or more copies of the Hardcount server behind a load balancer that ends HTTPS. In the Compose stack this is the `hardcount-api` service.
- A managed Postgres with automatic failover and point-in-time restore.
- A managed Redis. Hardcount uses it only for rate limiting, and all copies share its counts. If Redis goes down after start-up, see [Redis going down after boot](runbook/replica-will-not-start.md#redis-going-down-after-boot).
- A connection pooler is optional: [Postgres requirements → Connection poolers](postgres-requirements.md#connection-poolers).

The settings for each copy are in [Settings](../settings.md). Every copy uses the same settings and the same database.

## What runs

- **The server:** the image `ghcr.io/gethardcount/hardcount:<release>`, for example `v0.9.1`. Its entrypoint is `hardcount`; with no arguments it serves the API and runs the background jobs, on port 8080 (`PORT`). Run `hardcount migrate` once before new copies start on a new release: [Upgrades](#upgrades).
- **The dashboard:** the image `ghcr.io/gethardcount/hardcount-dashboard:<release>`, static files on port 80. It holds no state, so any number of copies works. It calls the API on its own address, so serve both from one address: send `/v1/` and `/health` to the server copies and everything else to the dashboard. The Compose stack's nginx does exactly this ([`deploy/compose/nginx.conf`](../../deploy/compose/nginx.conf)).
- **HTTPS at the load balancer.** In `staging` and `production` the sign-in cookie is `Secure`, so the dashboard's sign-in works only over HTTPS.

## How the copies share the work

Each copy answers API requests. Background jobs are different: billing runs, payment retries (dunning), and sending webhooks and emails must run once, not once per copy. Two things make sure of that, so copies do not bill or send twice:

- **One copy runs each job at a time.** Before each run of a job, a copy takes a short lease: a row in Postgres that says "this copy runs this job now" (see [leader lease](../glossary.md)). The other copies stand by. If the copy holding a lease dies, another one takes over within about 15 seconds.
- **Queued rows are claimed once.** Webhooks and emails wait in queue tables (outboxes) until a background worker sends them. A worker skips any row another worker already holds, instead of waiting for it (Postgres `SKIP LOCKED`).

A few features still assume one copy: the dashboard's live webhook stream, test-clock advances, and one billing log note. None of them touch money.

## Set up the load balancer

1. Point the load balancer's health check at `GET /health/ready`.

   ```bash
   curl -fsS https://your-hardcount-host/health/ready
   ```

   **You see:** `{"checks":{"api":"ok","database":"ok","scheduler":"..."},"status":"ok"}`, with status 200.

2. Set `TRUST_PROXY` on every copy to the address range (CIDR) of the load balancer. Separate several ranges with commas.

   Why it matters: [Self-host → Settings](../self-host.md#settings). Set `TRUST_PROXY=none` only when the copies take client connections directly, with no proxy in front.

   **You see:** the copy starts. In `production`, a copy with `TRUST_PROXY` unset refuses to start with `TRUST_PROXY is required in production`.

3. Set `SHUTDOWN_DRAIN_DELAY` to at least the load balancer's health-check interval times its unhealthy threshold. For an AWS ALB with its defaults, that is 30s × 2, so set `SHUTDOWN_DRAIN_DELAY=60s`. Use `0` when the orchestrator removes the copy from the load balancer itself, as Kubernetes does. The default is 5s outside `local`.

   **You see:** when you stop a copy, `/health/ready` on that copy answers 503 `{"status":"draining"}` for that long, while it still serves requests.

4. Set the shutdown grace period (Kubernetes `terminationGracePeriodSeconds`) above `SHUTDOWN_DRAIN_DELAY` plus 90 seconds: 120s with the 5s default, 180s with `SHUTDOWN_DRAIN_DELAY=60s`. The Compose stack sets `stop_grace_period: 120s`.

5. Set the load balancer's timeouts. Its read or idle timeout must be above 35 seconds, the server's write timeout. Then a slow request gets the server's own 504, not a dropped connection. The Compose stack's nginx uses 40s. The dashboard's live webhook log (`/v1/webhook_events/stream`) is a long-lived stream: turn response buffering off for that path and allow it at least an hour, or the log stops updating.

6. Check the database connections. Each copy opens up to `DB_MAX_OPEN_CONNS` connections (default 20), plus a few while it starts. Copies × `DB_MAX_OPEN_CONNS` must stay below your Postgres `max_connections`.

## Health checks

| Endpoint | What it tells you |
|---|---|
| `GET /health` | Liveness: 200 while the process runs. |
| `GET /health/ready` | Readiness: 200 when the database is reachable. 503 when it is not, or when the copy is shutting down. The body also reports the billing scheduler, which does not change the status code. |

Neither endpoint is rate limited or written to the audit log.

## What happens when a copy stops

On a stop signal (SIGTERM), a copy first tells the load balancer it is leaving: `/health/ready` answers 503 while the copy still accepts connections. Then it finishes its work in three stages, each with a time limit.

| Step | What happens | Time limit |
|---|---|---|
| Readiness drain | `/health/ready` answers `503 {"status":"draining"}`; `/health` stays 200. The copy keeps listening, so the load balancer has time to stop sending it requests. | `SHUTDOWN_DRAIN_DELAY` (default 5s outside `local`) |
| 1. Requests in progress | The dashboard's live webhook stream is closed first, so it cannot hold this stage open. | Up to 30s |
| 2. A test-clock advance in progress | Stopped at the limit. The clock stays `advancing` until a copy restarts. See [Test-clock advance hung](runbook/test-clock-advance-hung.md). | Up to 30s |
| 3. Background jobs | | Up to 30s |

The worst case is `SHUTDOWN_DRAIN_DELAY` plus 90 seconds. Usually the three stages take under 2 seconds after the drain delay.

A grace period shorter than that can kill a copy (SIGKILL) halfway through. Hardcount is built to survive that: queued webhooks and emails a copy had claimed are picked up again when its lease runs out, and the next copy to start recovers a killed test-clock advance. The cost is that a few webhooks or emails can be sent twice.

## Upgrades

[Upgrade two or more copies](../upgrades.md#upgrade-two-or-more-copies) says in which order to upgrade the copies, and [Going back](../upgrades.md#going-back) how to undo an upgrade.

**Next:** [A background job is stuck](runbook/background-job-stuck.md): what to check when billing or another background job stops.
