## What & why

The customer page's **Activity** and **Margin** cards opened on a rolling "last 30 days". A billing period runs 28 to 31 days from the anchor day, so that window never matched the invoice shown beside it. After `scripts/demo.sh` (usage on day 1, test clock advanced 32 days), Margin read **$0.00 / "No usage in the window yet"** next to a **$3.88** invoice: the rolling window started two days after the usage.

This PR makes both cards open on the billing period, as decided in **ADR-121** (included):

| | Before | After |
|---|---|---|
| Activity default | 30d | **Cycle** (existing preset) |
| Margin presets | Last 7 / 30 / 90 days, Custom | **Current period** (default), **Last invoiced period**, Last 7 / 30 / 90 days, Custom |
| Margin window label | none | inclusive dates of the period, e.g. "Feb 28, 2026 – Mar 30, 2026" |
| Empty window | "No usage in the window yet" | "No usage in this window. Latest usage: Jan 31, 2026." + "Show from Jan 31, 2026" (Margin) / "View in Usage events" (Activity) |
| No active subscription (Margin, Current period) | n/a | "This customer has no current billing period: no active subscription." + "Show last 30 days" |

- **Current period** = the `period` that `GET /v1/customers/{id}/usage` returns with no params: the cycle of the subscription whose cycle started last. Activity's Cycle tab and Margin share that query (one request).
- **Last invoiced period** = the latest period end among the customer's invoices that are not draft or voided and have a subscription.
- **The API is unchanged.** The rolling presets and Custom stay.

### Scenario pass (summary; full analysis in ADR-121)

| Scenario | Behaviour |
|---|---|
| Demo (usage day 1, clock +32d) | Current period is the new, empty cycle → hint names Jan 31 → one click shows it; Last invoiced period covers it and matches the invoice's usage lines |
| Several subscriptions, different anchors | Current period is the latest-started subscription's cycle; the card prints the window so the figure is labelled. Per-subscription breakdown not built (risk listed in the ADR) |
| Calendar vs anniversary | Both are the subscription's own period, so both match their invoices |
| Mid-period plan change / cancel / trial | Margin revenue is still re-rated over the window (unchanged); differences vs invoice listed as a risk in the ADR, separate decision |
| Test clock | Periods come from the server on the customer's clock; rolling presets still anchor on frozen_time |
| No data / no subscription | Explicit messages with a one-click way out |
| Several currencies | Unchanged (margin omitted when currencies differ) |
| Tenant TZ | Period labels and the latest-usage range use the tenant zone |

### Peer evidence (read from source, 2026-10-16)

Orb customer costs default to the current billing period of the active subscription (`orb-python src/orb/resources/customers/costs.py:100-104`); Lago current usage is per subscription and always the current period (`lago-api app/services/invoices/customer_usage_service.rb:200-219`); Flexprice subscription usage defaults to the current period (`internal/ee/service/subscription.go:2800-2813`) and its margin view opens on the calendar month to date. None opens a per-customer view on a rolling 30 days.

### Tests and mutation check

`web-v2/tests/usageWindow.test.ts` (8 tests), including the demo scenario as a fixture. Each mutation below turned `npm test` red (1 failure each), and the suite is green with the code restored:

| Mutation | Result |
|---|---|
| Margin default back to `30d` | fail 1 |
| Activity default back to `last_30d` | fail 1 |
| Drafts counted as invoiced | fail 1 |
| Window end treated as inclusive | fail 1 |
| Last invoiced period resolved as rolling 30 days | fail 1 |
| Latest-usage range not clamped when usage is after "now" | fail 1 |

### Checks

- `npx tsc -b`: ok
- `npx eslint` on the 5 changed/new TS files: 0 errors, 0 warnings
- `npm test`: 241 pass, 0 fail
- `npm run build`: ok
- `make lint-manual-test`: ok
- No Go or OpenAPI change, so no Go checks or `make gen` needed.

### Screens to look at (no browser was available here)

1. **Customer page, demo customer** (`scripts/demo.sh`, open the printed customer link):
   - Activity opens on **Cycle**; under Billed usage $0.00: "No usage in this window. Latest usage: <day-1 date>. View in Usage events". Click the link → Usage events for this customer, custom range from that date to the clock's date, listing the 15 events.
   - Margin opens on **Current period**, the period line under the description, and the latest-usage line with the **Show from <date>** button. Click it → select shows Custom, both date pickers filled, non-zero totals.
   - Margin → **Last invoiced period** → period line equals the invoice's Period; Usage billed equals the sum of the invoice's usage lines.
2. **A live customer with steady usage**: Margin Current period shows the period line and non-zero totals; Activity Cycle header and Upcoming card cover the same period.
3. **A customer with no subscription**: Margin shows the no-billing-period message and "Show last 30 days".
4. **Narrow width (390px)**: the Margin header with the longer select labels and the period line wraps cleanly; the latest-usage line plus button wraps without overflow.

### Merge notes

- Conflicts expected with open PRs (all keep-both-sides): #1068 (MANUAL_TEST line on Activity/Margin presets and the Margin evidence line, both rewritten here too: keep this PR's wording), #1069 and #1071 (`CostDashboard.tsx`, different hunks), and CHANGELOG with every open PR.
- ADR number 121 checked against origin/main and all open PRs.

## Checklist

- [x] Tests cover the new behavior (unit; no store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes (no Go change; not run)
- [ ] `make lint` passes (no Go change; not run)
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added/updated (FLOW M1 boxes, the Activity box, and the clock box at TC; edited boxes downgraded to `[ ]`)
- [ ] `make gen` re-run (no `api/openapi.yaml` change)
- [x] Frontend touched? `npm run build && npm test` pass; eslint 0 errors on changed files (full `npm run lint` not run)

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
