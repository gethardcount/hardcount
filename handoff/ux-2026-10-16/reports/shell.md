## UX audit: app shell, Home and admin pages (plus design-system pass)

I went through every page in my area at desktop light, desktop dark and mobile, and captured 40+ interactive states. I edited no repo files.

The most serious problems:
- **The webhook event detail says something false.** It reads "the event is queued" for events that will never be delivered.
- **The Home page has small number errors.** The Failed payments trend is compared against a hard-coded 0, and draft invoices get the "pending" dot colour.
- **The shell has gaps.** Ctrl+K search can't find 4 pages, Escape doesn't close the mobile menu, and the login page ignores dark mode.
- **The design system has no status-colour tokens and no shared page header.** There are 348 hard-coded colour classes, and 25 pages copy the same heading markup by hand.

Evidence locations:
- Crawl slices: `/tmp/claude-0/ux/slices/`
- My state shots: `/tmp/claude-0/ux/work-shell/` (scripts `shoot.cjs` and `shoot2.cjs` are there too)
- Code paths are under `/home/user/velox/web-v2/src/`

One caveat about the crawl's mobile screenshots. Several are wider than 390px (webhooks is 1092px, audit-log 741px). That comes from the crawl's style override (`main{overflow:visible}`), not real page overflow. I measured live at 390px: the document width is 390 everywhere, but `main` on Settings is 430px wide (the tab strip overflows sideways). The real mobile overflow is limited to that and to tables that scroll inside their card.

---

### A) Verdict per page

- **App shell.** It is solid: Test/Live is on top, the test banner stays pinned, and the active nav item is marked. Its problems are labels and grouping:
  - Configuration mixes daily operations (Credits, Credit Notes, Dunning) with setup.
  - Two pairs of items share an icon (Pricing and Provider costs, Dunning and Dunning policies).
  - Capitalisation varies between labels.
  - Ctrl+K has its own copy of the nav list, which has fallen out of sync.
  - The floating "4 of 5 · Setup" pill covers page content on every route.
- **Home.** A useful first screen: the attention rows are good and every number links somewhere. But three notice rows come before any KPI, and the 220px revenue chart stays at full height when empty. The KPIs have small errors (hard-coded baseline, mislabelled sparkline, draft dot colour). The page answers "how much?" but not "what needs me today?".
- **Login / Forgot password.** Clean and centred, with an honest SMTP-off message. Weaknesses: errors appear as one banner rather than on the field, "Sign In" is title case, and both pages ignore the saved dark theme.
- **Settings.** The tabs are deep-linkable and the unsaved-changes save bar is good. Weaknesses:
  - The "Saved" pill always shows, even on a tenant that never saved anything.
  - The Payments tab header is styled differently from the other tabs.
  - There is no Email section, so an operator only learns SMTP is off from the Team tab's "Invites are off".
  - On mobile, the tab strip overflows sideways.
- **API keys.** Clear: the cards explain each key type and Revoke asks for confirmation. Weaknesses: the dialog defaults to Secret with No expiration, it never says whether the key will be Test or Live, and the copy uses "--" instead of real dashes.
- **Webhooks (Endpoints).** Hard to scan. Five event pills stack in one cell, the Description column is empty, and three buttons sit in every row. "paused" has no reason shown, and the Edit dialog calls the same thing an "Active" checkbox.
- **Webhook events.** The live tail and replay are strong. But it has no filters or search, the status shows as raw `no_endpoints`, and the expanded row says "the event is queued" when nothing will ever be delivered. Replay is offered on events that have no endpoint.
- **Audit log.** Strong content: day grouping, the simulated-time badge, server-side export, and expandable detail. Weaknesses:
  - The filters are native `<select>`s with no labels, sitting next to shadcn inputs of a different height.
  - With a filter that matches nothing, the empty state says "Actions will be recorded here automatically", which is wrong for that state.
  - The "View" links sit in a ragged column.
- **Not found.** Fine. It uses `text-lg` while every other page heading is `text-2xl`.

### B) Findings

Area prefix **SH**.

