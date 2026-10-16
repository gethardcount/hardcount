## What & why

Second PR of the dashboard modernization, stacked on #1090 (base `feat/ui-foundation`). It fixes the app shell problems that four of the six audit reports hit independently.

| Before | After |
|---|---|
| Ctrl+K kept its own page list. It missed Recipes, Provider costs, Dunning policies and Test clocks, and called Usage "Usage Events". | Sidebar and Ctrl+K read one config (`lib/navigation.ts`). A test fails if a routed page is missing from it. |
| Sidebar sections Billing / Configuration / System mixed daily work (Credits, Credit Notes, Dunning) with setup. Two pairs of pages shared an icon. Labels mixed Title Case and sentence case. | Grouped by task: Home · Billing · Revenue · Catalog · Developers. Settings moves to the footer. Sentence case and one icon per page, both enforced by tests. |
| A floating "4 of 5 · Setup" pill sat over page content on every route: buttons, last table rows, and the subscription "Set thresholds" button, which it intercepted a click on. | Setup is a sidebar row; its checklist opens beside the sidebar. |
| Escape did not close the mobile menu. | Escape closes it. |
| Palette rows highlighted in solid primary, results used `--`, and invoices showed the amount due. | Quiet accent highlight, `·` separator, invoice total. |
| The page was titled "Dashboard". | "Home", matching the sidebar. |

## Evidence (Playwright, local build, seeded data)

- `/customers` at 1440, light and dark: new sections, sentence-case labels, Setup 4/5 and Settings in the footer, no floating pill.
- Setup row clicked: the checklist opens beside the sidebar.
- Ctrl+K, typed "test": the first result is "Test clocks · Simulate time in test mode".
- 390px: menu opened, then Escape; the drawer closes.

## Checklist

- [x] Tests cover the new behavior: `web-v2/tests/navigation.test.ts` (5 tests: every route is in the nav, test-only in test mode, sentence case + unique icons, detail highlighting, palette and sidebar both read the config)
- [ ] `go test` / `make lint` (no Go change)
- [x] `CHANGELOG.md` entry added
- [x] `MANUAL_TEST.md` flow added (FLOW U16, index updated)
- [x] Frontend: `npx tsc -b` ok, eslint 0 errors on changed files, `npm test` 242 pass, `npm run build` ok

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RjEwmyS5oYa5uTQxXiiymu
