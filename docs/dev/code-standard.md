# Code standard

How we write code and tooling in Hardcount. Each rule is in bold, with its
reason after it and a link to the guide it comes from.

- **Why we build this way:** [How Hardcount is engineered](../ENGINEERING.md)
- **Package layout:** [Architecture](../architecture.md)
- **Which test to write, and how to write it:** [How we test](../testing.md)
- **Comments and docs:** [Code comments](code-comments.md) for Go comments,
  and [Writing style](writing-style.md) for docs
- **Database migrations:** [Migrations](migrations.md)
- **Changes to money code** (invoices, payments, credits, dunning,
  subscriptions, tax): the [money-path playbook](money-path-robustness-playbook.md)
  applies on top of this page.

New and changed code follows this page. Old code is brought in line by small
cleanup PRs, not by one big rewrite. **lint:** names the linter that checks a
rule; `.golangci.yml` and `web-v2/eslint.config.js` show which ones CI runs
today. Before you push, run `make lint` and `make test-unit` (or `make test`
with Postgres running).

## The short version

1. Pass dependencies in. Keep environment reads and defaults in `internal/config`.
2. Never discard an error, and never guess a value that is missing.
3. Money is integer cents. Tenant data goes through a tenant transaction.
4. Keep each function, file and component about one thing.
5. Delete code, files and options that nothing uses.
6. Prefer the standard library and the tools we already have.

## Go code

Each domain package in `internal/` has a **handler** (HTTP), a **service**
(logic) and a **store** (SQL). Code is formatted with gofmt. **lint:** gofmt.

### Money, tenants and the API

