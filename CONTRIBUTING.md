# Contributing to Hardcount

Hardcount is a billing system you run yourself, written in Go on PostgreSQL. This page says how to take part, and how a change is built, tested and merged.

Report a security problem privately ([SECURITY.md](SECURITY.md)), and ask questions in [GitHub Discussions](https://github.com/gethardcount/hardcount/discussions).

## What we accept

**We are not accepting outside pull requests at this time.** One maintainer reviews every change to a system that bills people, so the code comes from maintainers and invited collaborators. On GitHub, only a collaborator can open a pull request here.

You can still take part, and you are credited for it:

1. **Report a bug.** Open an issue with the steps that show it. Bugs in money and billing state are looked at first.
2. **Propose a fix.** In the issue, describe the change, paste a patch, or link a branch on your fork. A maintainer writes the change and uses your code where it fits. Code you post ships under the [MIT licence](LICENSE).
3. **Say where you got lost.** A doc that misled you or a design you could not follow is worth an issue.

**How you are credited.** A commit that uses your code names you in a `Co-authored-by:` line, so GitHub lists you as one of its authors. The `CHANGELOG.md` entry names who reported the bug or proposed the fix.

**Becoming a collaborator.** A maintainer may invite someone whose issues and patches show they know the codebase. A collaborator opens pull requests, and the rest of this page is for them.

**Not accepted:** issues that promote you or a product, and issues sent in bulk by an account that does not use Hardcount. They are closed. Repeats, or behaviour the [code of conduct](CODE_OF_CONDUCT.md) rules out, lead to a block.

**Rules for every change:**

- **Send one change. Run it yourself, and be able to explain every line.**
- **Money and billing state** (invoices, payments, credits, dunning, subscriptions, tax) follow the [money-path playbook](docs/dev/money-path-robustness-playbook.md).
- **AI help is fine.** End each commit an AI tool helped with in an `Assisted-by:` line, such as `Assisted-by: Claude Code`.

## Development setup

You need Go 1.27 or newer and Docker. Dashboard work and `make gen` also need Node 22 or newer, and the demo needs `jq`.

Clone the repository, or your fork of it:

```bash
git clone https://github.com/gethardcount/hardcount.git
cd hardcount
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

## Opening a pull request

1. **Branch from `main`.** A change that fixes an issue names it in the pull request.
2. **Make the change, with its test.** First read [How Hardcount is built](docs/ENGINEERING.md), [Architecture](docs/architecture.md), the [code standard](docs/dev/code-standard.md) and [How we test](docs/testing.md). [Contributor docs](docs/dev/README.md) lists the rest, with two glossaries.
3. **Update the docs.** A change a user can see gets a `CHANGELOG.md` entry under `[Unreleased]`. The [writing style](docs/dev/writing-style.md) says how, and which other docs change.
4. **Open the pull request.** Title it like the commits on `main`: `fix(invoice): what is now true`. Other types: `feat`, `docs`, `test`, `refactor`, `chore`, `ui`, `ci`. The area is optional, and `!` marks a breaking change. The template is the checklist.
5. **Wait for CI and review.** We squash-merge, and the change ships under the [MIT licence](LICENSE).

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
