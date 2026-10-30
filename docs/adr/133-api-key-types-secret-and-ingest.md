# ADR-133: API keys are secret or ingest; platform keys and /v1/tenants are gone

**Date:** 2026-10-30
**Status:** Accepted

## Summary

Hardcount stores two API key types. A secret key (`hc_secret_`) can do
everything in one account. An ingest key (`hc_ingest_`, "Usage only" in the
dashboard) holds one permission, `usage:ingest`, and can only send usage
events. The platform key type and the `/v1/tenants` routes it served are
removed: nothing could issue the first platform key, and the routes read
and created accounts across the whole server. Accounts are created only by
the bootstrap.

## Context

**The LiteLLM proxy held a full-access key.** The LiteLLM guide told
operators to put a secret key in `litellm_config.yaml`. A proxy config is
copied between machines, kept in a repo, and read by whoever runs the
proxy. A copy of that key could refund, void, read every customer and mint
new keys. The proxy only ever needs to send usage.

**Peers ship a key or role for ingest only.** From their code, read for the
competitive review of 2026-10-23:

- Flexprice enforces five roles, one of them `event_ingestor`
  (`config/rbac/roles.json`).
- Lago has three free roles over about 130 permissions
  (`config/permissions.yml`).
- Kill Bill uses Apache Shiro roles and permissions.

Stripe's restricted keys
([docs](https://docs.stripe.com/keys#limit-access)) answer the same need:
a key limited to what one integration does.

**The platform key could not exist.** `CreateKey` issued a platform key
only to a caller that already held one, and no route, tool or bootstrap
step made the first. `/v1/tenants` (create, list, get) answered only to a
platform key, so it served no one. It also reached across tenants: the
list read every account on the server. The pre-release reviews
(simplification audit 2026-10-23, right-thing review 2026-10-12) both
asked to cut it.

## Decision

1. **Key types.** `api_keys.key_type` is `secret` or `ingest`
   (`api_keys_key_type_check`, migration 00221). The prefixes are
   `hc_secret_{live,test}_` and `hc_ingest_{live,test}_`. The parser
   (`auth.StoredKeyTypes`) reads only these, so an `hc_platform_` key is an
   unknown key (401). `POST /v1/api-keys` takes `key_type` `secret`
   (the default) or `ingest`; any other value is a 422.
2. **One new permission.** `usage:ingest` (`auth.PermUsageIngest`) gates
   `POST /v1/usage-events`, `POST /v1/usage-events/batch` and
   `POST /v1/integrations/litellm/spend`. Secret keys and dashboard
   sessions hold it along with every other permission. An ingest key holds
   only it.
3. **What an ingest key reaches.** The three routes above, and
   `GET /v1/whoami`, which needs no permission for any credential. Every
   other route that takes an API key answers it with the ordinary
   missing-permission 403 (`insufficient permissions: this key type does
   not have <perm> access`). That includes backfill, the held-usage
   release and discard, provider costs and the usage reads, which need
   `usage:write` or `usage:read`.
4. **Platform removed.** `KeyTypePlatform`, `PermTenantRead`,
   `PermTenantWrite`, the `hc_platform_` prefix, the `/v1/tenants` routes,
   their spec paths, the tenant HTTP handler and `tenant.Service.Create`
   and `List` are deleted. Accounts are created by `tenant.RunBootstrap`,
   from `POST /v1/bootstrap` or `cmd/hardcount-bootstrap`, which calls it
   directly and needs no HTTP route.
5. **Migration 00221 refuses, never deletes.** If any row has
   `key_type = 'platform'`, the migration stops with the count and a
   runbook link, and changes nothing. Such a row was written by hand, and
   deleting a credential is the operator's call.

## Why this design

- **A permission, not a path list.** Routes already declare the permission
  they need. The ingest key is a permission set with one entry, so the
  usual gate refuses it, with the usual answer, on every route that names
  any other permission. A new route that forgets its permission fails
  `TestIngestKey_RefusedOnEveryRouteButUsage`, which walks the router.
- **Not `usage:write`.** That permission also backfills past usage,
  releases and discards held usage, and edits provider costs. A gateway
  needs none of those.
- **whoami stays open.** It needs no permission today, for any key. It
  tells an operator which account and mode a key belongs to, which is the
  first check after pasting a key into a proxy. It returns no customer
  data.
- **Remove, not hide, the platform key.** A credential type that cannot be
  issued is dead code on the auth path, and its routes are the only ones
  that cross tenants. Keeping them would mean testing and documenting a
  surface no one can reach.

## Alternatives considered

- **A. Per-resource scopes, like Stripe restricted keys:** a key carries a
  chosen list of permissions. Rejected for now: no user has asked for any
  scope but ingest, and a scope picker is a larger UI and API surface. The
  permission strings already exist, so this can come later without a new
  model.
- **B. Ingest key with `usage:write`:** no new permission. Rejected: see
  "Not `usage:write`" above.
- **C. Keep platform keys and add a bootstrap path that mints the first
  one:** rejected. One server with several accounts is served by running
  the bootstrap again (CONTRIBUTING, "Add a second account"). No user has
  asked to manage accounts over the API.

## Consequences

### Positive

- The LiteLLM guide gives the proxy a usage-only key. A leaked proxy config
  can add usage, but cannot move money, read customers or make keys.
- One fewer key type, three fewer routes, and no route that reads across
  tenants.

### Risks / open items

- A leaked ingest key can still send usage for any customer of its
  account. Usage is billed, so treat it as a secret, rotate it like any key
  and watch the Usage page.
- An install with a hand-made platform row stops at 00221 until the row is
  deleted ([runbook](../ops/runbook/migration-failed-midway.md#00221-found-platform-api-keys)).
- Roles for dashboard users are still recorded and not enforced (ADR-081).
  This ADR does not change that.

## References

- ADR-011 (dashboard sessions are separate from API keys), ADR-033 (the
  LiteLLM adapter), ADR-081 (no RBAC for members).
- Migration 00221 (`internal/platform/migrate/sql/00221_api_key_type_ingest.sql`).
- `internal/auth/permission.go`, `internal/api/router.go`,
  `internal/api/ingest_key_routes_test.go`.
- [docs/api-keys.md](../api-keys.md#key-types),
  [LiteLLM integration](../integrations/litellm.md).