| id | page | severity | category | evidence | recommendation | size |
|---|---|---|---|---|---|---|
| SH-1 | Webhook events | broken | function | `work-shell/webhook-event-expanded.png`; `pages/WebhookEvents.tsx:370` | For `no_endpoints`, say "Not delivered: no active endpoint subscribes to this event" and link to Endpoints. Show "queued" only when status is pending. Stripe's event view does this: "No webhook endpoints". | S |
| SH-2 | Webhook events | confusing | function | `slices/webhook-events__desktop-light__p1.png`; `WebhookEvents.tsx:296,302` | Use human status labels ("No endpoint", "Delivered", "Failed (3 tries)"). Disable Replay when no endpoint subscribes, with a tooltip saying why. | S |
| SH-3 | Webhook events | friction | interaction | same shot (20 rows, no controls) | Add filters for event type, status and customer, plus search by event ID (Stripe Workbench "Events" filter bar). Put "Live" and "Paused" on the connection pill. | M |
| SH-4 | Home | confusing | function | `pages/Dashboard.tsx:237` (`previous={0}`) | Failed payments compares against a hard-coded 0, so the trend means nothing (0 → shows "– 0.0%"). Send `failed_payments_prev` from the API or hide the trend. | S |
| SH-5 | Home | confusing | function | `Dashboard.tsx:100-103,211` | The MRR card's sparkline is drawn from *revenue* data. Use MRR history or drop it. (This is in code only; the seeded data has no paid revenue, so it isn't visible.) | S |
| SH-6 | Home | confusing | visual | `slices/home__desktop-light__p1.png`; `Dashboard.tsx:332,403-408` | Drafts get the amber "pending" dot (`payment_status`) plus a "draft" badge. Use a neutral dot for drafts, or drop the dot because the badge already carries status. | S |
| SH-7 | Home | confusing | function | `slices/home__desktop-light__p1.png` ("Active customers 6 · 3 subs") | Inferred: the "active" count seems to include the canceled customer (umbrella) and the one with no subscription (hooli). Rename it "Customers" or count only paying customers, and add a tooltip with the definition. | S |
| SH-8 | Home | friction | hierarchy | `slices/home__desktop-light__p1.png` | Above the KPIs: a notice row plus two alert rows plus the header. The empty chart still takes 220px ("No paid USD invoices"). Collapse the empty chart to a 64px line. Merge the alerts into one "Needs attention" card in a right column (see the wireframe in C). | M |
| SH-9 | Shell (all pages) | friction | interaction | `slices/home__desktop-light__p1.png` (pill covers "View all →"), `slices/audit-log__desktop-light__p1.png` (covers a row); `components/OnboardingLauncher.tsx:44` | Move setup progress into the sidebar footer above the account row (Linear/Vercel "Getting started" item). Or, at minimum, reserve bottom padding in `main`. | M |
| SH-10 | Shell (Ctrl+K) | broken | function | `work-shell/palette-query-testclock.png`; `components/CommandPalette.tsx:35-49` vs `Layout.tsx:33-65` | The palette's nav list has no Recipes, Provider costs, Dunning policies or Test Clocks, and says "Usage Events" where the sidebar says "Usage". Build both from one shared `NAV` config. | S |
| SH-11 | Shell (Ctrl+K) | friction | interaction | `work-shell/palette-empty.png` | It only navigates and finds records. Add actions: Create customer, Create invoice, Switch to Live/Test, Toggle theme, Copy API key (Linear and Vercel ⌘K pattern). Use a quieter selected-row highlight (`bg-accent`, not solid primary; `CommandPalette.tsx:129`). | M |
| SH-12 | Shell (Ctrl+K) | polish | consistency | `palette-query-invoice2.png`, `palette-query-acme.png`; `CommandPalette.tsx:183,227`; `ApiKeys.tsx:222-223` | Replace "--" with "·" (middot) or "—" (em dash). | S |
| SH-13 | Shell (mobile drawer) | confusing | a11y/mobile | `work-shell/mobile-drawer-after-esc.png` (still open after Escape) | Close on Escape and trap focus: use the shadcn `Sheet`, or a keydown handler in `Layout.tsx:500-525`. Give the overlay `aria-hidden`. | S |
| SH-14 | Shell (mobile) | polish | mobile | `work-shell/mobile-drawer.png` (setup pill above the overlay); `OnboardingLauncher.tsx:44` z-30 = overlay z-30 | Put the launcher below the overlay or hide it while the drawer is open. Hide the "Ctrl+K" hint on touch devices. | S |
| SH-15 | Shell (nav) | friction | hierarchy | `slices/home__desktop-light__p1.png`; `Layout.tsx:33-58` | Regroup the nav (see C1). Give Provider costs and Dunning policies their own icons (they reuse `Tag` and `AlertTriangle`). Use sentence case throughout ("Credit Notes", "Audit Log", "API Keys" and "Test Clocks" become "Credit notes", "Audit log", "API keys", "Test clocks"). | S |
| SH-16 | Shell (theme) | confusing | dark | `work-shell/login-dark.png` (light while `velox-theme=dark`); `hooks/useDarkMode.ts` is only called in `Layout.tsx:233` | Apply the theme class from an inline script in `index.html` before first paint, for every route. Offer Light / Dark / System (Vercel account menu) instead of a two-state toggle. | S |
| SH-17 | Shell (account menu) | polish | hierarchy | `work-shell/account-menu.png` | The menu has 4 separators for 4 items. Group it as identity / preferences (theme) / help (Report issue, Docs) / Sign out. Show the tenant or workspace name: today only the email identifies the account. | S |
| SH-18 | Settings | confusing | function | `slices/settings__desktop-light__p1.png` (Business name empty, but "Saved" shows); `pages/Settings.tsx:357-361` | Show "Saved" only for a few seconds after a save (toast or inline "Saved ✓ 2s"). When nothing has changed, show nothing. | S |
| SH-19 | Settings | confusing | function | `work-shell/settings-team.png` (only hint is "Invites are off"); no email section in `Settings.tsx:366-370` | Add an "Email" tab showing SMTP status read-only (configured or not, from address, missing link env vars from the boot WARNs) and a "Send test email" button. CLAUDE.md treats email as a single delivery path; operators currently can't see it. | M |
| SH-20 | Settings / Payments | polish | consistency | `work-shell/settings-payments.png` vs `settings-invoicing.png` | Payments puts an icon header *outside* the card; every other tab puts the title inside. Use one `SettingsSection` component (title and description on the left, fields on the right: Stripe/Vercel settings two-column pattern). | S |
| SH-21 | Settings / Tax | confusing | function | `work-shell/settings-tax.png` (Manual selected, rate 0, "0 turns tax off") | When Manual is selected with 0%, show a warning: "Manual at 0% charges no tax; choose None to say so explicitly". Make "Seller country" a read-only row with an "Edit in General" link, not a box that looks like a disabled input. | S |
| SH-22 | Settings / Invoicing | polish | hierarchy | `work-shell/settings-invoicing.png` | Payment terms appear twice (chips plus a days input), and "Net 60" wraps onto a second line. Use one select with a custom days field. Move "Credit low-balance alert" to the Credits settings or a Notifications section. | S |
| SH-23 | Settings (mobile) | friction | mobile | `slices/settings__mobile-light__p1.png`; measured `main.scrollWidth=430` at 390px | The tab strip overflows. Use a scrollable tab strip with an edge fade, or a select menu below `sm`. | S |
| SH-24 | API keys | friction | function | `work-shell/apikey-create-dialog.png` | The dialog defaults to Secret + No expiration and doesn't say which mode the key is for. Add a "Creates a **test** key" line and default expiry to 90 days (Stripe restricted-key flow). | S |
| SH-25 | API keys | polish | hierarchy | `slices/api-keys__desktop-light__p1.png` | Each key takes a 160px card. Use a table instead (Name, Key, Type, Last used, Created, ⋯ menu), the Stripe API keys table, and put Rotate/Revoke in an overflow menu. | M |
| SH-26 | Webhooks | friction | hierarchy | `slices/webhooks__desktop-light__p1.png` | Replace the event pills with "5 events" plus a hover list. Drop the empty Description column. Make the row open the endpoint detail (the route `/webhooks/endpoints/:id` exists). Move Rotate/Edit/Delete into a ⋯ menu (Stripe Webhooks list). | M |
| SH-27 | Webhooks | confusing | function | `slices/webhooks__desktop-light__p1.png` ("paused"), `work-shell/webhook-edit-dialog.png` ("Active" checkbox); `Webhooks.tsx:218,595` | Use one word, "Disabled" or "Enabled", with a switch. Show *why* an endpoint is disabled (recipe placeholder, or auto-disabled after failures). Success rate "—" should read "No deliveries yet". | S |
| SH-28 | Webhooks dialog | polish | consistency | `webhook-add-validation-bad.png` ("must be a valid URL", lowercase) vs "URL is required"; the Add footer has a grey band, the Edit footer doesn't | Use sentence-case error copy and one `DialogFooter` style. | S |
| SH-29 | Audit log | friction | consistency/a11y | `slices/audit-log__desktop-light__p1.png`; `pages/AuditLog.tsx:392-455` | Native selects with no label and a different height from the inputs; no accessible names. Use the shared `Select`/`Combobox` with `aria-label`. Collapse into a filter bar ("+ Filter" chips, the Linear/PostHog pattern). | M |
| SH-30 | Audit log | confusing | function | `work-shell/audit-filter-empty.png`; `AuditLog.tsx:481` | When filters match nothing, say "No entries match these filters" with a "Clear filters" button. The total count also disappears; keep "0 of 49". | S |
| SH-31 | Audit log | polish | visual | `slices/audit-log__desktop-light__p1.png` (the "View" column zig-zags) | Fixed-width right columns: simulated time, actor, then link. Show resource type as a label ("Invoice", not "invoice"; `audit-row-expanded.png`). | S |
| SH-32 | Login / Forgot | polish | consistency | `work-shell/login-empty-submit.png`; `Login.tsx:141-143` | Show errors on the field (email/password) for missing values; keep the banner for bad credentials only. Use "Sign in" (sentence case). Make "Back to sign in" a secondary button (`ForgotPassword.tsx:113`, `text-xs` muted) so it's easy to see. | S |
| SH-33 | Home (mobile) | polish | mobile | `slices/home__mobile-light__p1.png` (VLX-000001 wraps; amount meets the badge) | Stack the row: number and status on line 1, amount and age on line 2. Make the period tabs a select. | S |
| SH-34 | Not found | polish | consistency | `slices/not-found__desktop-light__p1.png`; `NotFound.tsx:18` | Use the standard EmptyState size. Add "Search (⌘K)" as a second action. | S |
| SH-35 | Design system | confusing | consistency | 348 raw palette classes (161 amber, 90 emerald, 44 blue); `ui/badge.tsx` success/warning/info use raw `emerald-100` etc. | Add semantic tokens `--success/--warning/--info` (+ `-foreground`, `-subtle`) to `index.css`. Change Badge, Callout and AlertRow to use them; ban raw hues with a lint rule. | L |
| SH-36 | Design system | friction | consistency | 25 pages copy the `<h1 className="text-2xl font-semibold text-foreground">` markup by hand; subtitle top margin varies (`mt-0.5`, `mt-1`, `mt-2`, `mt-4`); only detail pages have `DetailBreadcrumb` | Add a `PageHeader` component (breadcrumb, title, description, meta, actions) and use it on all pages. | M |
| SH-37 | Design system | polish | consistency | 18 hand-built destructive buttons (`variant="outline" … text-destructive`, e.g. `Webhooks.tsx:262`), while `ui/button.tsx` destructive is a soft-red fill and the confirm dialogs use solid red | Three red treatments. Define two: `destructive` (solid, in confirm dialogs only) and `destructive-ghost` (in lists). | S |
| SH-38 | Design system | polish | visual | `ui/card.tsx` hard-codes hover shadow values; the `--shadow-card`/`--shadow-modal` tokens in `index.css` are unused | Every card lifts on hover, even ones you can't click. Remove the hover shadow from the base Card; add `<Card interactive>`; use the shadow tokens. | S |
| SH-39 | Design system | polish | consistency | `text-[10px]` ×20 and `text-[11px]` ×20; `text-[28px]` in TrendCard | Fold these into the type scale in D (`text-2xs` = 11px for micro labels; KPI = `text-3xl`). | S |

