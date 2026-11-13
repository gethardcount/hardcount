# Contributing to Hardcount

Hardcount is a billing system you run yourself, written in Go on PostgreSQL. This page takes you from a fresh clone to a merged pull request.

Report a security problem privately ([SECURITY.md](SECURITY.md)), and ask questions in [GitHub Discussions](https://github.com/gethardcount/hardcount/discussions).

## What we accept

1. **Start with an issue.** For anything bigger than a typo or a broken link, open an issue or comment on an existing one, and wait. An issue is agreed when a maintainer says so on it.
2. **Some areas need an invitation.** A change here needs an agreed issue that says an outside pull request is welcome:
   - money and billing state (invoices, payments, credits, dunning, subscriptions, tax), which follow the [money-path playbook](docs/dev/money-path-robustness-playbook.md);
   - database migrations;
   - sign-in, sessions and API keys;
   - CI and release files (`.github/`);
   - dependencies (`go.mod`, `web-v2/package.json` and the lock files).
3. **Send one change. Run it yourself, and be able to explain every line.**
4. **AI help is fine.** End each commit an AI tool helped with in an `Assisted-by:` line, such as `Assisted-by: Claude Code`.

**Never accepted:** links that promote you or a product, batches of tiny edits made to collect contributions, and changes to the licence or the credits.

A pull request below this bar gets one short comment and is closed. You may open an issue and try again. Repeats, or behaviour the [code of conduct](CODE_OF_CONDUCT.md) rules out, lead to a block.

## Development setup

You need Go 1.27 or newer and Docker. Dashboard work and `make gen` also need Node 22 or newer, and the demo needs `jq`.

Fork the repository on GitHub, then:

```bash
git clone https://github.com/<your-user>/hardcount.git
cd hardcount
git remote add upstream https://github.com/gethardcount/hardcount.git
cp .env.example .env
```

1. **Start Postgres.**

   ```bash
   docker compose up -d postgres
   ```

   **You see:** `docker compose ps` lists `postgres` as `healthy`.

2. **Create the first account and its keys.**

   ```bash
   make bootstrap
   ```

   **You see:** a box with the sign-in email (`admin@hardcount.local`), a password shown only this once, and two API keys. Copy the key under "Secret Key — TEST mode" (`hc_secret_test_…`).

3. **Start the API.** `make dev` reads `.env` and applies new migrations.

   ```bash
   make dev
   ```

   **You see:** `curl http://localhost:8080/health` returns `{"status":"ok"}`.

4. **Start the dashboard** (only for UI work), in a second terminal.

   ```bash
   cd web-v2 && npm install && npm run dev
   ```

   **You see:** Vite prints `http://localhost:5173/`. Sign in there with step 2's email and password.

5. **Bill a test customer end to end** (optional), in a third terminal.

   ```bash
   ./scripts/demo.sh hc_secret_test_...   # the key from step 2
   ```

   **You see:** numbered steps, each with a green `✓`.

### Run the tests

```bash
go test ./... -race -short -count=1         # unit tests
go test -p 1 ./... -count=1 -short=false    # all tests
go test ./internal/invoice/ -run TestName -count=1 -short=false   # one test
```

Integration tests use the Postgres from step 1. They empty only their own `hardcount_test` database, which the container creates from `docker/init.sql` when its volume is first made. `go test` does not read `.env`: for another Postgres, set `TEST_ADMIN_DATABASE_URL` and `TEST_DATABASE_URL`.

To start over, or if `hardcount_test` is missing, `docker compose down -v` deletes the volume. Then repeat steps 1 and 2.

### Local services

`docker compose up -d postgres redis mailpit` starts everything the API can use.

- **Redis**, on port 6379, is optional outside production: without it, only sign-in (`/v1/auth`) is rate-limited.
- **Mailpit** catches every email, on SMTP port 1025 (set in `.env.example`). Read them at `http://localhost:8025`.

For a second account, run `make bootstrap` again with another `HARDCOUNT_BOOTSTRAP_EMAIL` ([Settings](docs/settings.md#first-account)). To run your change as a one-VM install does, see [Build from source](deploy/compose/README.md#build-from-source-instead).

## Frontend (web-v2)

The dashboard's setup and checks are in [`web-v2/README.md`](web-v2/README.md). Use npm 10, or 11.6.3 or later: 11.5.0 to 11.6.2 drops an `@emnapi` package from the lockfile, and CI's `npm ci` fails.

## Your first pull request

1. **Agree the change in an issue.** New here? Comment on one labelled [`good first issue`](https://github.com/gethardcount/hardcount/labels/good%20first%20issue). Then, in your fork, branch from `main`.
2. **Make the change, with its test.** First read [How Hardcount is built](docs/ENGINEERING.md), [Architecture](docs/architecture.md), the [code standard](docs/dev/code-standard.md) and [How we test](docs/testing.md). [Contributor docs](docs/dev/README.md) lists the rest, with two glossaries.
3. **Update the docs.** A change a user can see gets a `CHANGELOG.md` entry under `[Unreleased]`. The [writing style](docs/dev/writing-style.md) says how, and which other docs change.
4. **Open the pull request.** Title it like the commits on `main`: `fix(invoice): what is now true`. Other types: `feat`, `docs`, `test`, `refactor`, `chore`, `ui`, `ci`. The area is optional, and `!` marks a breaking change. The template is the checklist.
5. **Wait for review.** We answer within two days. CI on a fork's pull request starts when a maintainer approves the run. We squash-merge, and your change ships under the [MIT licence](LICENSE).

## What CI runs on your PR

CI runs these checks on every pull request. Run the ones your change touches before you push.

All but `upgrade`, `doc-links` and `doc-style` must pass to merge, on a branch up to date with `main`. If `main` moved, use **Update branch** or rebase. A red `upgrade` or `doc-links` does not block the merge. Fix one your change caused.

| CI job | Checks | Run locally |
|---|---|---|
| `unit` | Unit tests, with the race detector | `go test ./... -race -short -count=1` |
| `test` | Integration tests on real Postgres, and module checksums | `go test -p 1 ./... -count=1 -short=false` |
| `test-pgbouncer` | The same tests through PgBouncer, a connection pooler | `scripts/pgbouncer-test.sh` |
| `pricing-shapes` | Each common way to price, billed and checked to the cent | This job's `go test` lines in `.github/workflows/ci.yml` |
| `migrations` | A fresh install under each database role setup, a backup restore, and Postgres 15 and 18 | `scripts/ci/migrations-fresh-install.sh` |
| `golden-path` | The [first-invoice tutorial](docs/first-invoice.md) and the demo on a real server, then [hardcount-doctor](docs/dev/glossary.md) over the rows | Steps 2, 3 and 5 of [Development setup](#development-setup), then `DATABASE_URL=postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable go run ./cmd/hardcount-doctor` |
| `lint` | golangci-lint, ShellCheck, `govulncheck`, and checks on the Dockerfile, quickstart and nginx files | `make lint`, `scripts/ci/shellcheck.sh`, then `govulncheck ./...` |
| `codegen-drift` | The generated TypeScript matches `api/openapi.yaml` | `make gen && git diff --exit-code` |
| `frontend` | The dashboard's build, unit tests, eslint, and knip for unused code | `cd web-v2 && npm run build && npm test && npm run lint` |
| `e2e` | Playwright against a real API and dashboard | `cd web-v2 && npm run test:e2e` ([setup](web-v2/e2e/README.md)) |
| `upgrade` | The previous release bills a month, then your code starts on its database. Earlier invoices must read the same | `scripts/ci/upgrade-from-previous-tag.sh` |
| `doc-links` | Every internal Markdown link and `#anchor` resolves | `lychee --offline --include-fragments '**/*.md'` |
| `doc-style` | Vale, a prose linter, comments on added lines and never fails | `vale <changed .md files>` |

Install the tools. Build the two Go tools with the Go version in `go.mod`, because one built with an older Go refuses the module: `GOTOOLCHAIN=go<go.mod version> go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v<version>` (the `GOLANGCI_LINT_VERSION` in `.github/workflows/ci.yml`) and `GOTOOLCHAIN=go<go.mod version> go install golang.org/x/vuln/cmd/govulncheck@latest`. Then `brew install shellcheck lychee vale` or your package manager. Most scripts need Docker; each header lists what it needs.

`govulncheck` can fail on a new Go vulnerability your change did not cause: say so in the pull request.
