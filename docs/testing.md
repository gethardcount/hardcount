# How we test Hardcount

Every change ships with a test ([why](ENGINEERING.md)).

## The four layers

| Layer | Tools | What belongs here |
|---|---|---|
| 1. Logic and money | `go test`: unit tests (`-short`) and integration tests on real Postgres | Money math, state changes, tenant isolation, migrations, API contracts, and the source tests in `internal/arch` (Go tests that read the code and fail on a banned pattern) |
| 2. Static checks | golangci-lint, eslint, `tsc -b`, generated code up to date (`make gen`), ShellCheck | Rules about how code is written |
| 3. Dashboard end to end | Playwright, in a real browser | A smoke test that opens every page, and the main user journeys, against a real API |
| 4. Live services | The [release checklist](dev/release-checklist.md), run by hand before each release | Real Stripe in test mode, real email, the LiteLLM usage callback and the invoice PDF |

Most tests belong in layer 1. Use layer 3 or 4 only when no Go test can prove the same thing.

## Which test to write

| You changed | Write |
|---|---|
| Money, or a billing state such as an invoice's status | A Go integration test against real Postgres. Test the race and the repeat, not only the happy path. The [money-path playbook](dev/money-path-robustness-playbook.md) says how |
| Pure logic with no database: a calculation, a parser, a formatter | A Go unit test. For dashboard code, a Vitest test in `web-v2/tests/` |
| An API endpoint | A Go test that calls the handler and checks the status and the body |
| A new field on an invoice or a line item in the API | List it in `scripts/ci/upgrade-new-fields.txt`, with why. The `upgrade` CI job fails until you do |
| A rule about how code may be written | A golangci-lint rule in `.golangci.yml`, or a source test in `internal/arch`. Not a script |
| A dashboard page or a user journey | A Playwright spec in `web-v2/e2e/`. A new page also gets a line in the `routes` list in `smoke.spec.ts` |
| Stripe, email or PDF behavior that a fake cannot prove | A line in the [release checklist](dev/release-checklist.md), and a Go test for the logic around it |

A Playwright test checks that the screen shows what the API returned. Arithmetic stays in Go tests.

## Writing tests

We follow the Go wiki's [test comments](https://go.dev/wiki/TestComments) and Playwright's [best practices](https://playwright.dev/docs/best-practices). The rules below are ours, and review checks them. Older tests do not all follow them yet: some sleep, match an error's text, insert rows with raw SQL or find elements by CSS selector.

### Every Go test

- A test must be able to fail. Assert a value, a row or an effect, not only "no error". For a fix, watch the new test fail without the fix.
- Build test data through production code: constructors, services, the API. Raw SQL is only for a state production cannot reach, with a comment that says why.
- Wait on a condition, never a sleep: a channel, a fake clock, or `testutil.WaitForLockWaiter` for a query blocked on a lock.
- Check an error with `errors.Is` or its error code, not its message text.
- Use plain Go assertions: no testify, no mock generators.
- Use the helpers in `internal/testutil`; do not copy them. Test a shared helper once, in its own package.
- Skip a test only when something it needs is missing. A test that fails at some hours needs a fake clock, not a skip.

### Tests on real Postgres

- `testutil.SetupTestDB` gives a test the real database, migrated and empty. It skips under `-short`, so add no skip of your own.
- `SetupTestDB` connects as a non-owner role set up like the app's (`hardcount_test_app`). So row-level security (RLS, the database rule that hides other tenants' rows) applies in the test as it does in production.
- Every call empties the tables. So run packages one at a time (`-p 1`), and never use `t.Parallel` with it.
- A second run at the same time needs its own database (`TEST_ADMIN_DATABASE_URL`, `TEST_DATABASE_URL`), made as `docker/init.sql` makes `hardcount_test`.
- Make a test's own Postgres role with `testutil.CreateRole`. A fixed name is shared by every database on the server.

### Test doubles

A test double is a hand-written stand-in for a real dependency.

- A double is for an outside service, such as Stripe or email, or for a peer domain behind the small interface its caller defines.
- A double never copies a rule that lives in our SQL: a `WHERE` clause, a lock, a unique index or a conditional update. A Go copy proves the copy, not the query. Test that rule on the real store.
- Assert the resulting state. Count calls only when the call is the effect you check, such as "one email was queued".
- Some packages have an in-memory copy of their own store (`memStore`). Do not write a new one, and add no SQL rule to an old one.

### Money and state tests

The [money-path playbook](dev/money-path-robustness-playbook.md) lists the tests a money change needs. Two more points:

- CI runs the race detector on the `-short` unit tests only. Run a Postgres concurrency test yourself, with `go test -race -short=false` on its package.
- When steps must run after a commit, test a request canceled during COMMIT. The commit can succeed while the caller gets `context.Canceled` ([code standard](dev/code-standard.md)). Copy `cutCommitInvoices` in `internal/billing/tax_retry_one_tx_integration_test.go`, which wraps the store method and cancels the request after the real call.

### Dashboard tests

Playwright specs follow the [e2e README's rules](../web-v2/e2e/README.md#rules): find elements by role or label, wait for the screen and not a timer, make your own data, and run a new or changed spec five times.

## Running tests

[What CI runs on your PR](../CONTRIBUTING.md#what-ci-runs-on-your-pr) lists every CI job with its local command. The [e2e README](../web-v2/e2e/README.md) covers Playwright, and the trace of a CI failure.

## Checking a change by hand

- **Look at the screen.** For a UI change, look at a screenshot in the state a user would see. Text and DOM checks cannot see a layout bug.
- **Check money at the provider.** Confirm a payment or refund amount in the Stripe dashboard, not only in Hardcount. Any Stripe test account works: the release checklist's [Setup](dev/release-checklist.md#setup) says how to connect one.