### C) Proposed page anatomy

**C1. Shell (Linear/Vercel collapsible sidebar plus a shared page header)**
```
┌──────────────┬──────────────────────────────────────────────────────────────┐
│ ◆ Acme ▾     │ ⚗ Test mode · test data, no real money        [Switch to Live]│ ← sticky
│ [Test|Live]  ├──────────────────────────────────────────────────────────────┤
│ 🔍 Search ⌘K │ Billing / Invoices / VLX-000006                               │ ← breadcrumb
│──────────────│ VLX-000006  [Finalized]                  [⋯] [Secondary][Primary]│ ← PageHeader
│ Home         │ Globex · $4,160.00 · due Nov 15                               │ ← meta line
│ Customers    │──────────────────────────────────────────────────────────────│
│ Subscriptions│  content (max-w 1600 for lists; 1200 for detail; 880 forms)  │
│ Invoices   2 │                                                              │
│ Usage        │                                                              │
│ ▸ Revenue ops│  (Credits, Credit notes, Dunning ②)                          │
│ ▸ Catalog    │  (Pricing, Recipes, Provider costs, Dunning policies)        │
│ ▸ Developers │  (API keys, Webhooks, Events, Audit log)                     │
│ ▸ Test tools │  (Test clocks — test mode only)                              │
│──────────────│                                                              │
│ ◔ Setup 4/5  │  ← replaces the floating pill                                │
│ ⚙ Settings   │                                                              │
│ (o) ops@… ▾  │                                                              │
└──────────────┘                                                              │
  [«] collapses to a 56px icon rail; tooltips (already wired: Layout.tsx:118) show in that mode
```
Why: grouping by operator task (operate / configure / develop) matches Stripe's "Developers" split. Moving setup into the sidebar stops it covering content. One PageHeader removes 25 hand-copied headings.

