# UX audit brief (Velox dashboard, 2026-10-16)

Velox is an open-source usage-based billing engine. The operator dashboard is React (web-v2/src, Tailwind, shadcn-style
components in web-v2/src/components/ui, react-query). Repo: /home/user/velox (main, read-only for you: do NOT edit repo files).

## What you have
- The app is RUNNING with seeded data: http://127.0.0.1:5173 (dashboard, test mode), API http://localhost:8080.
  Signed-in browser session: /tmp/claude-0/ux/state.json. Playwright: require('/opt/node22/lib/node_modules/playwright').
  Template for interactive-state shots (dialogs, tabs, menus, hover, validation errors, empty filters):
  /tmp/claude-0/ux/shoot-template.cjs — copy it into /tmp/claude-0/ux/work-<your-area>/ and edit; save shots there too.
  Do not submit destructive actions (archive, cancel, void, delete, rotate). Opening dialogs and cancelling is fine.
  If you must create a record to see a state, name it with a "ux-" prefix.
- Screenshots of every route: /tmp/claude-0/ux/shots/<page>__<variant>.png (variants: desktop-light 1440w, desktop-dark,
  mobile-light 390w). Tall pages are sliced for legible viewing in /tmp/claude-0/ux/slices/<page>__<variant>__pN.png —
  READ THE SLICES (the Read tool shows images). Look at every slice for your pages, desktop, dark and mobile.
- Seeded customers: acme (demo, test clock advanced 32d, LLM token usage, margin data), globex (3 months history on a test
  clock, 2 subscriptions: Pro anniversary + Team seats calendar), northwind (live wall clock, Pro, usage today, a draft
  invoice), initech (14-day trial), umbrella (canceled), hooli (no subscription, no usage), stark (annual plan, $500
  credit grant, finalized manual invoice), wayne (EUR plan). No Stripe keys are configured (so no payments succeed).

## What to judge (for each page in your area, every state you can reach)
1. Function: can an operator finish the real task on this page (find X, understand a number, act on it)? Are numbers
   explained, consistent with numbers elsewhere, and in the right units/currency/timezone? Dead ends? Misleading copy?
2. Information hierarchy: what is the most important thing on this page, is it first and largest? Duplication? Empty
   cards taking space? Too long a scroll? Should it be tabs / a two-column layout / a summary header?
3. Interaction patterns: tables (sorting, filters, row click, pagination, bulk), dialogs/forms (validation, defaults,
   destructive confirms), empty/loading/error states, feedback toasts, keyboard, search.
4. Visual consistency: spacing, type scale, buttons (primary vs secondary usage — e.g. too many purple primaries),
   badges/status colors, card styles, alignment, icons, dark mode contrast, mobile layout (overflow, cramped tables).
5. Benchmark against modern SaaS dashboards (Stripe Dashboard, Linear, Vercel, Orb, Metronome, Lago, Resend, PostHog).
   Name the SPECIFIC pattern you'd borrow (e.g. "Stripe customer page: summary header + right rail of key details +
   tabbed body (Overview / Payments / Invoices / Events)"). Do not write generic "make it modern".

## Evidence standard
Every finding cites a screenshot file (and slice) and, where relevant, the code file:line that produces it. Only report
what you actually saw or read. If you inferred, say so.

## Output (your final message)
A) Per page: 2-4 sentence verdict.
B) Findings table rows: id (area prefix + number) | page | severity (broken / confusing / friction / polish) |
   category (function, hierarchy, interaction, visual, consistency, a11y, mobile, dark) | evidence (shot + code) |
   recommendation (concrete, with the borrowed pattern) | size: S (isolated, < ~1h, good first issue: one file, no design
   decision), M (a component or page section), L (page redesign / cross-cutting).
C) Proposed page anatomy for the 1-3 most important pages in your area: an ASCII wireframe of the redesigned layout
   (header, summary, tabs/sections, right rail, primary actions) with one line on why.
D) Cross-cutting patterns you noticed (shared components that should exist / be standardized).
E) What is already good and should be kept.
Be thorough: every page and state in your area. Be concrete and honest.
