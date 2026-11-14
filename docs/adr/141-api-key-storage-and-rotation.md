# ADR-141: An API key is a shape the database states, stored as a salted hash and changed in one transaction

**Date:** 2026-11-14
**Status:** Accepted

## Summary

A key is a type prefix (`hc_secret_live_`, `hc_secret_test_`,
`hc_ingest_live_` or `hc_ingest_test_`) and 64 lowercase hex characters. The
table stores its lookup prefix (the type prefix and 12 hex characters,
unique) and a SHA-256 hash of the key with a fresh 16-byte salt, never the
key. The database refuses a row of any other shape, and the server refuses a
string of any other shape before it looks anything up. One rule decides a
key's status (revoked, else expired at its `expires_at`, else active), and
only an active key authenticates or rotates. Creating, revoking and rotating
a key each commit in one transaction with the key's audit row; a rotation
locks the old key's row, and its grace never outlives the old key's own
expiry.

## Context

Before this decision, the rules were in several places and some disagreed:

- The shape lived only in Go. The table took any prefix, hash, salt or name,
  and the lookup index was not unique. A key with no mode was read as live.
- Two minters existed: the setup command stored `sha256(key)` with no salt;
  the API salted.
- Three places decided "usable": the lookup's SQL dropped revoked keys,
  `ValidateKey` refused after `expires_at`, and rotation refused at it.
- A key change committed first and wrote its audit row afterwards, ignoring
  the result. A rotation was three transactions, and could extend an
  expiring key by up to seven days.
- A key-issuing answer could be kept by the idempotency layer, so the raw
  key sat in `idempotency_keys` for a day.

## Decision

- **Shape.** `internal/auth/key.go` holds the format: the type prefixes, the
  parser (`lookupPrefix`), the minter (`mintKey`) and the hash
  (`hashKey`). Migrations 00230 and 00231 state the same shape as CHECKs
  (the prefix agrees with `key_type` and `livemode`; the hash is 64 hex;
  the salt is empty or 32 hex; the name is not blank) and make the lookup
  prefix unique. A test runs one table of prefixes through both the parser
  and the CHECK.
- **Storage.** The stored hash is `hex(SHA-256(salt || key))` with a fresh
  16-byte salt. A setup key written in v0.9.3 or earlier has an empty salt
  and validates through the same function.
- **One minter.** The setup command mints through `auth`
  (`CreateKeyTx`), so its keys have the same shape, salt and audit row.
- **Status.** `statusAt` is the one rule; every key the API returns carries
  `status`, and `ValidateKey` and `RotateKey` decide from it.
- **Changes.** Create, revoke and rotate each run on one tenant
  transaction that also writes the audit row (`LogInTx`); an audit failure
  rolls the change back. A rotation locks the old key's row first, so two
  rotations at once make one replacement. Its grace is `LEAST` of the
  old expiry and the grace end.
- **Secrets shown once.** Routes whose answer carries a secret shown once
  ignore `Idempotency-Key` (`x-show-once` in the spec), and the upgrade
  deleted any answer kept before.
- **Authentication.** `session.Authenticate` is the one function that turns
  a request's credentials into a caller; the event stream re-runs it every
  15 seconds and closes on a verdict.

## Why this design

The database and the server now agree on what a key is, and a reader finds
each rule in one place. A refusal happens at the cheapest point: a string
that is not a key never reaches the database. The audit trail cannot miss a
change, because the change and its row share a fate.

## Alternatives considered

- **Unsalted new keys.** A 256-bit random secret does not need a salt, and
  dropping it would save a few lines, but the normal path already salted and
  the docs promise a salted hash. Kept the salt.
- **Re-hashing older unsalted keys on use.** Puts a write on the request
  path for keys that already validate. Not done; rotating such a key gives
  it a salt.
- **A `replaced_by` column or a "rotated" status.** The audit row records
  the rotation (`old_key_id`), so the key table needs neither.
- **A limit on failed keys per client address.** No measured need yet; the
  rate-limit page says to put one in front of Hardcount.

## Consequences

- A hand-written key of another shape stops the upgrade at 00230 or 00231,
  with a count and a runbook section; nothing is deleted.
- A failed audit write answers `500` and leaves the key unchanged.
- A key used at exactly its `expires_at` is expired.
- Rotating a key whose name is over 100 characters keeps the name; only a
  new key's name is limited.

## References

- [ADR-011](011-email-password-auth-and-clean-api-keys.md): dashboard
  sessions are bound to a user, not to a key.
- [ADR-026](026-error-boundary-sanitization.md): one `401` wording, so an
  answer never says whether a key exists.
- [ADR-090](090-audit-in-tx-emission.md): audit rows on the change's
  transaction.
- [ADR-133](133-api-key-types-secret-and-ingest.md): the secret and
  usage-only key types.
- [Authentication and keys](../api/authentication.md),
  [API keys](../api-keys.md),
  [the runbook's 00230 and 00231 sections](../ops/runbook/migration-failed-midway.md).