**C2. Home (Stripe Home "Today" plus Orb/Metronome revenue overview)**
```
Home                                    Updated 1m ago   [7d|30d|90d|12m]
⚗ Totals exclude 2 test-clock customers · Test clocks →          (one quiet line)
┌ MRR ──────────┬ Revenue ───────┬ Outstanding ───┬ Failed payments ┐
│ $1,049  ▲2%   │ $0  (0 paid)   │ $4,185 · 2 inv │ 0   vs 0 prev   │  ← KPI strip, 1 row
│ ARR $12,588   │ ▁▁▁▁▁          │ 1 overdue      │                 │
└───────────────┴────────────────┴────────────────┴─────────────────┘
┌ Revenue (USD) ──────────────────────────────┐ ┌ Needs attention ────────┐
│ chart (collapses to a one-line empty state) │ │ ⚠ 2 dunning runs     →  │
│                                             │ │ ○ 2 open invoices    →  │
└─────────────────────────────────────────────┘ │ ○ 1 draft to finalize →  │
┌ Recent invoices ────────────── View all → ──┐ │ ✓ Webhooks: 1 disabled → │
│ VLX-000006  Globex  Open  $4,160   7m       │ └─────────────────────────┘
│ ...  (adds a Customer column; no dot)       │ ┌ Recent activity (audit) ┐
└─────────────────────────────────────────────┘ └─────────────────────────┘
```
Why: what needs action goes in a right column, next to the trend rather than above it. "Outstanding" becomes a KPI, so that amount appears once instead of in a banner.

