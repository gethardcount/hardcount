# Contributing to Velox

Thank you for your interest in contributing to Velox.

This page is for anyone preparing a pull request. It covers how to set up a
dev environment and how the code is organized. It also lists the checks CI
runs against your change and the house rules that gate a merge.

## Development Setup

```bash
# Prerequisites: Go 1.26+, Docker; Node 22+ only for dashboard work;
# jq only for scripts/demo.sh

# Clone
git clone https://github.com/getvelox/velox.git
cd velox

# Start Postgres
docker compose up -d postgres

# Run migrations
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox migrate

# Run tests
go test ./... -race -short -count=1    # unit tests (CI runs -race too)
go test -p 1 ./... -count=1 -short=false  # unit + integration tests

# Run the API and the dashboard from source
cp .env.example .env
make bootstrap                            # prints the operator login and the API keys
make dev                                  # API on :8080
cd web-v2 && npm install && npm run dev   # dashboard on :5173, in a second terminal
./scripts/demo.sh <vlx_secret_test_... from make bootstrap>
```

To try a release without Go or Node, use `deploy/compose/quickstart.sh` instead (see the README).

### Without make

These are the same steps as above without make. `make dev` reads `.env`, which sets `DATABASE_URL` and `RUN_MIGRATIONS_ON_BOOT=true`; here you set them on the command line:

```bash
docker compose up -d postgres
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" go run ./cmd/velox-bootstrap
DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/velox
```

### Run your change in the Compose stack

This builds the API image from your checkout and runs it in the same stack as a [one-VM install](deploy/compose/README.md):

```bash
docker build -t velox:local .        # from the repo root
cd deploy/compose
cp .env.example .env
$EDITOR .env   # set POSTGRES_PASSWORD, VELOX_APP_DB_PASSWORD, VELOX_ENCRYPTION_KEY, VELOX_BOOTSTRAP_TOKEN
VELOX_IMAGE=velox:local docker compose up -d
```

**You see:** `docker compose logs -f velox-api` prints `"listening" addr=:8080` when the API is ready.

To build the image and run it on its own, against a Postgres you already have:

```bash
docker build -t velox:latest .
docker run --rm -e DATABASE_URL="..." -p 8080:8080 velox:latest
```

## Project Structure

Each billing domain (invoice, credit, customer, and so on) is a
self-contained package under `internal/`:

```
internal/{domain}/
  store.go       — Store interface (what the domain needs from persistence)
  postgres.go    — PostgreSQL implementation of the store
  service.go     — Business logic (validates, orchestrates)
  handler.go     — HTTP handlers (decodes request, calls service, writes response)
  *_test.go      — Unit tests (in-memory store) + integration tests (real Postgres)
```

**Rules:**
- Peer domain packages never call each other's concrete `Service`/`Store` types. Cross-domain imports are limited to shared value types, cross-cutting infra, and the narrow interfaces of the `billing` coordinator. The allowlist in `internal/arch/boundaries_test.go` enforces this. A new cross-domain import fails the test until it is added there deliberately
- Every handler uses `respond.JSON()` / `respond.FromError()` for responses
- Every store method runs inside a transaction scoped to one tenant by Postgres Row-Level Security (RLS)
- Tests use `testutil.SetupTestDB()` for integration tests

## Adding a New Domain

1. Create `internal/{domain}/store.go` with the store interface
2. Create `internal/{domain}/postgres.go` implementing the store
3. Create `internal/{domain}/service.go` with business logic
4. Create `internal/{domain}/handler.go` with HTTP handlers using `respond` package
5. Add SQL migration in `internal/platform/migrate/sql/`
6. Wire into `internal/api/router.go`
7. Write tests (both unit with in-memory store AND integration with Postgres)

## Code Style

- `go fmt` and `go vet` must pass
- No exported types without doc comments
- Errors use `errs.DomainError` for domain-specific errors or `fmt.Errorf` for validation
- JSON field names are `snake_case`
- ID format: `vlx_{type}_{random_hex}` (e.g., `vlx_cus_abc123`)

## Frontend (web-v2)

