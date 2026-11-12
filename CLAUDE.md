# Hardcount: for AI coding sessions

Hardcount is an open-source usage-based billing engine in Go on PostgreSQL. This page adds what an AI session needs beyond [CONTRIBUTING](CONTRIBUTING.md).

**Before any change to money or billing state, read the [money-path playbook](docs/dev/money-path-robustness-playbook.md).**

## Sharing the repository

Several sessions may share this clone.

- **Work in your own git worktree,** under `.claude/worktrees/<task-name>`. The main working tree stays on `main`: do not edit, switch or stage anything in it.
- **Never `git stash`.** Every worktree shares one stash.
- **Do not run two integration runs (`-short=false`) at once.** They share one test database. If another session may be running one, use your own ([Tests on real Postgres](docs/testing.md#tests-on-real-postgres)).

  ```bash
  go test ./... -race -short -count=1         # unit tests
  go test -p 1 ./... -count=1 -short=false    # all tests
  ```

- **The dev servers default to ports 8080 (API) and 5173 (dashboard).** If another session holds them, run `make dev PORT=<free port>` (a `PORT` in your shell loses to `.env`). Start the dashboard with `HARDCOUNT_DEV_PORT=<free port>` and `HARDCOUNT_API_TARGET=http://localhost:<your API port>`.
- **Rebase onto `origin/main` before you push.**
- **End each commit with `Assisted-by: Claude Code`,** not `Co-Authored-By` ([What we accept](CONTRIBUTING.md#what-we-accept)).

## Claiming a number

Another session's unmerged branch may hold the next migration or ADR number.

- **Migration:** run `git fetch origin`, then `make migration name=...`. [Writing a database migration](docs/dev/migrations.md)
- **ADR** (a decision record): nothing checks its number. List `docs/adr/` on `origin/main` and every branch, then take the next free one.

## Where the rules live

| Before you | Read |
|---|---|
| Set up, test or open a pull request | [CONTRIBUTING](CONTRIBUTING.md) |
| Design a change | [How Hardcount is built](docs/ENGINEERING.md) |
| Import across packages, or open a transaction | [Architecture](docs/architecture.md#design-rules) |
| Write code, a comment or a test | [Code standard](docs/dev/code-standard.md), [Code comments](docs/dev/code-comments.md), [How we test](docs/testing.md) |
| Change the API | [OpenAPI workflow](docs/dev/openapi-workflow.md) |
| Write docs, PR text or dashboard text | [Writing style](docs/dev/writing-style.md), [Activity lines and UI words](docs/dev/activity-lines.md) |
| Meet an unknown word | [Product glossary](docs/glossary.md) for billing words, [contributor glossary](docs/dev/glossary.md) for code words |
