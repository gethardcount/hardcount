# Contributing to Hardcount

Hardcount is a billing system you run yourself, written in Go on PostgreSQL. It
counts what your customers use, works out each bill, makes the invoice, and
charges the card through Stripe.

This page takes you from a fresh clone to a pull request that passes CI. It
shows how to run Hardcount from source, where the code goes, and which checks CI
runs on your change.

## Development setup

You need Go 1.26 or newer and Docker. Node 22 or newer is needed only for
dashboard work, and `jq` only for `scripts/demo.sh`.

```bash
git clone https://github.com/gethardcount/hardcount.git
cd hardcount
cp .env.example .env
```

1. **Start Postgres.**

   ```bash
   docker compose up -d postgres
   ```

   **You see:** `docker compose ps` lists the `postgres` service as
   `healthy` after a few seconds.

2. **Create your first account and its keys.** This also runs the database
   migrations.

   ```bash
   make bootstrap
   ```

   **You see:** a "Hardcount Bootstrap Complete" box with the dashboard sign-in
   (email `admin@hardcount.local` unless you set `HARDCOUNT_BOOTSTRAP_EMAIL`), a
   generated password, and three API keys. Save the password: it is shown
   only once. Copy the key under "Secret Key — TEST mode"
   (`hc_secret_test_…`).

3. **Start the API.** `make dev` reads `.env`, which points at the local
   Postgres and runs migrations on boot.

   ```bash
   make dev
   ```

   **You see:** a JSON log line with `"msg":"listening"` and
   `"addr":":8080"`. `curl http://localhost:8080/health` returns
   `{"status":"ok"}`.

4. **Start the dashboard**, in a second terminal.

   ```bash
   cd web-v2 && npm install && npm run dev
   ```

   **You see:** Vite prints `http://localhost:5173/`. Open
   `http://localhost:5173/login` and sign in with the email and password
   from step 2. The dashboard opens in test mode, where nothing charges real
   money.

5. **Bill a test customer end to end**, in a third terminal. This is the
   golden path: the one flow CI also runs on every PR, from loading prices
   to a finished invoice.

   ```bash
   ./scripts/demo.sh hc_secret_test_...   # the key from step 2
   ```

   **You see:** nine numbered steps, each with a green `✓` line, then
   "Done: every step above ran and was checked" and two dashboard links,
   one to the invoice and one to the customer.

Run the tests with Postgres still up:

```bash
go test ./... -race -short -count=1         # unit tests (CI runs -race too)
go test -p 1 ./... -count=1 -short=false    # unit + integration tests
```

To try a release without Go or Node, use `deploy/compose/quickstart.sh`
instead (see the [README](README.md)).

### Local services

`docker compose up -d postgres redis mailpit` starts everything the API can
use on your machine:

| Service | Where | Notes |
|---|---|---|
| Postgres | `:5432` | Password `hardcount`. Data is kept in a Docker volume. |
| Redis | `:6379` | Used for rate limiting. Optional outside production: without it, only sign-in (`/v1/auth`) is rate-limited. |
| Mailpit | `:1025` (SMTP), `:8025` (web) | Catches every email Hardcount sends, so nothing leaves your machine. |
| The API | `:8080` | Started by `make dev`. Not a container. |

`make bootstrap` takes an optional owner email and password:
`HARDCOUNT_BOOTSTRAP_EMAIL=you@example.com HARDCOUNT_BOOTSTRAP_PASSWORD=change-me-please1 make bootstrap`.
A password needs at least 12 characters, and Hardcount refuses common passwords
from leaked-password lists.

### Add a second account

One Hardcount server can hold many accounts (tenants), each with its own data,
dashboard users and API keys. To add another one, run the bootstrap tool
again with a different owner email:

```bash
make bootstrap HARDCOUNT_BOOTSTRAP_EMAIL=tenant-b@hardcount.local \
  HARDCOUNT_BOOTSTRAP_PASSWORD='choose-a-password' \
  HARDCOUNT_BOOTSTRAP_TENANT='Tenant B'
```

