# Escalation

How to handle an incident by severity. Back to the
[runbook index](index.md).

## SEV-1

Data leakage, a billing-correctness bug, or all customer charges failing.

1. **Contain the damage.** Revoke API keys or stop the API container;
   Velox has no read-only mode. Pause webhook delivery by deactivating
   endpoints (`PATCH` with `active=false`, which keeps the signing secret).
2. **Snapshot the database** for forensic review.
3. **Assemble responders:** backend lead, DBA, and the support lead if
   customers are affected.
4. **Communicate:** status page and notifications to affected customers.
5. **Postmortem** within 5 business days.

## SEV-2

A subset of customers affected, with bounded financial impact.

1. Find the affected scope with a database query.
2. Page the on-call engineer. Do not wait for the next business day on
   billing issues.
3. Patch, then correct past records (credit notes, manual reconciliation).
4. Postmortem.

## SEV-3

A small operator UX issue or an edge case.

- File an issue and schedule it for the next sprint.
