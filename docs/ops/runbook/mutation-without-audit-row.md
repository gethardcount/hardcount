# Mutation without an audit row

Every API route that changes data must write an audit row. This page is for
when one did not. Back to the [runbook index](index.md).

## Symptom

`velox_audit_uncovered_mutation_total{route}` shows any increase: a route
changed data and wrote no audit row.

## Why

The counter should be **flat zero**. Every route that changes data is
declared in `internal/api/audit_routes.go` as `explicit` (it writes an
audit row) or `exempt` (it does not need to). A non-zero counter means one
of three things, most likely first:

1. A route declared `explicit` has a code path that skips writing the row.
2. A 2xx path that really changes nothing (a cache or idempotency replay, a
   save with no changes) needs an `audit.MarkSkip` declaration.
3. A new route shipped without a declaration. CI blocks this (the
   route-walk test fails the build), unless someone edited the registry to
   silence it.

## Check

Find the route from the `route` label and the `UNCOVERED MUTATION` error
log.

## Fix

Fix the code path that skipped the audit row, or add the missing
declaration. Do not add an exemption without recording what is being given
up.

## Verify

The counter stops rising for that route.
