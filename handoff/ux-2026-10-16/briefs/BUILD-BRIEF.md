# Build brief: Velox dashboard modernization (2026-10-16)

You own one AREA of the dashboard. The maintainer has given full authority to redesign pages, remove/add/
update pages and fix functionality (server too) to make the product good. Work like a senior product engineer:
correct first, then clear, then beautiful. Read this whole brief, then /home/user/velox/CLAUDE.md,
docs/dev/writing-style.md, and (for any money/state change) docs/dev/money-path-robustness-playbook.md.

## Inputs
- Verified findings: /tmp/claude-0/ux/reports/merged-a.md and merged-b.md (ids M-…; class FIX-NOW / ISSUE /
  REDESIGN / DROP; "REFUTED" notes matter). Original area reports with wireframes (section C) and "keep" lists
  (section E): /tmp/claude-0/ux/reports/{customers,subscriptions,invoices,usage-catalog,shell,e2e}.md.
- Screens: /tmp/claude-0/ux/shots, /tmp/claude-0/ux/slices, auditors' work-* folders.
- Running stack: API http://localhost:8080 (test key /tmp/claude-0/run/test.key), Postgres on :5432 (dev db `velox`,
  test roles for `velox_test`). Main dashboard dev server http://127.0.0.1:5173 serves `main` — do not use it to
  judge your branch: start your own vite from your worktree on YOUR PORT (given in your task):
  `cd <worktree>/web-v2 && npx vite --host 127.0.0.1 --port <PORT> --strictPort` (background; kill by PID when done).
  Signed-in session for Playwright: /tmp/claude-0/ux/state.json. Playwright: require('/opt/node22/lib/node_modules/playwright').
  Screenshot helper pattern: /tmp/claude-0/ux/shoot-template.cjs (inject the `.h-screen/main` style before fullPage shots).
- New foundation (already on the base branch — USE IT, don't re-invent):
  - Tokens: `success|warning|info|danger` with `-foreground` and `-subtle` (e.g. `bg-danger-subtle text-danger-foreground`),
    `text-2xs`. No new raw palette classes (`emerald-600`, `amber-100` …) in code you touch.
  - `components/page/PageHeader` (back, title, badges, meta, description, actions; ONE primary action max),
    `MetaSep`, `Section` (title/description/actions/flush), `DetailLayout` (main + 320px right rail),
    `PropertyList` (label-over-value, copy), `KpiStrip`.
  - `lib/statusLabels` (`invoiceStatus`, `subscriptionStatus`, `subscriptionFlags`, `genericStatus`, `humanizeStatus`)
    + `components/StatusBadge`. Every status pill you touch goes through these.
  - Dialogs already scroll with a pinned footer; DateTimePicker wraps.
  - `lib/navigation.ts` drives sidebar + Ctrl+K (if you add/remove/rename a page, update it and its test).
  - If you need a change to a shared foundation file (components/ui/*, index.css, page/*, statusLabels, navigation),
    keep it small and additive, and say so in your report.

## Branching (stacked PRs)
- Base everything on `feat/app-shell` (it contains the foundation): 
  `cd /home/user/velox && git worktree add -b <branch> .claude/worktrees/<slug> feat/app-shell`
  then `ln -s /home/user/velox/web-v2/node_modules .claude/worktrees/<slug>/web-v2/node_modules` (never commit it).
- Order inside your area: first CORRECTNESS PRs (wrong numbers, broken flows, server fixes), each small and
  self-contained; then the REDESIGN PR(s) stacked on them. Each PR's base = the branch before it in your chain
  (first one's base = `feat/app-shell`). One PR = one complete change.
- If your task says to merge another branch first (a dependency), `git merge --no-ff origin/<that-branch>` into
  your first branch and state the dependency in that PR.
- Never edit /home/user/velox itself. Never merge to main. Never force-push others' branches.

## Every PR includes
1. Tests: Go unit/integration tests for server changes (real Postgres for store/money-path; take the lock:
   `flock /tmp/claude-0/pg.lock go test -p 1 -count=1 -short=false ./internal/<pkg>/...`); web logic in pure
   `src/lib` helpers tested in `web-v2/tests/*.test.ts` (node --test). Mutation-check each new test (revert the fix,
   see it fail, restore) and record the failing line.
2. Visual proof for UI: Playwright screenshots from YOUR vite port of before (main, port 5173) and after (your
   port), desktop 1440 light, dark, and 390px, for each changed screen and interactive state. LOOK at them (Read the
   PNG) — a UI change is not done until you have looked. Save under /tmp/claude-0/ux/build-<area>/<pr-slug>/.
3. CHANGELOG.md entry under `## [Unreleased]` (### Changed / ### Fixed / ### Added), dated (2026-10-16), plain
   words, bold outcome first, ≤ ~50 words. MANUAL_TEST.md: rewrite the boxes that describe changed behaviour (a box
   whose assertion changed goes to `[ ]`; `[x]` only with an automated test named); delete boxes that describe removed
   UI. Keep the Flow index right. `make lint-manual-test` must pass.
4. Checks (all must pass, report output): Go: gofmt, `go vet`, `/tmp/claude-0/gobin/golangci-lint run ./<pkgs>/...`,
   `make lint-clock lint-tz lint-funding-set lint-manual-test`, `go test ./... -short -count=1`, integration tests of
   touched packages (with the flock). OpenAPI change → `make gen`, commit generated files, no drift. Web:
   `npx tsc -b`, `npx eslint <changed files>` (0 errors), `npm test`, `npm run build`.
5. Commit message like the repo history (`feat(web): …` / `fix(invoice): …`), ending with exactly:

   Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
   Claude-Session: https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu

6. Push (`git push -u origin <branch>`) and open the PR via REST (GraphQL is blocked):
   `gh api repos/getvelox/velox/pulls -f title="…" -f head=<branch> -f base=<base-branch> -F body=@<file>`
   PR body follows .github/PULL_REQUEST_TEMPLATE.md headings, with: what & why (the finding ids it resolves), a
   before/after table, the evidence (screenshot descriptions — what you saw), test + mutation evidence, check results,
   the "Money path only" site-set for money/state changes, and ends with:

   🤖 Generated with [Claude Code](https://claude.com/claude-code)

   https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
   Save each PR body in /tmp/claude-0/ux/build-<area>/<pr-slug>.md too.

## Product rules
- Correct numbers beat pretty layouts. Never show a number you can't explain (label estimates as estimates; show
  the window/period a number covers; one currency per figure; never fabricate a seller name).
- One status vocabulary, one primary action per view, sentence case, plain words (docs/dev/writing-style.md).
- Keep what the auditors listed as good (section E of each report) unless you have a reason.
- Removing a page or an operator capability: allowed when it is truly redundant or folded elsewhere (e.g. a page
  becoming a tab); say exactly what moved where in the PR. If a removal would take away a capability operators rely
  on with no replacement, don't — report it instead.
- API changes: additive by default. A breaking change to a public API field needs a strong reason, a CHANGELOG
  "API:" note, and must be called out in your report.
- ISSUE-class items: do NOT do them unless your redesign touches that exact code anyway (then fix and list them as
  "resolved by redesign"). The maintainer files the rest as GitHub issues.

## Final report (your last message)
Per PR: number + URL, branch, base, title, finding ids resolved, files, tests + mutation result, checks, screenshots
looked at, anything not done or needing a product decision. Then: ISSUE-class items you resolved, ISSUE items still
open, new problems you found. Be truthful about failures and skipped checks.