The tool runs from source, so it needs Go and this repository. It is not in
the Docker image. There is no button for this in the dashboard and no API
route: no key can create an account or read another one
([ADR-133](docs/adr/133-api-key-types-secret-and-ingest.md)).

### Several worktrees

Never `git stash` in a repo shared by several worktrees. The stash belongs
to the whole repo, and a `pop` can land another worktree's work in your
tree.

### Without make

These are the same steps without make. `make dev` reads `.env`, which sets
`DATABASE_URL` and `RUN_MIGRATIONS_ON_BOOT=true`. Here you set them on the
command line:

```bash
docker compose up -d postgres
DATABASE_URL="postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable" go run ./cmd/hardcount-bootstrap
DATABASE_URL="postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable" RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/hardcount
```

To run only the migrations:

```bash
DATABASE_URL="postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable" go run ./cmd/hardcount migrate
```

### Run your change in the Compose stack

This builds the API image from your checkout and runs it in the same stack
as a [one-VM install](deploy/compose/README.md):

```bash
docker build -t hardcount:local .        # from the repo root
cd deploy/compose
cp .env.example .env
$EDITOR .env   # set POSTGRES_PASSWORD, HARDCOUNT_APP_DB_PASSWORD, HARDCOUNT_ENCRYPTION_KEY, HARDCOUNT_BOOTSTRAP_TOKEN
HARDCOUNT_IMAGE=hardcount:local docker compose up -d
```

**You see:** `docker compose logs -f hardcount-api` prints a log line with
`"msg":"listening"` and `"addr":":8080"` when the API is ready.

To build the image and run it on its own, against a Postgres you already have:

```bash
docker build -t hardcount:latest .
docker run --rm -e DATABASE_URL="..." -p 8080:8080 hardcount:latest
```

## Project structure

Each billing domain (invoice, credit, customer, and so on) is a
self-contained package under `internal/`. [Architecture](docs/architecture.md)
shows how the packages work together.

```
internal/{domain}/
  store.go       — Store interface (what the domain needs from persistence)
  postgres.go    — PostgreSQL implementation of the store
  service.go     — Business logic (validates, orchestrates)
  handler.go     — HTTP handlers (decodes request, calls service, writes response)
  *_test.go      — Unit tests (in-memory store) + integration tests (real Postgres)
```

**Rules:**

- One domain package never calls another domain's `Service` or `Store`
  directly. A package may import shared value types, cross-cutting
  infrastructure (`auth`, `audit`, `session`), and the small interfaces of
  the `billing` coordinator. The allowlist in
  `internal/arch/boundaries_test.go` enforces this: a new cross-domain
  import fails the test until it is added there on purpose.
- Every handler uses `respond.JSON()` / `respond.FromError()` for responses.
- Most store methods run inside a transaction scoped to one tenant by
  Postgres Row-Level Security (RLS):
  `db.BeginTx(ctx, postgres.TxTenant, tenantID)`. A tenant is one account's
  data in a shared database; RLS hides every other tenant's rows.