The dashboard is a React + TypeScript + Vite app. See
[`web-v2/README.md`](web-v2/README.md) for setup, the dev server, and the
directory's own CI checks. Two things often surprise first-time contributors:

- `npm install` relies on `web-v2/.npmrc`. That file deliberately tolerates a
  peer-dependency mismatch, so do not "fix" the mismatch.
- Typechecking is `npx tsc -b`. Do not use `tsc --noEmit`: here it checks
  nothing and reports no error.

## What CI runs on your PR

Every gate below runs on every PR. Run the ones relevant to your change
locally before you push, so a failing CI build holds no surprises:

| Gate | Local command | Notes |
|---|---|---|
| Unit tests + race detector | `go test ./... -race -short -count=1` | the `unit` job, with `go vet ./...`. Includes the source-rule tests in `internal/arch`, for example: a calendar date must be formatted in an explicit zone (`.In(loc)`), or carry a same-line `//tz:ok` |
| Integration tests | `go test -p 1 ./... -count=1 -short=false` | needs `docker compose up -d postgres`; test DBs are auto-provisioned |
| The same suite through a connection pooler | `scripts/pgbouncer-test.sh` | needs Docker; starts PgBouncer in transaction mode in front of the same Postgres |
| Money-invariant sweep | `go run ./cmd/velox-doctor`, with `DATABASE_URL` set to the owner URL of the database you ran the golden path against | runs in CI after the golden-path demo, over the rows it wrote |
| golangci-lint | `make lint` | config in `.golangci.yml`; much stricter than `go vet`. Also holds two Velox rules: no bare `time.Now()` in clock-pinned packages (waive with `//nolint:forbidigo // wall-clock: <reason>`), and no single-invoice period lookup in proration code (waive with `//nolint:forbidigo // single-invoice-ok: <reason>`) |
| Codegen drift | `make gen` then `git diff --exit-code` | required whenever `api/openapi.yaml` changes |
| Vulnerability scan | `govulncheck ./...` | can fail when a new Go CVE is published, even if your change is fine. Say so in the PR and we'll handle it |
| Frontend | `cd web-v2 && npm run build && npm test && npm run lint` | only if you touched `web-v2/` |
| Dashboard end to end | `cd web-v2 && npm run test:e2e` | Playwright against a running API and dashboard; see [How we test](docs/testing.md). Not a required check yet |
| Golden path | `make bootstrap`, then `make dev`, then `scripts/demo.sh <secret test key>` | CI boots a real server and walks recipe → invoice end to end; the key is printed by `make bootstrap` |
| Browser tests | `cd web-v2 && npm run test:e2e` | Playwright against a real API; setup in [`web-v2/e2e/README.md`](web-v2/e2e/README.md). Not a required check yet |
| Links between docs | `lychee --offline --include-fragments '**/*.md'` | internal links and anchors only |
| Docs style | `vale <changed .md files>` | advisory: it comments, it does not fail the build |

Fork PRs get the identical CI run, because no gate uses secrets.

## Pull Requests

1. Fork the repo and create a feature branch
2. Write tests for new functionality, at the layer [How we test](docs/testing.md) names for your change
3. Run the gates above that your change touches (at minimum the two test commands)
4. **Docs travel with the change, in the same PR:** any user-visible change
   gets a `CHANGELOG.md` entry, and any UI change is covered by a
   Playwright spec in `web-v2/e2e/`. This is a hard house rule — the bar
   is "the doc doesn't lie."
   For how to write docs, see the [writing style guide](docs/dev/writing-style.md).
5. Submit a PR with a clear description of what and why

**Touching money or a state machine?** This covers invoices, payments,
credits, dunning (failed-payment retries), subscriptions and tax. Follow the
[Money-Path Robustness Playbook](docs/dev/money-path-robustness-playbook.md):

- Before opening the PR, work through its implementation checklist (§3).
- In the PR description, include the state's site-set enumeration (§2). That
  is the list of every code site that can write or react to that state.
- A guard for concurrency or a money invariant needs an automated test. The
  test forces the race and is verified to fail once the guard is removed (a
  collision + mutation-verified test, §5).
