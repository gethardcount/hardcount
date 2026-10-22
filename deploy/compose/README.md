# The Docker Compose stack

This folder runs Velox on one machine: Postgres, Redis, the Velox API, the dashboard, and nginx in front of them.

- **Try it on your own computer:** run `./quickstart.sh` here. It uses the published images and local mode, so sign-in works at `http://localhost:8080` in every browser. Walkthrough: [quick start](../../docs/quick-start.md).
- **Install it on a server:** follow [Production on one VM](../../docs/production-one-vm.md). It installs a pinned release, then adds HTTPS, email, Stripe, nightly backups and a go-live checklist.

## Build from source instead

Production on one VM runs the published release images, set by `VELOX_IMAGE` and `VELOX_DASHBOARD_IMAGE` in `.env`. Leave those two lines out, and `docker compose up -d` builds both images from your checkout. The first build takes a few minutes. Everything else is the same.

To upgrade a stack built from source, run these in `deploy/compose`. This example moves to `v0.9.0`.

1. Take a backup. [Production on one VM, step 7](../../docs/production-one-vm.md#7-back-up-every-night) sets up this command. If your checkout is not in `/opt/velox`, change its two `/opt/velox` paths.

   ```bash
   sudo velox-backup
   ```

   **You see:** `Verified:  yes`.

2. Check out the release and build it.

   ```bash
   git fetch --tags
   git checkout v0.9.0
   docker compose build
   ```

   **You see:** `HEAD is now at …` from git, then the two images build.

3. See what the upgrade will change. `migrate status` only reads.

   ```bash
   docker compose run --rm velox-api migrate status
   ```

   **You see:** the plan. If it says `ahead`, this install ran newer code than the release: check out a newer release and build again.

4. Start the new build. It applies the database upgrades as it starts.

   ```bash
   docker compose up -d
   ```

   **You see:** after a few seconds, `curl -fsS http://localhost/health/ready` (`localhost:8080` if `NGINX_HTTP_PORT=127.0.0.1:8080`, as in Production on one VM) answers `"status":"ok"` again.

Read the [changelog](../../CHANGELOG.md) upgrade notes first, as for any upgrade. If the upgrade stops with an error, see [If something goes wrong](#if-something-goes-wrong).

## Sign in before HTTPS

With `APP_ENV=production`, the default in this stack, the sign-in cookie is always marked `Secure`. Browsers keep a `Secure` cookie on HTTPS. At `http://localhost`, Chrome and Firefox keep it too; Safari does not. At any other plain-HTTP address, such as `http://<server-ip>/`, no browser keeps it, and the sign-in page says so.

Until you add HTTPS, sign in at `http://localhost/` on the server in Chrome or Firefox, or through an SSH tunnel from your laptop:

```bash
ssh -L 8080:localhost:80 you@server   # localhost:8080 instead if NGINX_HTTP_PORT=127.0.0.1:8080
# then open http://localhost:8080/
```

## Day-to-day commands

| To | Run |
|---|---|
| See logs | `docker compose logs -f` |
| Restart the API | `docker compose restart velox-api` |
| Stop, keeping data | `docker compose stop` |
| Stop and delete all data | `docker compose down -v` |
| See the database's version and the upgrades still to apply | `docker compose run --rm velox-api migrate status` |
| Open a database shell | `docker compose exec postgres psql -U velox -d velox` |

To upgrade, see [Upgrade to a new release](../../docs/production-one-vm.md#upgrade-to-a-new-release), or [Build from source instead](#build-from-source-instead).

## If something goes wrong

To see the settings the API container runs with, ask Docker. The image has no shell and no `env` command, so `docker compose exec velox-api env` fails.

```bash
docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(docker compose ps -aq velox-api)"
```

The output holds passwords and keys, so keep it private.

**The API exits with `VELOX_ENCRYPTION_KEY is required in production`.**
Set the key in `.env` (`openssl rand -hex 32`) and run `docker compose up -d` again.

**The API exits with `APP_DATABASE_URL is required in production` or `default/guessable password`.**
Set `VELOX_APP_DB_PASSWORD` in `.env` and start the stack again. Compose builds `APP_DATABASE_URL` from it.

**The API exits with `could not open the app database connection`.**
The `velox_app` user is missing, or its password differs from `VELOX_APP_DB_PASSWORD`. Postgres creates that user only on its very first start, on an empty data volume. On an existing volume, set the password yourself:

```bash
docker compose exec -e VELOX_APP_DB_PASSWORD postgres \
  psql -U velox -d velox -v pw="$VELOX_APP_DB_PASSWORD" \
  -c "ALTER ROLE velox_app PASSWORD :'pw'"
```

If the user does not exist at all, create it with the SQL in [Postgres requirements](../../docs/ops/postgres-requirements.md#the-two-database-users).

**The API exits with `role can BYPASS row-level security`.**
`APP_DATABASE_URL` points at the admin user, usually because `DATABASE_URL` was copied into it. Point it at `velox_app`.

**The API exits with `migration … failed`, or names `goose_db_version`.**
A database upgrade stopped. A failed migration leaves nothing to repair. Compose keeps starting the API again, and each start tries the upgrade again, so stop it first with `docker compose stop velox-api`. Fix the cause the error names, then start it with `docker compose up -d velox-api`. The cases are in [A database upgrade stopped](../../docs/ops/runbook/migration-failed-midway.md).

**`/health/ready` shows `scheduler: degraded`.**
The API is up and still answers 200 here, but its background jobs have stopped running. Check the API logs, then see [A background job is stuck](../../docs/ops/runbook/background-job-stuck.md).

**`/metrics` returns 401.**
`METRICS_TOKEN` is not set, or the scraper did not send it. Set it in `.env` (`openssl rand -hex 32`), start the stack again, and have Prometheus send it as a bearer token.

**Port 80 is already in use.**
Set `NGINX_HTTP_PORT=8080`, or any free port, in `.env` and start the stack again.

**Sign-in says it does not work at this address, or Safari does not keep you signed in.**
You opened the dashboard over plain HTTP. See [Sign in before HTTPS](#sign-in-before-https).

**Next:** [Run Velox in production on one VM](../../docs/production-one-vm.md).