**C3. Settings (Stripe/Vercel settings pattern)**
```
Settings
┌ General      ┐  Business info                         ┌──────────────────┐
│ Invoicing    │  Shown on invoices and emails          │ fields …         │
│ Tax          │  ───────────────────────────────────── └──────────────────┘
│ Payments  ⚠  │  Address                                ┌──────────────────┐
│ Email     ⚠  │  "From" block on PDFs                   │ fields …         │
│ Team         │                                         └──────────────────┘
│ API keys     │  (left nav replaces tabs; ⚠ = needs setup; becomes a select on mobile)
│ Webhooks     │
└──────────────┘                     sticky save bar: ● Unsaved changes  [Discard][Save]
```
Why: a vertical sub-nav fits 7+ sections without the mobile overflow (SH-23). Status dots show which setup is unfinished (Payments, Email). Folding API keys and Webhooks in under Developers shortens the main nav.

### D) Design-system spec

**What exists now** (from `index.css` and `components/ui`):

- **Colour:**
  - Shadcn neutral tokens (oklch) are the base.
  - Primary is violet `oklch(0.55 0.28 285)` (dark `0.50`).
  - Chart semantic tokens exist (`--chart-success/warning/danger/info`), but only the charts use them.
  - There are no UI status tokens.
- **Radius:** the base is `--radius: 0.625rem`, with sm to 4xl derived from it. Each component uses a different step:
  - Card is `rounded-xl`; Button is `rounded-lg`; Badge is `rounded-4xl`.
  - On pages: `rounded-lg` ×57, `rounded-md` ×54, `rounded-xl` ×7.
