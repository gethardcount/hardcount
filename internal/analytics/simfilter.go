package analytics

// Simulated-data exclusion fragments for analytics aggregates (ADR-086).
//
// Analytics metrics aggregate rows onto a shared WALL-CLOCK time axis (revenue
// over dates, MRR, period counts). Test-clock SIMULATED rows live on a frozen
// axis — their timestamps are stamped at the clock's frozen_time — so summing
// them into a wall-clock aggregate is temporally incoherent (inflated counts,
// wrong time buckets). Every analytics aggregate therefore EXCLUDES simulated
// rows. In live mode these fragments are a no-op (no simulated rows exist there);
// they only bite in test mode, where wall-clock-test and simulated data share
// livemode=false.
//
// The discriminator differs per table — there is no uniform column:
//   - invoices / credit_notes  → durable boolean is_simulated.
//   - customers / subscriptions → test_clock_id (NULL on live/wall-clock rows).
//   - usage_events / customer_credit_ledger / invoice_dunning_runs → neither;
//     they filter through the owning customer / invoice.
//
// Each is an AND-fragment appended to an existing WHERE clause (the
// customer_credit_ledger balance, which has no WHERE, adds its own).
//
// Completeness is guarded behaviorally, not by a static grep: the real-Postgres
// test TestOverview_ExcludesSimulatedData asserts that adding a full simulated
// customer graph moves NO overview number — so a new aggregate that forgets a
// fragment fails CI. (A static "does this SELECT exclude simulated?" check is
// undecidable; the behavioral invariant is not.)
//
// Scope note: per-row DISPLAY surfaces (customer/invoice lists, detail pages)
// deliberately do NOT use these — they correctly SHOW simulated rows, badged.
// Only shared-axis AGGREGATES exclude.
const (
	notSimCustomer    = ` AND test_clock_id IS NULL`                                                 // customers.test_clock_id
	notSimSubBare     = ` AND test_clock_id IS NULL`                                                 // subscriptions, unaliased (helpers inline the alias-s form)
	notSimInvoice     = ` AND is_simulated = false`                                                  // invoices.is_simulated
	notSimViaCustomer = ` AND customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NULL)` // usage_events / ledger
	notSimViaInvoice  = ` AND invoice_id IN (SELECT id FROM invoices WHERE is_simulated = false)`    // invoice_dunning_runs
)
