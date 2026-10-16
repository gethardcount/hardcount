## What & why

First PR of the dashboard modernization. A UX audit of every page (desktop, dark, 390px phone; five area reviews plus an end-to-end walk of every operator flow) found the same root gaps on every page. This PR adds the shared pieces the page redesigns build on, and fixes the cross-cutting defects that live in shared components.

**Shared pieces (used by the follow-up page PRs):**
- **Semantic status tokens** in `index.css` (`success`, `warning`, `info`, `danger`, each with solid / foreground / subtle, light and dark). `Badge` reads them. Today 348 raw palette classes (`emerald-700`, `amber-100`, …) stand in for status colours.
- **`text-2xs`** (11px): the floor for micro labels, replacing 40 `text-[10px]`/`text-[11px]` one-offs.
- **Page anatomy components** (`components/page/`):
  - `PageHeader`: back link, title with badges beside it, one meta line, actions.
  - `Section`: titled card.
  - `DetailLayout`: main column plus a 320px right rail that stacks below `lg`.
  - `PropertyList`: label-over-value facts with copy buttons.
  - `KpiStrip`: one row of headline numbers.

  25 pages hand-copy the page header today.
- **One status vocabulary** (`lib/statusLabels.ts` + `StatusBadge`). The same invoice read "Open" (tab), "finalized" (badge), "Showing finalized" (subtitle) and "pending" (payment badge). Invoices now fold lifecycle and payment into one Stripe-style badge: Draft, Open, Past due, Payment failed, Processing, Paid, Void, Uncollectible. Past due follows the entity's clock. Codes never show as snake_case.

**Fixes visible now:**

| Before | After |
|---|---|
| A dialog taller than the window cut off its buttons (New invoice at 900px; recipe configure). | Dialogs cap at the viewport, scroll inside, and pin the footer. |
| The date-time field (Extend trial, Advance clock) spilled past the dialog edge on a phone, and in Extend trial on desktop. | The time field wraps under the date. |
| Sign-in and reset pages rendered light for a dark-mode user. | The theme is applied before first paint on every operator route. Customer-facing pages (hosted invoice, payment pages, portal) stay light. |
| Every card lifted on hover, so static cards read as clickable. | Only `<Card interactive>` lifts. |

## Evidence (Playwright, local build, seeded data)

- New invoice dialog at 1440×700: body scrolls; Cancel / Save draft / Save & send stay pinned.
- Extend trial at 390px: the date and time fields both sit inside the dialog.
- `/login` with dark mode saved: renders dark, no flash.
- Invoices list light/dark: badges on the new tokens read correctly in both. The list still uses the old status words; it adopts `invoiceStatus` in its redesign PR.

## Checklist

- [x] Tests cover the new behavior: `web-v2/tests/statusLabels.test.ts` (4 tests: invoice folding, test-clock-aware Past due, subscription flags, sentence-case codes)
- [ ] `go test ./... -race -short -count=1` passes (no Go change)
- [ ] `make lint` passes (no Go change)
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added (FLOW U15, index updated)
- [ ] `make gen` re-run (no `api/openapi.yaml` change)
- [x] Frontend touched? `npx tsc -b` ok, eslint on changed files 0 errors, `npm test` 237 pass, `npm run build` ok

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
