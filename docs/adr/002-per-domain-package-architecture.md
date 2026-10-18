# ADR-002: Per-Domain Package Architecture

**Date:** 2026-04-15
**Status:** Accepted

## Status
Accepted

## Date
2026-04-14

## Summary

Each business domain (invoice, credit, dunning, payment, pricing, subscription, usage, customer and the ones added later) is its own package under `internal/`, with its own store, service and HTTP handler. A domain never calls another domain's service or store, so a change in one domain cannot quietly break another. The `billing` engine coordinates domains through small interfaces it defines, and `api.NewServer` wires the adapters that satisfy them. `internal/arch/boundaries_test.go` enforces the rule: a new cross-domain import fails CI unless its allowlist entry gives a reason (shared infrastructure, shared value types, or the billing coordinator). Status: active.

## Context
Billing systems accumulate complex cross-cutting concerns: invoices reference subscriptions, subscriptions reference pricing, payments reference invoices and customers, dunning references payments and subscriptions. A naive approach — shared models or a single `billing` package — quickly becomes a dependency hairball where every change risks cascading breakage.

We needed an architecture that allows each domain to evolve independently while still enabling coordinated workflows like "generate invoice, apply credits, charge payment, start dunning on failure."

## Decision
Each domain (invoice, credit, dunning, payment, pricing, subscription, usage, customer) lives in its own package under `internal/`. Each package owns three layers:

- **Store interface** — defines the data access contract (e.g., `credit.Store`, `invoice.Store`)
- **Service** — business logic, depends only on its own Store interface
- **Handler** — HTTP layer, depends on its Service

**Zero cross-domain imports between peer packages.** The `billing.Engine` coordinates across domains by depending on narrow interfaces (`SubscriptionReader`, `UsageAggregator`, `PricingReader`, `InvoiceWriter`, `CreditApplier`) rather than importing peer packages. Wiring happens in `api.NewServer()` via adapter structs that satisfy these interfaces.

## Consequences

### Positive
- Each domain is independently testable — mock its Store interface and test the Service in isolation
- No circular dependency risk; the compiler enforces the boundary
- Adding a new domain (e.g., coupons, audit) requires zero changes to existing domains
- Narrow interfaces make the billing engine's dependencies explicit and auditable

### Negative
- Adapter boilerplate in `api/router.go` to bridge domains (e.g., `creditGrantAdapter`, `paymentRetrierAdapter`)
- Cross-domain queries (e.g., "invoice with customer name and dunning status") require joining at the handler or API layer rather than a single SQL join
- Developers must understand the wiring in `NewServer()` to trace a full request path

### Trade-offs
- More files and interfaces in exchange for strong isolation. This is the right trade for a billing system where incorrect cross-domain coupling can produce financial bugs that are hard to detect and expensive to fix.

## Alternatives Considered
- **Monolithic `billing` package**: Rejected. Grows unmanageable quickly; every domain change requires understanding the full package. Common failure mode in Go billing systems.
- **Shared domain models with separate services**: Rejected. A shared `models` package becomes a God package that every service imports, creating implicit coupling. Changes to shared types ripple everywhere.
- **Hexagonal architecture with ports/adapters**: Conceptually similar to what we do, but the full ports-and-adapters ceremony (separate `port` and `adapter` packages) adds indirection without proportional benefit at our current scale.
