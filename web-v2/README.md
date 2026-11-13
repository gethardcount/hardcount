# web-v2 — the Hardcount dashboard

React + TypeScript + Vite single-page app: the operator dashboard (customers,
invoices, subscriptions, pricing, dunning, webhooks) plus the customer-facing
public pages (hosted invoice, payment update, shared cost dashboard).

## Setup

```bash
npm install        # npm 10, or 11.6.3 or later (CONTRIBUTING.md, Frontend)
npm run dev        # Vite on :5173, proxying /v1 to the Go server on :8080
```

The dev server needs the backend running — from the repo root:
`make dev` (or the two commands in the root README's Quick start).

Node 22+ (CI runs 22; anything below 20.19 breaks Vite).

## The checks CI runs on this directory

```bash
npx tsc -b         # strict typecheck. NOT `tsc --noEmit` — that silently
                   # checks nothing here (composite project references).
npm run lint       # eslint, then knip: unused files, exports and
                   # dependencies fail (knip.jsonc says what is ignored)
npm test           # vitest
npm run build      # tsc -b + vite build — the strictest of the four
npm run test:e2e   # Playwright browser tests; needs a running API (e2e/README.md)
```

## Generated code — do not hand-edit

`src/lib/gen/**` is generated from `api/openapi.yaml` at the repo root.
Change the spec, then run `make gen` from the root (it runs `npm run gen`
here). CI's codegen-drift job fails any PR where the spec and the generated
artifacts disagree. `public/openapi.yaml` is a copy of the spec that
`npm run dev`, `npm run build` and `npm run gen` make first; it is not
committed.

## Conventions worth knowing before a first PR

- Relative-time / "now"-dependent UI must take a record's "now" from
  `useNowFor` or `useNowResolver` in `src/hooks/useNow.ts` — a record on a
  test clock (a simulated billing timeline) lives at the clock's time; an
  eslint rule bans bare `Date.now()` in the affected surfaces.
- Money is formatted through `src/lib/priceDisplay.ts` — exact string math,
  never `parseFloat` on money strings.
- Every user-visible change updates `CHANGELOG.md`, and UI changes are
  covered by a Playwright spec in `e2e/` (same PR — see
  [How we test](../docs/testing.md)).