- **Type:**
  - Body is 14px, Geist, line-height 1.6, letter-spacing -0.011em.
  - h1 is `text-2xl`, and is consistent on 25 pages. The exceptions are NotFound (`text-lg`) and HostedInvoice (`text-base`).
  - KPI values are `text-[28px]`; micro text is `text-[10px]` or `text-[11px]` (40 uses).
- **Spacing:** page padding is `p-4 md:p-8`. Gaps between sections are mostly `mt-6` (80), mixed with `mt-4` (28) and `mt-3`/`mt-5`/`mt-8`.
- **Button use:** pages use 0–4 primary buttons each (list pages 0–1). Purple primaries are not overused; the detail pages with 8–9 are mostly dialog submit buttons. The real inconsistency is destructive styling (SH-37).

**Proposed spec:**

- **Type scale (rem / line-height):**

  | Name | Size / line-height | Weight | Use |
  |---|---|---|---|
  | `display` | 30/36 | 600 | KPI value |
  | `title` | 24/32 | 600 | Page H1 |
  | `heading` | 16/24 | 600 | Section and card title |
  | `body` | 14/20 | 400 | Body text |
  | `label` | 13/18 | 500 | Form labels, table header |
  | `caption` | 12/16 | 400 | Muted text |
  | `micro` | 11/14 | 500 | Uppercase eyebrows and badges |

  Nothing goes below 11px.
- **Spacing scale:** steps of 4px: 4, 8, 12, 16, 24, 32, 48.
  - Page padding: 32 (desktop), 16 (mobile).
  - PageHeader to content: 24.
  - Between sections: 24.
  - Inside a card: 20.
  - Table cells: 12×16.
  - Form fields: 16 vertical gap.
- **Colour tokens to add:**
  - `--success`, `--warning`, `--info`, `--danger`, each with `-fg` and `-subtle` (bg) variants.
  - `--mode-test` (amber) and `--mode-live` (emerald).

  Badge, Callout, AlertRow, NoticeRow, status dots and the Test/Live toggle should all read these tokens.
- **Components:**
  - `PageHeader {breadcrumb?, title, description?, meta?, actions?: {primary?, secondary[], overflow[]}}`. Rule: at most one primary button per header.
  - `Section {title, description, actions?}`: a card, or a two-column settings row.
  - `Card` variants: `default` (no hover), `interactive` (hover lift and pointer), `stat` (KPI).
  - `DataTable`: sticky header, row click opens detail, ⋯ row menu, a filter bar, empty and filtered-empty states, numeric columns right-aligned with `tabular-nums`.
  - `StatusBadge {status}`: one map from status to tone and label, always human words, never snake_case (fixes SH-2 and SH-31).
  - Button variants: `primary`, `secondary` (outline), `ghost`, `destructive` (solid; confirm dialogs only), `destructive-ghost`. Sizes: sm (28), md (32), lg (36).
  - `FilterBar`: "+ Filter" chips with labelled selects, built from `Combobox`. Replaces the 13 native `<select>`s.
- **Page widths:** lists 1600 (existing `CONTENT_BAND`), detail 1200, forms and settings 880.

### E) Keep

- Test/Live segmented toggle at the top of the sidebar, with the optimistic slide and `inert` on the closed mobile drawer (`Layout.tsx:137-220,517`).
- Mode switch keeps `?tab=` and drops record IDs (`Layout.tsx:226-266`).
- The pinned test-mode banner, plus the separate red live-without-Stripe blocker.
- Sidebar count badges, which share tone with the Home attention rows.
- The "Test clocks not counted" honesty note, and Simulated badges with simulated times in the audit log.
- Plain SMTP-off messages on Forgot password and Team.
- Revoke confirm dialog copy, which names the key and what happens next.
- Settings unsaved-changes save bar with Discard; deep-linkable tabs.
- Audit log: day grouping, server-side CSV export that applies the current filters, expandable rows with copyable IDs.
- Webhook events live tail with per-attempt delivery history and replay.
- Ctrl+K server-side search across customers, invoices, plans and subscriptions.
- `CONTENT_BAND` width cap and the reasoning behind it.
