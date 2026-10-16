I've paused the catalog area as you asked. One correctness PR is open and a second branch is pushed as WIP; the rest of the catalog scope hasn't been started. Both branches are in one worktree, `/home/user/velox/.claude/worktrees/catalog`. It has no uncommitted changes, and I stopped my vite (5184) and API (8184).

| Branch | Base | Last commit | Pushed | PR |
|---|---|---|---|---|
| `ux/catalog-usage-totals` | `feat/app-shell` | `adfde2bf` | yes | **#1097** https://github.com/getvelox/velox/pull/1097 |
| `ux/catalog-usage-filter-export` | `ux/catalog-usage-totals` | `b03086d7` (WIP) | yes | none |

**#1097 — usage totals follow each meter's aggregation (M-USG-1, M-USG-2, Usage-page part of M-CUST-8)**
- **Server:** `GET /v1/usage-events/aggregate` rows now carry a value computed the way each meter bills: sum, event count, each customer's peak, or each customer's latest reading. Rows also get the event and customer counts, the highest single reading and a daily series. The endpoint takes a `tz` parameter; an unknown zone returns 422. The old `total` and `total_units` fields are kept.
- **Page:** "Total Units" is removed. Each meter has one row in its own unit, with a caption naming the number and a daily sparkline. On All time, Storage now reads 350 GB (Northwind 140 + Globex peak 210) instead of 550 GB.
- **Tests:** two new Go tests (one against real Postgres, one handler test for the bad-`tz` 422) and a new web test file. Three mutations each made a test fail. The light, dark and 390px screenshots are under `/tmp/claude-0/ux/build-catalog/usage-totals/`, and I looked at them.
- **Checks:** all pass except one integration test in `./internal/api`, `TestE2E_ReferencedIDsAreTenantAndModeScoped`. It fails because this sandbox can't resolve `example.com`; it's unrelated to the change, and the PR body says so.

**`ux/catalog-usage-filter-export` (WIP, M-USG-3/6)**
- **Done:**
  - The server export (`/v1/exports/usage-events.csv`) accepts the page's customer, meter and dimension filters; the filters are recorded in the export's audit row. A real-Postgres test covers this and failed when the dimension filter was dropped.
  - `make gen` was run.
  - Helpers for the export window and dimension-filter validation are tested; the old 50,000-row cap helper and its test are removed.
  - The Export button now downloads from the server. When a window is longer than 366 days (or All time), the file holds the last 366 days and a toast says so.
  - The dimension box waits 400ms after typing stops and shows an inline error instead of breaking the page. `tsc` passes and eslint shows no errors.
- **Not done:** screenshots, CHANGELOG and MANUAL_TEST (delete the "Export over 50,000 rows warns" box and rewrite the Export CSV box), the full check run, and the PR.

**Next steps to resume**
1. On `ux/catalog-usage-filter-export`: restart the API with `/tmp/claude-0/ux/build-catalog/restart-api.sh` and start vite on 5184 pointed at port 8184. Take before/after screenshots of the dimension error state and the export, update the docs, run `checks.sh`, replace the WIP commit with a proper one, push, and open the PR with base `ux/catalog-usage-totals`.
2. Remaining correctness PRs, each stacked on the one before:
   - M-CAT-5 (currency on Create plan)
   - M-CAT-7 (lock wording and an "affects N" server count)
   - M-PLAN-1/3 (subscription count by status, per seat, Quantity and MRR)
   - M-CAT-2/4 (one glossary)
   - M-PC-2/1 (provider costs per 1M tokens, delete confirm)
   - M-CRD-1 (credits columns)
   - M-PRC-1 (archive prices). The table already has a `lifecycle_state` column with `archived`, so no migration is needed.
3. The redesign PRs (catalog per C1, meter detail per C2, plan detail per C3, Usage page, Recipes). These should carry your new flow-simplification goal: one dialog for meter and price, "Create new price" in pickers, keys from names, smart currency defaults, bulk provider-cost entry, and "Flow: before N → after M steps" counts in each PR body.

Nothing else needs a product decision yet. The tooling is in `/tmp/claude-0/ux/build-catalog/`: `shoot.cjs` for screenshots, `checks.sh` for the full check run, `restart-api.sh` for the API on 8184, and `usage-totals.md` for the #1097 PR body.