- **Amounts are `int64` cents, in fields named `..._cents`.** Never use
  `float64` for money. Round with `internal/platform/money`, which rounds half
  to even so totals don't drift.
  ([Stripe: minor units](https://docs.stripe.com/currencies#minor-units))
- **Read and write tenant data in a tenant transaction:**
  `db.BeginTx(ctx, postgres.TxTenant, tenantID)`, so row-level security
  filters every query. `postgres.TxBypass` is only for the cases its
  allowlist test lists.
  ([AWS: Postgres row-level security](https://aws.amazon.com/blogs/database/multi-tenant-data-isolation-with-postgresql-row-level-security/))
- **API JSON fields are `snake_case`, and IDs carry a type prefix** such as
  `hc_cus_`.
- **Domain errors are `errs.DomainError` values** (from `internal/errs`), so
  `respond.FromError` can map them to the right status and error code.

### Wiring and config

- **A constructor takes every dependency it needs.** A nil field that means
  "skip this step" makes the code silently skip work. Use a `Set...` method
  only to break a real construction cycle.
  ([Google: global state](https://google.github.io/styleguide/go/best-practices#global-state))
- **Only `internal/config` and `cmd/*/main.go` read environment variables, and
  each setting's default lives there.** Each variable is parsed once into a
  typed field. Code that receives a setting adds no fallback of its own: a
  constructor that turns a zero limit into 100 hides a missing value. In
  compose files, write `${VAR:-}`, not `${VAR:-100}`, so the config's default
  applies. **lint:** forbidigo on `os.Getenv`.
  ([Fail fast](https://martinfowler.com/ieeeSoftware/failFast.pdf))
- **Production code has no branch, flag or constructor that exists only for
  tests.** Tests use the seams production uses: interfaces, and the clock in
  `internal/platform/clock`. A test-only path still ships, and can be
  switched on by mistake.
  ([xUnit Test Patterns: test logic in production](http://xunitpatterns.com/Test%20Logic%20in%20Production.html))
- **No package-level mutable variables** outside `main` and Prometheus
  metrics. A global hides who changes it, and lets tests affect each other.
  ([Google: global state](https://google.github.io/styleguide/go/best-practices#global-state))

### Errors

- **Never discard an error with `_ =`.** Return it. If a discard is truly
  safe, say why on the same line. **lint:** errcheck.
  ([Google: handle errors](https://google.github.io/styleguide/go/decisions#handle-errors))
- **Wrap errors with context:** `fmt.Errorf("load invoice %s: %w", id, err)`.
  ([Go blog: errors](https://go.dev/blog/go1.13-errors))
- **Return an error or log it, not both.** The caller that decides what to do
  next does the logging.
  ([Google: logging errors](https://google.github.io/styleguide/go/best-practices#logging-errors))
- **Compare errors with `errors.Is` and `errors.As`, never `==`.** `==` breaks
  silently the day a helper wraps the error. **lint:** errorlint.
  ([Go blog: errors](https://go.dev/blog/go1.13-errors))
- **A store returns `errs.ErrNotFound` for a missing row.** `sql.ErrNoRows`
  never leaves the store, so handlers check one thing.
  ([Go blog: errors](https://go.dev/blog/go1.13-errors))
- **After a `rows.Next()` loop, check `rows.Err()`, and always close rows.**
  Otherwise a dropped connection looks like a short result.
  **lint:** rowserrcheck, sqlclosecheck.
  ([database/sql](https://pkg.go.dev/database/sql#Rows.Next))

### Structure

- **Handlers stay thin:** decode the request, call the service, write the
  response. Transactions, money math and state changes live in the service,
  and SQL lives in the store.
  ([Fowler: service layer](https://martinfowler.com/eaaCatalog/serviceLayer.html),
  [ADR-002](../adr/002-per-domain-package-architecture.md))
- **Guard each state change at the write itself, with the list of states it
  may come from:** `UPDATE ... WHERE status = ANY($1)`, then check the row
  count. Zero rows means another writer got there first. A check read before
  the write can be stale by the time the write runs, and a deny-list
  (`status <> 'voided'`) silently allows states added later.
  ([Saltzer and Schroeder: fail-safe defaults](https://web.mit.edu/Saltzer/www/publications/protection/Basic.html),
  [money-path playbook](money-path-robustness-playbook.md))
- **Keep a function under about 200 lines and a file under about 3,000.**
  Split by named steps. Split money code only in a PR that changes nothing
  else. **lint:** funlen at 200 lines, new code only.
  ([Google: simplicity](https://google.github.io/styleguide/go/guide#simplicity))
- **One concept per file, named for what it holds:** `proration.go`,
  `handler_plans.go`. No `utils.go`, `helpers.go` or `misc.go`. Merge a file
  with one small function into its neighbour.
  ([Google: package size](https://google.github.io/styleguide/go/best-practices#package-size))
- **Each consumer declares the small interface it needs, and passes the
  concrete type in.** Don't write an adapter whose only job is to rename or
  forward a call.
  ([Google: interfaces](https://google.github.io/styleguide/go/best-practices#interface-ownership-and-visibility))
- **Delete code that no production path reaches.** A test calling it is not a
  reason to keep it. Find it with `deadcode ./cmd/...`.
  ([Go blog: deadcode](https://go.dev/blog/deadcode))

### Concurrency and logging

- **Work that outlives a request uses `context.WithoutCancel(ctx)` with a
  timeout,** not `context.Background()`, so it keeps the request id and tenant.
  ([context.WithoutCancel](https://pkg.go.dev/context#WithoutCancel))
  - A transaction whose follow-up steps must run is taken on such a ctx with
    `commitBudget` (30s), just before the transaction: pgx commits on the
    transaction's ctx, so a cancel during COMMIT can apply the commit and
    still return an error. Nothing else runs on that ctx.
  - Each group of steps after that commit gets its own such ctx with
    `stepsBudget` (5 minutes, above three Stripe calls at their limit),
    taken after the commit.
  - Each transaction takes its own `commitBudget` ctx, including one that
    records a failure after another ran out of time.
  - A unit that calls Stripe between its transactions (a refund: create,
    then issue with the Stripe call) takes one `stepsBudget` ctx before its
    first transaction. A compensating step after a failure (the refund's
    void) still takes its own `commitBudget` ctx.
  - Each budget is unexported in the package that uses it.
- **Every goroutine has an owner that waits for it at shutdown,** such as the
  `sync.WaitGroup` in `cmd/hardcount/main.go`. If nothing waits, a comment
  names the background job that retries the work.
  ([Go wiki: goroutine lifetimes](https://go.dev/wiki/CodeReviewComments#goroutine-lifetimes))
- **Log with the `slog` `...Context` functions when a context exists.** Our
  log handler adds `request_id` and `tenant_id` from it. **lint:** sloglint.
  ([log/slog](https://pkg.go.dev/log/slog#hdr-Contexts))
- **Never log secrets:** API keys, tokens, passwords or webhook signing keys.
  ([OWASP: logging](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html))

### Idioms and dependencies

- **Write current Go:** `slices`, `maps`, `min`/`max`, `for i := range n`.
  Run `go fix ./...` at each Go version bump, as its own PR.
  ([Go 1.26 notes](https://go.dev/doc/go1.26))
- **Name things the Go way:** short names, no stutter (`invoice.Service`, not
  `invoice.InvoiceService`).
  ([Effective Go: names](https://go.dev/doc/effective_go#names))
- **Reach for the language, then the standard library, then a module already
  in go.mod, then a new well-known module.** Write it yourself only when none
  fits, and say why in the PR.
  ([Google: least mechanism](https://google.github.io/styleguide/go/guide#least-mechanism))

## Tooling and CI

- **Delete files nothing uses:** scripts, Makefile targets, configs,
  generators and their output. Git keeps the history.
  ([SWE at Google, ch. 15](https://abseil.io/resources/swe-book/html/ch15.html))
- **Track Go tools with the `tool` line in go.mod** and run them with
  `go tool <name>`. ([Go 1.24 notes](https://go.dev/doc/go1.24))
- **CI fails when go.mod is not tidy:** `go mod tidy -diff`.
  ([Go 1.23 notes](https://go.dev/doc/go1.23))
- **Pin each tool's version in one file** that both CI and `make` read, so a
  local run matches CI.
  ([golangci-lint: CI install](https://golangci-lint.run/docs/welcome/install/ci/))
- **CI calls the same `make` target a developer runs,** so a check that passes
  locally passes in CI. Each check lives in one target and one CI job.
  ([Fowler: continuous integration](https://martinfowler.com/articles/continuousIntegration.html))
- **Define CI setup once,** and reuse a repeated block with a YAML anchor
  instead of pasting it.
  ([GitHub: YAML anchors](https://github.blog/changelog/2025-09-18-actions-yaml-anchors-and-non-public-workflow-templates/))
- **Generate code only with `make gen`.** CI reruns it and fails on any
  changed or new file, so generated code can't drift from its spec.
  ([sqlc: CI](https://docs.sqlc.dev/en/latest/howto/ci-cd.html))
- **A CI check either fails the build or is deleted.** Use
  `continue-on-error` only on a step that publishes a report, like the
  coverage upload, with a comment saying who reads it.
  ([SWE at Google, ch. 20](https://abseil.io/resources/swe-book/html/ch20.html))
- **Shell scripts pass ShellCheck, and workflows pass actionlint.** CI fails
  a ShellCheck warning in any tracked script (`scripts/ci/shellcheck.sh`). A
  script that grows past about 100 lines or holds real logic becomes Go.
  ([Google shell guide](https://google.github.io/styleguide/shellguide.html))
- **Pin third-party GitHub Actions to a full commit SHA,** with the release in
  a comment. Workflows default to `contents: read`, and every job has
  `timeout-minutes`.
  ([GitHub: hardening](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions))
- **Scan and sign an image before any public tag points at it.**
  ([Docker: test before push](https://docs.docker.com/build/ci/github-actions/test-before-push/))
- **Keep `.env` files out of build contexts:** `.dockerignore` patterns start
  with `**/` so they match at any depth.
  ([Docker: build context](https://docs.docker.com/build/concepts/context/))
- **Pin every image to a version tag,** and let Dependabot update compose
  files too.
  ([Docker best practices](https://docs.docker.com/build/building/best-practices/))
- **Turn on a new linter only after counting its findings.** Fix them in the
  same PR if they are few. If not, check only new code
  (`issues.new-from-merge-base`, a golangci-lint setting).
  ([golangci-lint config](https://golangci-lint.run/docs/configuration/file/))

## Frontend (web-v2)

- **New API calls use the client and types that orval generates from the
  OpenAPI spec.** Add nothing to `src/lib/api.ts`. When a PR changes how a
  page fetches data, move that page to orval.
  ([orval: React Query](https://orval.dev/docs/guides/react-query))
- **Define each query once,** as a `queryOptions` factory, and invalidate it
  through that factory's key.
  ([TanStack: query options](https://tanstack.com/query/latest/docs/eslint/prefer-query-options))
- **Server data lives only in the TanStack Query cache.** No fetching in
  `useEffect`, and no copies in state, context or module variables.
  ([TanStack: client state](https://tanstack.com/query/latest/docs/framework/react/guides/does-this-replace-client-state))
- **Never show a guessed value.** No `?? 'USD'` or `|| 'UTC'` on a required
  field, and no `catch` in a query function that returns an empty list. Show
  the loading or error state. A guessed currency or time zone shows, and can
  save, a wrong value with no error.
  ([PEP 20: refuse the temptation to guess](https://peps.python.org/pep-0020/))
- **Type every status with its generated union** and handle each value, so a
  new status fails `tsc -b` everywhere it's missing.
  ([TS: exhaustiveness](https://www.typescriptlang.org/docs/handbook/2/narrowing.html#exhaustiveness-checking))
- **Use an Effect only to sync with something outside React,** like a timer
  or the page title. Derive values during render.
  ([React: you might not need an Effect](https://react.dev/learn/you-might-not-need-an-effect))
- **Writes go through `useMutation`.** Money dialogs keep their idempotency
  key and their guard against a double click.
  ([TanStack: mutations](https://tanstack.com/query/latest/docs/framework/react/guides/mutations))
- **One concern per component.** A page is built from named sections and
  dialogs. ([Thinking in React](https://react.dev/learn/thinking-in-react))
- **Decision logic** (labels, dates, money, eligibility) **lives in pure
  functions in `src/lib`,** each with a Vitest test, and the component only
  shows the result.
  ([Fowler: modularizing React apps](https://martinfowler.com/articles/modularizing-react-apps.html))
- **Every request has a timeout.** Hand-written calls use `fetchWithTimeout`
  in `src/lib/api.ts`; the orval client already goes through it. Without one,
  a hung request leaves the page loading forever.
  ([AWS: timeouts and retries](https://builder.aws.com/content/3EumjoZascWd1oZiEgL8ORlv3qE/timeouts-retries-and-backoff-with-jitter))
- **`useMemo` and `useCallback` only for a measured cost,** or a stable
  identity something depends on.
  ([React: useMemo](https://react.dev/reference/react/useMemo))
- **Lint stays strict:** no `any`, no `@ts-ignore`, no suppressed hooks rules,
  and warnings fail CI. Delete unused exports instead of hiding them from
  knip (the unused-code checker). **lint:** eslint, knip.
  ([knip: handling issues](https://knip.dev/guides/handling-issues))

## What we don't adopt

Common practices that don't pay off here:

- **Renaming packages, or dropping `Get` from store methods:** hundreds of
  changes, no gain.
- **Strict style linters** (gocritic, revive, gochecknoglobals, wrapcheck, or
  funlen at its default limits): they flag hundreds of fine lines.
- **A CI gate on the modernize linter:** it grows new findings with every
  upgrade. We run `go fix` at each Go bump instead.
- **Digest-pinning every image, or reusable workflows:** more moving parts for
  the same result.
- **The React Compiler, or a client-state library:** no measured need.
- **Moving the whole dashboard onto orval in one PR:** it touches every money
  page at once. We move one page at a time.

Test practices we don't adopt are listed in [How we test](../testing.md#writing-tests).
