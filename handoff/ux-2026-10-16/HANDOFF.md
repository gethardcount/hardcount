# Handoff: time-window audit + dashboard modernization (paused 2026-10-16)

This folder carries everything from the cloud session that lived outside git: audit reports, verified findings, build
briefs, competitor evidence, PR bodies, per-team pause reports and helper scripts. Screenshots were not copied (≈300 MB);
regenerate them with `scripts/seed.sh` + `scripts/crawl.cjs` (see "Run it locally").

Paths inside the reports point at the cloud container (`/tmp/claude-0/...`). Locally, read them as:
`/tmp/claude-0/ux/reports/` → `reports/`, `/tmp/claude-0/ux/BUILD-BRIEF.md` → `briefs/BUILD-BRIEF.md`,
`/tmp/claude-0/ux/build-<area>/` → `build-notes/build-<area>/`. Screenshot paths (`shots/`, `slices/`, `work-*`) no
longer exist; re-run the crawl to get fresh ones.

## Where things stand

### Part 1 — time-window and date-boundary audit (done, PRs open, not merged)
- 21 bug-fix PRs: #1068–#1088 (all base `main`). Each has a mutation-checked test, CHANGELOG and MANUAL_TEST.
- #1089 (base `main`): Activity and Margin open on the billing period; Margin adds "Last invoiced period" (ADR-121).
  Walked in a browser; comment on the PR records it.
- Product decisions still open: listed in the session's final report (period display for mid-day cancels, UTC vs
  tenant-day buckets, "last 30 days" meanings, invoice date filter, `<` vs `<=`, past-due semantics, …).

### Part 2 — dashboard UX modernization (in progress, paused)
Audit: every page at 1440 / dark / 390, five area audits + an end-to-end flow walk, then two independent reviewers who
verified and merged ~190 findings into `reports/merged-a.md` and `reports/merged-b.md` (ids `M-…`, classes FIX-NOW /
ISSUE / REDESIGN / DROP). Competitor evidence from source (Lago, Orb, Metronome, Stripe spec, OpenMeter, Flexprice):
`reports/peer-evidence.md`.

PR stack (review/merge bottom-up):

```
main
├─ #1090 feat/ui-foundation        tokens, PageHeader/Section/DetailLayout/PropertyList/KpiStrip, status vocabulary, dialogs
│  └─ #1091 feat/app-shell         nav config (sidebar + Ctrl+K), setup in sidebar, Esc on mobile menu, Home rename
│     ├─ #1092 ux/customers-currency          one customer currency (includes the #1089 merge)
│     │  └─ #1095 ux/customers-margin-no-costs  no 100% margin without provider costs
│     │     └─ #1098 ux/customers-credits       credits granted/used/used-up on the customer page
│     ├─ #1093 ux/invoices-draft-seller        draft watermark; never invent a seller
│     │  └─ ux/invoices-hosted-page (WIP, pushed, no PR)
│     ├─ #1094 ux/admin-home-numbers           Home trends and counts correct
│     │  └─ ux/admin-email-settings (WIP, pushed, no PR)
│     ├─ #1096 ux/subs-preview-proration       preview prorates like the invoice
│     │  └─ ux/subs-dates-preview-card (WIP, pushed, no PR)
│     └─ #1097 ux/catalog-usage-totals         usage totals per meter aggregation
│        └─ ux/catalog-usage-filter-export (WIP, pushed, no PR)
```

What each team was doing and exactly how to resume: `pause-reports/<area>.md`. The plan per area (correctness PRs
first, then redesign per the wireframes in section C of each area report, plus "simplify flows" with before→after step
counts): `briefs/BUILD-BRIEF.md` + the per-area scope in the session transcript.

### Not done yet
- Remaining correctness PRs and all page redesigns in each area (see pause reports).
- Flow simplification counts (only Grant credits: 6 → 4 steps so far).
- Filing ISSUE-class findings as GitHub issues for contributors (wait until the area redesigns land, since many ISSUE
  items get fixed inside them; then file the rest from `reports/merged-*.md`).
- A summary report page for the team (optional).

## Run it locally (to reproduce the seeded app and screenshots)
1. `docker compose up -d postgres` (or any Postgres 16 with `docker/init.sql` applied).
2. Bootstrap: `VELOX_BOOTSTRAP_EMAIL=ops@acme.test VELOX_BOOTSTRAP_PASSWORD='Velox-walk-2026!' VELOX_BOOTSTRAP_TENANT="Acme AI" go run ./cmd/velox-bootstrap` — save the TEST secret key it prints.
3. API: `DATABASE_URL=postgres://velox:velox@localhost:5432/velox?sslmode=disable APP_ENV=local RUN_MIGRATIONS_ON_BOOT=true go run ./cmd/velox`
4. Dashboard: `cd web-v2 && npx vite --port 5173`
5. Seed: `bash scripts/demo.sh <test-key>` then `scripts/seed.sh` (edit its key path) — creates acme (demo, test clock),
   globex (3 months on a clock, 2 subs), northwind, initech (trial), umbrella (canceled), hooli (none), stark (annual +
   credits + manual invoice), wayne (EUR).
6. Screenshots: `scripts/crawl.cjs` (edit BASE/paths; needs Playwright) → every route at 1440 / dark / 390.
   Branch previews: run vite from a worktree on another port (`--port 5174`), point Playwright at it.

## Rules the work follows
CLAUDE.md (money-path playbook for any money/state change; worktree per task; docs in the same PR), BUILD-BRIEF.md
(tests + mutation check, screenshots looked at before a UI PR, CHANGELOG/MANUAL_TEST, one primary action per view,
sentence case, status vocabulary via `lib/statusLabels`, tokens instead of raw palette classes).