- Some store methods run under `postgres.TxBypass`, where RLS does not
  apply: work across tenants, work before the tenant is known, and two
  signed-in areas. Such a query must filter by `tenant_id` itself.
  [Architecture](docs/architecture.md#design-rules) lists them. A new use
  fails `internal/arch/txbypass_allowlist_test.go` until it is listed there.
- Integration tests use `testutil.SetupTestDB()`.

## Adding a new domain

1. Create `internal/{domain}/store.go` with the store interface.
2. Create `internal/{domain}/postgres.go` implementing the store.
3. Create `internal/{domain}/service.go` with business logic.
4. Create `internal/{domain}/handler.go` with HTTP handlers using the
   `respond` package.
5. Add a migration with `make migration name=...`, then follow
   [Writing a database migration](docs/dev/migrations.md).
6. Wire it into `internal/api/router.go`.
7. Write tests at the layers [How we test](docs/testing.md#which-test-to-write)
   names for your change.

## Code style

- `go fmt` and `go vet` must pass.
- No exported types without doc comments.
- Errors use `errs.DomainError` for domain-specific errors or `fmt.Errorf`
  for validation.
- JSON field names are `snake_case`.
- ID format: `hc_{type}_{random_hex}` (for example `hc_cus_abc123`).

## Frontend (web-v2)

The dashboard is a React + TypeScript + Vite app. See
[`web-v2/README.md`](web-v2/README.md) for setup, the dev server, and the
directory's own CI checks. Two things often surprise first-time contributors:

- `npm install` relies on `web-v2/.npmrc`. That file tolerates a
  peer-dependency mismatch on purpose, so do not "fix" the mismatch.
- Typechecking is `npx tsc -b`. Do not use `tsc --noEmit`: here it checks
  nothing and reports no error.

## What CI runs on your PR

CI runs every check below on every PR. Run the ones your change touches
locally before you push. Fork PRs get the same run, because no check uses
secrets.

**Required to merge.** A PR cannot merge until each of these passes on a branch that is up to date with `main`. If `main` moved after your last push, use **Update branch** on the PR (or rebase) and let the checks run again: two PRs that each add the same migration number cannot both merge that way.

| CI job | What it checks | Local command |
|---|---|---|
| `unit` | Unit tests with the race detector, then `go vet`. Includes the source-rule tests in `internal/arch` | `go test ./... -race -short -count=1` |
| `test` | Build, module checksums, and the integration tests against a real Postgres | `go test -p 1 ./... -count=1 -short=false` (needs `docker compose up -d postgres`; test databases are created for you) |
| `test-pgbouncer` | The same suite through PgBouncer, a connection pooler, in transaction mode | `scripts/pgbouncer-test.sh` (needs Docker) |
| `golden-path` | Boots a real server, runs `scripts/demo.sh` from prices to a finished invoice, then runs hardcount-doctor over the rows it wrote | Steps 2 to 5 of [Development setup](#development-setup), then `go run ./cmd/hardcount-doctor` |
| `lint` | golangci-lint (`.golangci.yml`), the Go version in `go.mod` against the `Dockerfile`, the quickstart release against the Compose template, and `govulncheck` | `make lint`, then `govulncheck ./...` |
| `codegen-drift` | Generated Go and TypeScript match `api/openapi.yaml` | `make gen && git diff --exit-code` |
| `frontend` | Dashboard build (`tsc -b` and Vite), unit tests, eslint, and knip (unused files, exports and dependencies) | `cd web-v2 && npm run build && npm test && npm run lint` |
| `e2e` | Playwright tests against a real API and dashboard. Setup: [`web-v2/e2e/README.md`](web-v2/e2e/README.md) | `cd web-v2 && npm run test:e2e` |
| `pricing-shapes` | Each common way to price, billed end to end and checked to the cent, plus the expected results of each price shape. The `test` job runs these too; this job gives them a check of their own | The `go test` lines in this job in `.github/workflows/ci.yml` (needs Postgres) |
| `migrations` | A fresh install on an empty database under three role setups, then a second run that has nothing to do. Also a backup-restore drill and an install on Postgres 14 | `scripts/ci/migrations-fresh-install.sh` (needs Docker, `psql` and `pg_restore`) |

**Also run, not required to merge.**

| CI job | What it checks | Local command |
|---|---|---|
| `upgrade` | Installs the previous release from its tag and bills a month with its demo, then starts HEAD on the same database. Every invoice from before must read the same through the API, HEAD must bill a new month, and hardcount-doctor must find nothing | `scripts/ci/upgrade-from-previous-tag.sh` (needs Docker) |
| `doc-links` | Every internal Markdown link and `#anchor` resolves. It turns red on a broken link; fix it before merging | `lychee --offline --include-fragments '**/*.md'` |
| `doc-style` | Vale prose lint on the lines your PR adds. It comments only and never fails | `vale <changed .md files>` |

The `docker` job builds and publishes the images. It runs only on `main`
and on release tags, after the jobs above pass.

Notes on three of them:

- **hardcount-doctor** is a read-only command that scans a database for states
  no correct code could produce, such as invoice totals that do not add up.
  Point `DATABASE_URL` at the owner connection: under the app's RLS role it
  sees no rows and reports clean.
- **`govulncheck`** can fail when a new Go vulnerability is published, even
  if your change is fine. Say so in the PR and we will handle it.
- **Lint waivers and source rules** (the `//nolint:forbidigo` reasons and
  the `//tz:ok` date rule) are described in
  [How we test](docs/testing.md#which-test-to-write).

## What we accept

Anyone can contribute. We judge the change, not the person. To keep review
time for real work, every pull request meets this bar:

1. **Start with an issue.** For anything bigger than a typo or a broken
   link, open an issue first, or comment on an existing one. Wait until a
   maintainer agrees with the approach, then link that issue in your pull
   request. A pull request without an agreed issue is closed without review.
2. **Some areas are maintainer-only.** A change in these areas needs an
   agreed issue that says an outside pull request is welcome:
   - money and state machines: invoices, payments, credits, dunning,
     subscriptions and tax;
   - database migrations;
   - sign-in, sessions and API keys;
   - CI and release files (`.github/`);
   - dependencies (`go.mod`, `web-v2/package.json` and the lock files).
3. **One change per pull request,** with its tests and docs, and CI green.
   Unrelated edits, such as formatting sweeps, renames or bulk dependency
   bumps, are closed.
4. **Show that you ran it.** Say what you changed, why, and how you checked
   it. A pull request that was not run or understood is closed.
5. **AI help is fine; say so.** If an AI tool wrote part of the change,
   say which part in the description. You answer for every line: in review,
   you can explain what it does and why. A change you cannot explain is
   closed.

**Never accepted:** links that promote you or a product, batches of tiny
edits made to collect contributions, and changes to the licence or the
credits.

**What happens next:** CI on a pull request from outside the project waits
until a maintainer has read the diff and approved the run. We answer within
two days. A pull request below the bar gets one short comment and is
closed; you are welcome to open an issue and try again.
Repeated pull requests below the bar, or behaviour the
[code of conduct](CODE_OF_CONDUCT.md) rules out, lead to a block.

## Pull requests

1. Fork the repo and create a feature branch.
2. Write tests for new behavior, at the layer
   [How we test](docs/testing.md) names for your change.
3. Run the checks above that your change touches (at least the two test
   commands).
4. Update the docs in the same PR. Every user-visible change gets a
   `CHANGELOG.md` entry, and every UI change is covered by a Playwright
   spec in `web-v2/e2e/`. A doc that no longer matches the code is a bug.
   For how to write docs, see the
   [writing style guide](docs/dev/writing-style.md).
5. Open the PR with a clear description of what changed and why.

**Touching money or a state machine?** This covers invoices, payments,
credits, dunning (retrying a failed card payment and emailing the
customer), subscriptions and tax. Follow the
[Money-path robustness playbook](docs/dev/money-path-robustness-playbook.md):

- Before opening the PR, work through its implementation checklist
  ([§3](docs/dev/money-path-robustness-playbook.md#3-implementation-checklist--gates-before-opening-a-money-path-pr)).
- In the PR description, list the state's site-set
  ([§2](docs/dev/money-path-robustness-playbook.md#2-the-meta-practice-complete-site-set-enumeration)):
  every place in the code that writes that state or reacts to it.
- A guard against a race or a broken money rule needs an automated test.
  The test forces the race, and you show it fails once the guard is
  removed ([§5](docs/dev/money-path-robustness-playbook.md#5-test-lock-doctrine)).

Words used here that you have not met are in the
[glossary](docs/dev/glossary.md).
