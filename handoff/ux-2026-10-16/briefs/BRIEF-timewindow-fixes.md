# Shared brief for fix agents (Velox time-window audit, 2026-10-16)

Repo clone: /home/user/velox (getvelox/velox, shallow, `origin/main` = 9c8327b). Read /home/user/velox/CLAUDE.md,
docs/dev/money-path-robustness-playbook.md (for any money/state change) and docs/dev/writing-style.md
(CHANGELOG + MANUAL_TEST shapes) before writing.

## Hard rules
- Fix ONLY the BUG items assigned to you. Do not change product behaviour beyond what the item says. If while
  working you find the fix needs a behaviour decision, STOP that item and report it instead of guessing.
- One PR = one complete change = one branch = one worktree:
  `cd /home/user/velox && git worktree add -b fix/<slug> .claude/worktrees/<slug> origin/main`
  Never edit /home/user/velox itself (it stays on main). Never push. Never merge.
- Each PR contains:
  1. a test that FAILS before the fix and passes after. Mutation-check it: revert just the fix, run the test,
     see it fail, restore. Record the failing output (1-3 lines) for the PR description.
  2. a CHANGELOG.md entry under `## [Unreleased]` → `### Fixed` (create the `### Fixed` subsection there if
     missing), dated `(2026-10-16)`, bold outcome first, plain words, ≤ ~50 words, per writing-style.md.
  3. MANUAL_TEST.md: add or revise a box under the flow that owns the behaviour, quoting the changed
     observable. Use `- [x] … auto: \`TestName\`` only if your automated test locks exactly that observable;
     otherwise `- [ ]` (pending; nobody has walked it — there is no browser here). If you change an existing
     box's assertion, downgrade `[~]` to `[ ]`. Keep the Flow index right if you add a flow.
- Checks before committing (all must pass; report output):
  - Go touched: `gofmt -l <files>` empty, `go vet ./<pkgs>/...`, `golangci-lint run ./<pkgs>/...`,
    `make lint-clock lint-tz lint-manual-test lint-funding-set` (whichever exist and apply),
    `go test ./... -short -count=1` (whole repo), and integration tests for touched packages:
    `flock /tmp/claude-0/pg.lock go test -p 1 -count=1 -short=false ./internal/<pkg>/...`
    (Postgres is running locally on :5432 with the repo's test roles; ALWAYS take the flock — other agents share it.)
  - `api/openapi.yaml` touched: `make gen` then `git status` must show only expected generated drift, committed.
  - web-v2 touched: `cd web-v2 && npx tsc -b && npx eslint <changed files> && npm test`. (node_modules: if the
    worktree has none, symlink: `ln -s /home/user/velox/web-v2/node_modules web-v2/node_modules` — do NOT commit
    the symlink; it's untracked, make sure `git status` doesn't stage it.) Dashboard tests are pure-function
    `node --test` tests in web-v2/tests/*.test.ts that import from src/lib; if the logic lives in a page/component,
    extract the minimal pure helper into src/lib and test that.
- Commit (one commit per branch is fine) with a conventional message like the repo's history
  (`fix(<area>): <plain outcome>`), body explaining what/why, ending with exactly:

  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu

- Write the PR description to
  /tmp/claude-0/-home-user-repo/60406105-c88f-5272-9e24-e1509e26ed6e/scratchpad/prs/<slug>.md
  following .github/PULL_REQUEST_TEMPLATE.md headings (What & why; Checklist ticked truthfully; Money path only
  section with the site-set from grep when it applies). Include: the scenario with dates, before/after output,
  the mutation-check evidence, check results, and for UI changes a "Screens to look at" list (exact page, state,
  what to see) since no browser is available. End the description with:

  🤖 Generated with [Claude Code](https://claude.com/claude-code)

  https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu

## Final report to me
Per branch: branch name, worktree path, commit sha, files changed, test name, mutation-check result, each check's
result (pass/fail, with output if fail), and anything you did NOT do or that turned out to need a product decision.
Be truthful: if a check failed or was skipped, say so.
