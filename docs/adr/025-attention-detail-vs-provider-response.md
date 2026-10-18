# ADR-025: Attention banner separates Velox's classification from upstream provider responses

**Status:** Accepted
**Date:** 2026-05-05

## Summary

An attention banner keeps two kinds of detail apart. `detail` is Velox's own explanation, and `provider_response` is text a provider such as Stripe actually returned. A check that fails before any provider call, such as `provider_not_configured`, leaves both empty, so the operator never mistakes Velox's text for a Stripe reply. The split is made from the typed `tax_error_code` when the response is built, and storage is unchanged. Status: active.

## Context

Operator-reported confusion on the invoice attention banner with the
typed code `tax.provider_not_configured`:

```
Tax calculation failed
Stripe Tax is selected in Settings → Tax but Stripe isn't connected
for this mode. Connect your Stripe keys for the active mode (test or
live) in Settings → Payments.

[Connect Stripe] [Retry tax]   tax.provider_not_configured

▼ Provider response
  no client configured for livemode=false
```

The disclosed string ("no client configured for livemode=false") is
**Velox's own internal classification message** — produced before any
HTTP call to Stripe. The label "Provider response" was misleading the
operator into thinking we got a 4xx back from Stripe; their reaction
was *"why are we hitting the provider when no creds are set?"* (we
weren't).

The root cause: `Attention.Detail` was a single field documented as
"raw provider payload" but populated from `inv.TaxPendingReason` —
which conflates two distinct semantics:

| Typed code | What `tax_pending_reason` actually carries |
|---|---|
| `provider_not_configured` (pre-flight, no API call) | Velox's own string |
| `provider_auth` (got 4xx from Stripe) | Upstream Stripe response body |
| `provider_outage` (got 5xx) | Upstream Stripe response body |
| `customer_data_invalid` (got 422) | Upstream Stripe response body |
| `unknown` | Either — depends on where the error was thrown |

The frontend rendered all of them under "Provider response" — accurate
for some, dishonest for others.

## Decision

Split `Attention.Detail` into **two fields with disjoint semantics**:

```go
type Attention struct {
    // ... existing ...

    // Detail: Velox's own classification context — always operator-
    // safe, always our framing. May be empty.
    Detail string

    // ProviderResponse: literal bytes from a third-party upstream
    // service (Stripe Tax envelope, Stripe last_payment_error body,
    // etc.). Populated ONLY when we actually called the provider and
    // received a response. Pre-flight classification errors leave
    // this empty.
    ProviderResponse string
}
```

### Routing rules

`classifyTaxAttention` uses the typed code to choose the slot:

| Code | Detail | ProviderResponse |
|---|---|---|
| `provider_not_configured` | empty | empty (no API call was made) |
| `provider_auth` | empty | upstream body (the 4xx) |
| `provider_outage` | empty | upstream body (the 5xx) |
| `customer_data_invalid` | empty | upstream body (the 422 with field detail) |
| `jurisdiction_unsupported` | empty | upstream body |
| `unknown` | empty | upstream body when present |

`classifyPaymentFailure`: `inv.LastPaymentError` is Stripe's
`last_payment_error` message — upstream payload — so it goes in
`ProviderResponse`. `Detail` left empty until we have Velox-side
context worth surfacing (e.g., "engine retried 3 times before giving
up", deferred until dunning surfaces it).

### Frontend

Two conditional disclosures, each rendering only when its field is
populated:

- **"Detail"** — Velox's own framing.
- **"Provider response"** — literal upstream payload.

Both empty (the new normal for `provider_not_configured`) → no
disclosure section at all. Headline + typed code + actions are the
whole UI.

## Consequences

- **Operator never has to guess provenance.** A "Provider response"
  label always means "this came back from the upstream service." A
  "Detail" label always means "Velox's own context."
- **`provider_not_configured` becomes self-explanatory.** The headline
  ("Stripe Tax is selected but Stripe isn't connected for this mode")
  + the action ("Connect Stripe") + the typed code stamp are
  sufficient. No internal string spilling into a misleading
  disclosure.
- **No schema migration.** The split happens at the API-formation
  layer using the typed `tax_error_code` as the discriminator;
  underlying `invoices.tax_pending_reason` storage is unchanged.
  Producers continue wrapping upstream payloads with `"stripe_tax: "`
  prefix; the classifier tells us whether the wrapped string is a
  real provider response or Velox-internal.
- **Extensible to other reasons.** The same split applies to
  `payment_failed` (Stripe's `last_payment_error` → ProviderResponse;
  Velox's "retried N times" context → Detail when surfaced),
  `payment_unconfirmed`, and any future attention reason that has
  both Velox-side and upstream-side context.

## Alternatives considered

- **Add `tax_provider_response` column** at the data layer.
  Cleaner storage but a migration that doesn't pay for itself when
  the API-formation split achieves the same wire shape. Reconsider if
  we ever need to query upstream-vs-internal distinctions in SQL
  (compliance reports, audits).
- **Single field with a `detail_source: "velox" | "provider"` flag.**
  Client switches on the flag to label the disclosure. Two fields is
  a cleaner API contract — no flag-handling on every render.
- **Suppress the disclosure for known pre-flight codes only.** A
  band-aid. Doesn't fix the conflation; new codes added later inherit
  the bug.
- **Just rename "Provider response" → "Detail" everywhere.** Loses
  the diagnostic clarity ("this came from Stripe vs Velox"); makes
  every banner uniformly vague.

## Tests

Per typed code, assertion that the right slot is populated:

- `provider_not_configured`: both fields empty.
- `provider_auth` / `provider_outage` / `customer_data_invalid` /
  `jurisdiction_unsupported` / `unknown`: ProviderResponse carries
  the wrapped upstream body, Detail empty.
- `payment_failed`: ProviderResponse carries `LastPaymentError`,
  Detail empty.

The pre-flight test (`TestClassifyInvoiceAttention_ProviderNotConfiguredEmptyResponse`)
is the canary — if a future change leaks Velox-internal strings into
ProviderResponse for pre-flight codes, the test fails loudly.
