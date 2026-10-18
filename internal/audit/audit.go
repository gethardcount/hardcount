package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

var auditWriteErrors *prometheus.CounterVec

// Audit-write failure outcomes. The counter used to carry NO outcome label at all,
// so an on-call engineer paging on velox_audit_write_errors_total (docs/ops/runbook/audit-write-errors.md:
// "rate > 0/s — SOC 2 evidence at risk") could not tell from metrics whether:
//
//   - evidence was PERMANENTLY LOST (a post-commit own-tx write failed: the mutation
//     is durable, the row is not, and nothing will ever retry it), or
//   - a mutation was REFUSED (an in-tx write failed, so shared fate rolled the
//     business change back — no evidence is missing, because nothing happened).
//
// Those demand opposite responses: the first is a compliance incident and the
// evidence cannot be reconstructed; the second is a availability blip that fixed
// itself correctly. One number could not distinguish them.
const (
	// OutcomeRowLost: the mutation COMMITTED and its audit row did not. Irrecoverable.
	OutcomeRowLost = "row_lost"
	// OutcomeMutationRefused: the audit write failed INSIDE the business tx, so the
	// mutation rolled back with it (ADR-090 shared fate). Nothing to reconstruct.
	OutcomeMutationRefused = "mutation_refused"
)

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_audit_write_errors_total",
		// NO tenant_id label. Prometheus client counters never age out, so a
		// raw-tenant label mints a permanent time series per tenant that ever has a
		// single failure — unbounded cardinality that grows with the customer base
		// and survives the incident that caused it. One shared-DB blip during a
		// busy hour could mint thousands. The TENANT belongs in the ERROR LOG (which
		// is queryable and expires); the METRIC answers "is evidence at risk, and in
		// which direction".
		Help: "Failed audit log writes by outcome: row_lost (mutation committed, evidence did not — irrecoverable) or mutation_refused (in-tx write failed, business change rolled back with it).",
	}, []string{"outcome"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			auditWriteErrors = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		auditWriteErrors = c
	}
}

// Logger appends entries to the audit log. Writes are synchronous so
// callers know whether the entry was persisted — important for compliance.
type Logger struct {
	db  *postgres.DB
	enc *crypto.Encryptor
}

func NewLogger(db *postgres.DB) *Logger {
	return &Logger{db: db}
}

// SetEncryptor lets the READ path decrypt actor_name.
//
// actor_name is resolved at read time by LEFT JOIN — an api_keys name, a
// customers display_name, or a users email. customers.display_name is ENCRYPTED
// AT REST (customer.PostgresStore.SetEncryptor), and audit_log has no encryptor,
// so with VELOX_ENCRYPTION_KEY set — i.e. in production — every customer-actor
// row (a hosted-invoice Pay click, a payment-update link) rendered its Actor as
// CIPHERTEXT: on the dashboard, and in the CSV handed to an auditor.
//
// Decrypting here is safe for all three sources: Encryptor.Decrypt passes a
// value without the encryption prefix straight through, so api-key names and
// user emails (plaintext) are untouched. Nil-safe: no key, no decryption, and
// unencrypted deployments behave exactly as before.
//
// The name is NOT stored in audit_log — it is joined from the live row — so this
// stays consistent with the append-only + GDPR rule: erase the customer and the
// name disappears from every historical row.
func (l *Logger) SetEncryptor(enc *crypto.Encryptor) { l.enc = enc }

// decryptActorName is the single point both readers (Query and the CSV Stream)
// pass through. A decrypt failure is NOT fatal: the row is still evidence, and a
// key rotation must not blank an auditor's whole log — the ciphertext is left in
// place, which is visibly wrong rather than silently absent.
func (l *Logger) decryptActorName(e *domain.AuditEntry) {
	if l.enc == nil || e.ActorName == "" {
		return
	}
	if plain, err := l.enc.Decrypt(e.ActorName); err == nil {
		e.ActorName = plain
	}
}

// Log records an audit entry synchronously and returns an error if the
// write fails. Callers may ignore the error when audit failure should
// not block the business operation, but failures are always logged and
// counted in metrics.
//
// resourceLabel is the human-readable name the /audit-logs page
// renders ("Updated arrears-mon" instead of the generic "Updated
// subscription"). Each caller passes the appropriate field — typically
// sub.Code, invoice.InvoiceNumber, customer.DisplayName, plan.Code,
// coupon.Code, etc. Pass "" when no label is available (event happens
// before the resource is hydratable, or the resource has no operator-
// facing name); the page falls back to the resource_type.
func (l *Logger) Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error {
	// Detach from the caller's cancellation (keeping its values — actor,
	// client IP, request id still resolve) so a client disconnect right
	// after the business operation commits cannot abort the audit write.
	// This is the residual own-tx path (ADR-090): it carries a real
	// post-commit window, which is exactly why LogInTx is the destination for
	// every writer that owns a transaction.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	tx, err := l.db.BeginTx(writeCtx, postgres.TxTenant, tenantID)
	if err != nil {
		// Own-tx path: the business mutation already committed, so this is evidence
		// PERMANENTLY LOST — nothing retries it. The tenant goes to the error log,
		// which is queryable and expires; the metric stays bounded.
		auditWriteErrors.WithLabelValues(OutcomeRowLost).Inc()
		slog.Error("audit: failed to begin transaction",
			"tenant_id", tenantID, "action", action,
			"resource_type", resourceType, "resource_id", resourceID,
			"error", err)
		return fmt.Errorf("audit log: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_aud")
	// Same ctx-derived sim axis as LogInTx (ADR-090 §5). The residual own-tx
	// callers are not a lesser class of evidence — the FINALIZE row, the heaviest
	// evidence in the log, is written through this path by BOTH of its writers:
	// billing.Engine.auditInvoiceFinalized (invoices the engine creates
	// already-finalized — cycle close, subscription create, cancel-final,
	// threshold — i.e. what a clock advance normally produces), and
	// invoice.Service.Finalize (the operator HTTP path and the tax-retry chain).
	// Engine invoices NEVER pass through Finalize; they are born finalized. Earlier
	// prose here named Finalize as the writer for "every engine-generated invoice",
	// which is the exact negation of what billing/engine.go says.
	simAt, clockID, metadata := simColumns(ctx, metadata)
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		// Same totality rule as LogInTx, and it was missing here. The old code
		// discarded this error and then guarded `metadata == nil` — the WRONG
		// case: on a marshal failure metadata is a perfectly valid non-nil map
		// and it is metaJSON that is nil. Nil binds into a JSONB NOT NULL column,
		// the INSERT fails, and ~85 call sites discard Log's error with `_ =` —
		// so one NaN in a telemetry bag silently DELETED the audit row for a
		// money mutation. Degrade instead: the row lands, carrying the failure it
		// represents.
		metaJSON, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	if metadata == nil {
		metaJSON = []byte("{}")
	}

	actorType, actorID := ResolveActor(ctx)

	ipAddress := ClientIP(ctx)
	requestID := chimw.GetReqID(ctx)

	// created_at is wall-clock — the audit row answers "when did the
	// operator click" / "when did the engine emit," which are both real-
	// time events regardless of which test clock the affected entity is
	// pinned to. ADR-030 table line 131 ("Audit log row recorded_at:
	// wall-clock | Forensics; never on the public API"). Diverged from
	// this for ~2 weeks via subscription.auditCtxForSub (since reverted
	// 2026-05-28) — see ADR-030 amendment.
	//
	// The SECOND axis is sim_effective_at + test_clock_id: the simulated
	// instant the ctx's clock binding CARRIED when this mutation was
	// performed. Since the contracted-instant arc (#520/#523, playbook
	// class J) the engine and dunning REBIND that instant per cycle close /
	// per retry, so rows inside one advance carry their own boundary
	// instants — a Jan→Mar advance stamps January's close on January's
	// rows. Writers without a rebinding still stamp where the clock stood.
	// Derived from the ctx's clock binding (simColumns), queryable via
	// ?test_clock_id= / ?sim_from= / ?sim_to=. It is not a substitute for
	// created_at — it is the only axis that survives ADR-086 teardown, which
	// hard-deletes every simulated business row and leaves the audit log as
	// the simulation's sole record.
	_, err = tx.ExecContext(writeCtx, `
		INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action,
			resource_type, resource_id, resource_label, metadata, ip_address,
			request_id, sim_effective_at, test_clock_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, id, tenantID, actorType, actorID, action, resourceType, resourceID, resourceLabel, metaJSON,
		nullIfEmpty(ipAddress), nullIfEmpty(requestID), simAt, clockID, time.Now().UTC())
	if err != nil {
		auditWriteErrors.WithLabelValues(OutcomeRowLost).Inc()
		slog.Error("audit: failed to insert entry",
			"tenant_id", tenantID, "action", action,
			"resource_type", resourceType, "resource_id", resourceID,
			"error", err)
		return fmt.Errorf("audit log: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		auditWriteErrors.WithLabelValues(OutcomeRowLost).Inc()
		slog.Error("audit: failed to commit entry",
			"tenant_id", tenantID, "action", action,
			"resource_type", resourceType, "resource_id", resourceID,
			"error", err)
		return fmt.Errorf("audit log: commit: %w", err)
	}

	// This request produced audit evidence — the AuditCoverage detector must not
	// report it as an uncovered mutation. Marked only after the COMMIT.
	markEmitted(ctx)
	return nil
}

// Entry is the typed input to LogInTx. Action/ResourceType take the
// domain.AuditAction* / existing wire strings — those strings are FROZEN
// (ADR-090): the FE filter vocabulary, badge maps, and existing rows all key
// on them, so a rename is a coordinated FE+MANUAL_TEST change, never a drive-by.
//
// There is deliberately NO sim field. The simulated-time axis
// (sim_effective_at / test_clock_id) is derived by the writer from the ctx's
// clock binding (clock.SimOf) — see simColumns. A per-emission field would
// mean ~90 emitters each remembering to populate it, which is exactly how
// stamping ended up partial: an emitter that forgets is invisible, and the
// resulting clock filter lies by omission. Deriving it once, at the two
// writers, makes the axis TOTAL for every path that runs under a bound clock
// and makes "clock C at wall-clock now" unrepresentable (clock.Sim pairs the
// halves). ADR-090 §5.
type Entry struct {
	Action        string
	ResourceType  string
	ResourceID    string
	ResourceLabel string
	Metadata      map[string]any
}

// SIM-AXIS COVERAGE (ADR-090 §5) — what ?test_clock_id= can and cannot see.
//
// COVERED: every path that runs under a clock binding. That is every ForClock /
// catchup phase (the advance binds clock.WithSim, so finalize, cancel, clawback
// issue, dunning and threshold rows all inherit it), every service whose entry
// point resolves an entity's pin (BindEffectiveNow yields the clock id along
// with the instant, and refuses to erase an inherited one), and the
// handler-level emitters that bind explicitly before emitting (invoice,
// subscription, customer — including CREATE, which binds from the clock it is
// handed because the customer pin does not exist yet — dunning, test_clock).
//
// A note on what "covered" means, so this list cannot quietly become a lie: a
// path is covered if it EMITS and its emission runs under a binding. Credit
// expiry runs under the catchup binding but emits NO audit row at all — the
// credit ledger is event-sourced and immutable, so the expiry entry IS the
// record, exactly as usage_events is the record for metering. It was listed
// here as covered, which credited a row that does not exist. Closure trigger:
// if credit expiry ever emits, it needs nothing but the ctx it already has.
//
// NOT COVERED — stated because a filter that omits rows silently is worse than
// no filter at all:
//
//   - Operator-driven CREDIT-NOTE routes (create/issue/void/retry-refund/send).
//     creditnote.Service has no clock.Resolver at all: the same defect that
//     makes an operator-issued CN against a simulated invoice stamp wall-clock
//     issued_at and is_simulated=false. Stamping the audit row while the entity
//     stays wall-clock would hide that bug inside the audit layer. Closure: fix
//     the CN clock binding — the sim axis then follows from ctx with no change
//     here. (Clock-DRIVEN CN paths — the catchup clawback issuer — ARE stamped.)
//   - Operator-driven PRICE-OVERRIDE routes (create/delete a customer price
//     override). Same root cause as the CN routes: internal/pricing has no
//     clock.Resolver at all, so the handler emits on an unbound r.Context() and
//     the row lands with NULL sim columns. This one is NOT a "not in the clock
//     domain" case and must not be mistaken for one — customer_price_overrides
//     IS torn down with the clock (ADR-086, testclock/postgres.go), so an
//     operator who negotiates a price for a simulated customer mid-simulation
//     leaves a row that is invisible to ?test_clock_id= AND, after teardown, is
//     the only surviving evidence the override ever existed. Closure: give
//     pricing a clock.Resolver and bind at the handler — the axis then follows
//     from ctx with no change here.
//   - PAYMENT-METHOD routes. NOT because they are "outside the clock domain" — an
//     earlier version of this note said that, and it is false by the very test the
//     price-override bullet above applies: payment_methods IS torn down with the
//     clock (testclock/postgres.go). The real reason is narrower and worth stating
//     exactly: a payment method is a REAL Stripe object. Attaching one during a
//     simulation attaches a real card at Stripe, so the row is a wall-clock fact
//     about a real-world effect, and stamping it into simulated time would claim
//     the card was attached at an instant that never happened. The accepted loss
//     is real: a clock-scoped view will not show "operator attached a card
//     mid-simulation." Closure trigger: the same one as the checkout rows below —
//     auditing the settle path, at which point these breadcrumbs join it.
//   - PUBLIC CHECKOUT rows (hosted-invoice Pay click, payment-update link,
//     checkout setup). These paths never bind a pin — they stamp wall-clock and
//     talk to real Stripe — so their rows are wall-clock rows about an entity
//     that happens to be pinned. This is consistent with what the clock view
//     already cannot show: the SETTLEMENT those clicks lead to is a Stripe
//     webhook, which the registry already records as writing no audit row at
//     all. Closure trigger: the same one — auditing the settle path.
//
// A row from either class has NULL sim columns and is therefore absent from the
// simulated slice — not mis-attributed. That is the safe failure direction, and
// it is the reason this comment exists instead of a plausible-looking guess.
//
// simColumns resolves the sim axis for an emission from the ctx's clock
// binding: the clock whose world this code path is running in, and the
// simulated instant it lands at. Returns (nil, nil) — SQL NULLs — for every
// wall-clock path, which is the overwhelming majority of rows and the reason
// the clock index (0148) is partial.
//
// It also mirrors the pair into the metadata bag under the legacy keys. The audit
// PAGE no longer reads that mirror — it reads the columns only, deliberately, so
// that what it renders is exactly what the clock filter can find. The mirror is
// still what the SUBSCRIPTION timeline renders (it offers no clock filter, so it
// has no promise to break), and it is where rows written before 0148 carry their
// sim context. The mirror is a COPY — the caller's map is never mutated, because
// emitters build their metadata once and reuse it.
//
// THE WRITER OWNS metadata["sim_effective_at"] AND metadata["test_clock_id"].
// Emitters must not set them: this function overwrites both unconditionally,
// silently, and an emitter cannot tell its value was dropped. Seven emitters
// used to hand-stamp them. Four wrote the same instant this does and were pure
// duplication. The other three (the trial-end cancel/activate rows) wrote the
// TRIAL-END instant — a genuinely different fact — and had it destroyed under
// catchup, because a catchup performs the cancel at the ADVANCE's instant,
// which can be weeks of simulated time after the trial ended. All seven are
// gone; the trial-end instant now travels as metadata["trial_end_at"], which
// says what it is. If you need another simulated instant on a row, give it its
// own key — do not overload this one.
//
// Both halves come from one clock.Sim, resolved from one read of the clock
// (clock.Resolver / the ForClock drivers), so the column pair can never
// disagree with itself. A partial binding reports Simulated()=false and is
// treated as absent rather than half-stamped: a row with a clock id and a
// wall-clock instant would sit IN the partial index and answer sim-time
// queries with a lie.
func simColumns(ctx context.Context, metadata map[string]any) (simAt, clockID any, meta map[string]any) {
	sim, ok := clock.SimOf(ctx)
	if !ok {
		return nil, nil, metadata
	}
	m := make(map[string]any, len(metadata)+2)
	for k, v := range metadata {
		m[k] = v
	}
	m["sim_effective_at"] = sim.At.UTC().Format(time.RFC3339)
	m["test_clock_id"] = sim.TestClockID
	return sim.At.UTC(), sim.TestClockID, m
}

// LogInTx writes an audit row on the CALLER's transaction — the business
// mutation and its audit row commit or roll back together (ADR-090 in-tx
// emission, shared fate). This is what makes "mutation committed but audit
// row missing" unrepresentable on covered paths; callers must propagate the
// error (which aborts their tx), never discard it.
//
// LogInTx is TOTAL by construction (design-panel amendment A2): tenant_id is
// taken from the transaction's own app.tenant_id GUC — it cannot disagree
// with RLS, and a missing GUC fails loudly: NULLIF(…, ”) maps BOTH the
// virgin-connection NULL and the pooled-connection empty string (a reverted
// is_local set_config placeholder — the dominant case in a warm pool) onto
// the NOT NULL constraint, so the guard does not silently depend on the
// tenants FK. A metadata bag that cannot marshal degrades to a
// {"marshal_error": …} payload instead of aborting a money transaction on a
// telemetry bug; a partial clock binding is treated as absent rather than
// polluting the partial clock index with a half-truth; the sim axis is
// derived from ctx, never supplied by the caller (simColumns);
// livemode is stamped by the table trigger from the same tx session. The
// vocabulary round-trip integration test INSERTs every declared action
// constant so a value the schema rejects cannot ship.
//
// LogInTx marks the request as having produced audit evidence (ADR-090 §4,
// amended by the uninstall PR — the ADR's "LogInTx does NOT mark the request
// handled" rule was written for the catch-all WRITER, where suppression cost a
// route its only row; it does not carry to a pure OBSERVER):
//
//   - Rolled back after we return? Then the mutation failed and the handler
//     answers non-2xx, and the detector only inspects 2xx — so a rolled-back
//     emission can never make an uncovered mutation look covered.
//   - A secondary emission deep inside another route's flow (the
//     proration-fallback grant inside a change-plan request) marks that request
//     too. That costs nothing REAL — the detector is a runtime backstop, and the
//     thing that actually holds a route to its own emission is the route-audit
//     registry + its arch test (internal/api/audit_routes.go), which forces every
//     mutating route to declare explicit or exempt(reason) in review. The blind
//     spot is bounded and named: a route that emits nothing of its own but nests
//     someone else's emission reads as covered at runtime.
//
// Self-marking is also load-bearing: POST /v1/tenants and both public checkout
// routes emit ONLY in-tx, and nothing else would account for them.
func (l *Logger) LogInTx(ctx context.Context, tx *sql.Tx, e Entry) error {
	simAt, clockID, metadata := simColumns(ctx, e.Metadata)
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		// Totality: never let an unmarshalable telemetry bag abort the
		// business tx (a nil metaJSON would violate the JSONB NOT NULL and
		// do exactly that). The row still lands, carrying the failure it
		// represents.
		metaJSON, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	if metadata == nil {
		metaJSON = []byte("{}")
	}

	actorType, actorID := ResolveActor(ctx)

	// created_at stays wall-clock (ADR-030); tenant_id comes from the tx
	// GUC so the row is authoritative for the transaction it rides in.
	// NULLIF folds the pooled-connection empty-string placeholder into
	// NULL so the NOT NULL constraint — not the incidental tenants FK —
	// is what rejects a GUC-less transaction.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action,
			resource_type, resource_id, resource_label, metadata, ip_address,
			request_id, sim_effective_at, test_clock_id, created_at)
		VALUES ($1, NULLIF(current_setting('app.tenant_id', true), ''), $2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	`, postgres.NewID("vlx_aud"), actorType, actorID, e.Action, e.ResourceType,
		e.ResourceID, e.ResourceLabel, metaJSON,
		nullIfEmpty(ClientIP(ctx)), nullIfEmpty(chimw.GetReqID(ctx)),
		simAt, clockID, time.Now().UTC())
	if err != nil {
		// In-tx path: returning this error ABORTS the caller's business transaction
		// (ADR-090 shared fate), so the mutation is refused and rolls back with the
		// row. No evidence is missing — nothing happened. That is the OPPOSITE
		// incident from the own-tx path's lost row, and the on-call runbook needs to
		// be able to tell them apart from the metric alone.
		auditWriteErrors.WithLabelValues(OutcomeMutationRefused).Inc()
		return fmt.Errorf("audit log (in-tx): insert: %w", err)
	}

	// This request produced audit evidence. Marked only AFTER a successful
	// INSERT: a failed emission must leave the request unaccounted-for, so the
	// caller's tx aborts (shared fate) and — if some caller were to swallow that
	// and still answer 2xx — the detector reports it rather than trusting a row
	// that isn't there.
	markEmitted(ctx)
	return nil
}

// ResolveActor determines who performed an audited action from the request
// context, in priority order:
//   - customer — a stamped customer actor (public token flows, auth.WithCustomerActor)
//   - user     — a dashboard operator on a session cookie (auth.WithUserID, ADR-011)
//   - api_key  — an SDK / curl caller on a Bearer vlx_… key (auth.WithKeyID)
//   - system   — background workers / cron with no request identity
//
// Every audit write path calls this, so the actor resolves identically on all of
// them: the post-hoc Logger.Log and the in-tx LogInTx. (There used to be a third
// — the catch-all AuditLog middleware — which ADR-090 deleted.) Before this, both read only auth.KeyID — and
// session.applyToCtx sets WithUserID but never WithKeyID — so every dashboard
// (session-cookie) operator action recorded actor_type='system'/'system', and
// the log could not answer "who did this?" for the primary UI. The 'user'
// value the audit_log CHECK constraint reserves was never written.
func ResolveActor(ctx context.Context) (actorType, actorID string) {
	if custID := auth.CustomerActorID(ctx); custID != "" {
		return "customer", custID
	}
	if userID := auth.UserID(ctx); userID != "" {
		return "user", userID
	}
	if keyID := auth.KeyID(ctx); keyID != "" {
		return "api_key", keyID
	}
	return "system", "system"
}

// nullIfEmpty preserves SQL NULL for optional text columns — avoids a
// column full of empty strings that later force callers to COALESCE.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// buildListWhere assembles the WHERE clause + args shared by the list and
// COUNT queries. Split out as a pure function so tests can pin the explicit
// tenant + livemode predicates: RLS (TxTenant) already enforces both, but the
// tenant-isolation policy carries a column-free bypass-GUC OR-arm, so the
// planner can never derive index quals from RLS alone — without the explicit
// predicates, every audit read (COUNT, page fetch, cursor seek) was a full
// seq scan across ALL tenants' rows (audit e2e 2026-07-13, F3). The values
// mirror exactly what BeginTx stamps into the GUCs from the caller's ctx, so
// they can only ever narrow, never disagree; RLS stays as the isolation
// backstop. Removing them keeps results identical (RLS masks it) and only
// destroys the query plan — which is why TestBuildListWhere_PinsPredicates
// exists.
func buildListWhere(tenantID string, livemode bool, filter QueryFilter) (whereClause string, args []any, nextIdx int, useCursor bool) {
	args = []any{tenantID, livemode}
	idx := 3
	where := " AND al.tenant_id = $1 AND al.livemode = $2"

	// Sim axis (0148). The clock predicate sits HERE, immediately after
	// tenant/livemode, because that is the leading-key order of the partial index
	// it rides: idx_audit_log_clock (tenant_id, livemode, test_clock_id,
	// created_at DESC, id DESC) WHERE test_clock_id IS NOT NULL. Equality on the
	// first three keys, then the list's sort and cursor columns already in order —
	// so a clock-scoped read needs no Sort step and the cursor seeks the same
	// tuple it sorts on. sim_effective_at is NOT an index key: there is one sort
	// axis (created_at), and a sim_from/sim_to window is a cheap filter inside an
	// already-narrow clock slice. TestSimAxis_UsesPartialClockIndex pins the plan.
	if filter.TestClockID != "" {
		where += andClause(&idx, "al.test_clock_id", &args, filter.TestClockID)
	}
	if !filter.SimFrom.IsZero() {
		where += fmt.Sprintf(" AND al.sim_effective_at >= $%d", idx)
		args = append(args, filter.SimFrom)
		idx++
	}
	if !filter.SimTo.IsZero() {
		where += fmt.Sprintf(" AND al.sim_effective_at <= $%d", idx)
		args = append(args, filter.SimTo)
		idx++
	}

	if filter.ResourceType != "" {
		where += andClause(&idx, "al.resource_type", &args, filter.ResourceType)
	}
	if filter.ResourceID != "" {
		where += andClause(&idx, "al.resource_id", &args, filter.ResourceID)
	}
	if filter.Action != "" {
		where += andClause(&idx, "al.action", &args, filter.Action)
	}
	if filter.ActorID != "" {
		where += andClause(&idx, "al.actor_id", &args, filter.ActorID)
	}
	// DateFrom / DateTo arrive as time.Time (parsed at the handler via
	// timefilter.ParseRange — accepts both RFC3339 and YYYY-MM-DD).
	// Postgres-go driver handles TIMESTAMPTZ binding from time.Time.
	// See ADR-010 for the tenant-TZ model.
	if !filter.DateFrom.IsZero() {
		where += fmt.Sprintf(" AND al.created_at >= $%d", idx)
		args = append(args, filter.DateFrom)
		idx++
	}
	if !filter.DateTo.IsZero() {
		where += fmt.Sprintf(" AND al.created_at <= $%d", idx)
		args = append(args, filter.DateTo)
		idx++
	}

	// The seek predicate is on the SAME axis as the ORDER BY (created_at, id) —
	// see auditListOrder. There is exactly ONE sort axis, deliberately: see the
	// note on the sim filters in QueryFilter.
	{
		useCursor = !filter.AfterCreatedAt.IsZero() && filter.AfterID != ""
		if useCursor {
			where += fmt.Sprintf(" AND (al.created_at, al.id) < ($%d, $%d)", idx, idx+1)
			args = append(args, filter.AfterCreatedAt, filter.AfterID)
			idx += 2
		}
	}

	return " WHERE " + where[5:], args, idx, useCursor // strip leading " AND "
}

// Query reads audit entries for a tenant.
func (l *Logger) Query(ctx context.Context, tenantID string, filter QueryFilter) ([]domain.AuditEntry, int, error) {
	tx, err := l.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// Default 50, clamp to 100 — was silently falling back to 50 on
	// >100 asks (no-silent-fallbacks principle, 2026-05-28).
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	whereClause, args, idx, useCursor := buildListWhere(tenantID, postgres.Livemode(ctx), filter)

	// Cursor path skips COUNT — the handler derives hasMore from a limit+1
	// over-fetch. The offset path (page 1, and every filter change) keeps a count
	// for the dashboard's "Page N · M total".
	//
	// It is BOUNDED. An unbounded COUNT(*) has to visit every matching row, and
	// audit_log is append-only — 0150 revoked DELETE from the runtime role, so the
	// set it walks grows forever and can never be reclaimed. The page that renders
	// 50 rows was paying an O(history) scan to print a number nobody acts on.
	//
	// Stop at CountCap+1 rows: below the cap the total is exact; at or above it the
	// caller is told "at least this many" and the UI says "10,000+".
	var total int
	if !useCursor {
		countQuery := `SELECT COUNT(*) FROM (SELECT 1 FROM audit_log al` + whereClause +
			fmt.Sprintf(" LIMIT %d) capped", CountCap+1)
		if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	query := auditListSelect + whereClause

	// Order by (created_at, id) DESC — id tiebreaker aligns with the
	// cursor predicate's tuple ordering so seek + ORDER BY stay in
	// lockstep regardless of microsecond-level ties.
	query += auditListOrder
	queryLimit := limit
	if useCursor {
		queryLimit = limit + 1
	}
	args = append(args, queryLimit)
	query += fmt.Sprintf(" LIMIT $%d", idx)
	idx++
	if !useCursor {
		args = append(args, filter.Offset)
		query += fmt.Sprintf(" OFFSET $%d", idx)
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var entries []domain.AuditEntry
	for rows.Next() {
		e, err := scanAuditEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		l.decryptActorName(&e)
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// auditListSelect is the row shape shared by the paged read (Query) and the
// unbounded compliance export (Stream) — ONE select, so the CSV an operator
// hands an auditor cannot drift from what the dashboard showed them.
//
// LEFT JOIN api_keys so the UI can show a human-readable key name instead of the
// raw actor_id (e.g., "Production" vs "vlx_secret_live_abc123…"). Join is
// (tenant_id, id), which matches the api_keys PK's tenant scope.
//
// Also LEFT JOIN customers for actor_type='customer' rows (customer-portal-driven
// mutations) and users for actor_type='user' rows (dashboard session operators —
// actor identity from #225): the operator's email is the identity they recognize
// (the post-ADR-011 users table carries email only — no display_name column).
// users is a global (non-RLS) table so the join works under TxTenant. COALESCE
// order is safe — the joins are mutually exclusive in practice (an actor_id is
// exactly one of key / customer / user).
// resource_label for `user` rows is RESOLVED AT READ TIME, never stored.
//
// audit_log is append-only and migration 0150 revoked DELETE from the runtime
// role, so anything written into resource_label is PERMANENT. Emails were being
// written there — the invitee's on member.invited, the operator's on login, the
// requester's on password_reset_requested — which made the audit log a GDPR
// erasure dead end: the one table from which a person's email could never be
// removed.
//
// Resolving instead of storing fixes both halves at once:
//
//   - ERASURE works. Delete the users row (or the member_invitations row) and the
//     address disappears from every historical audit row, because it was never in
//     them.
//   - "WHO was removed?" gets an answer. member.removed stores the target's USER
//     ID and no label; RemoveMember deletes the user_tenants row, so the tenant's
//     member list can no longer name them — but the global users row survives, and
//     this join reaches it.
//
// NULLIF-first so rows written BEFORE this change keep the label they stored.
// Append-only means their history cannot be rewritten; it can only stop growing.
const auditListSelect = `SELECT al.id, al.tenant_id, al.actor_type, al.actor_id,
		COALESCE(NULLIF(k.name, ''), c.display_name, u.email::text, '') AS actor_name,
		al.action, al.resource_type, al.resource_id,
		COALESCE(NULLIF(al.resource_label,''), ru.email::text, ri.email, '') AS resource_label,
		al.metadata,
		-- The row outlived its subject. ADR-086 teardown HARD-DELETES a clock's
		-- entire simulated graph, and the audit rows deliberately survive it — they
		-- are the only remaining record that the simulation happened. But the
		-- resources they name are gone, so a "View" link on them 404s, and the
		-- dashboard was rendering one. Tell the client, so it can stop.
		(al.test_clock_id IS NOT NULL AND tc.id IS NULL) AS subject_deleted,
		COALESCE(al.ip_address,''), COALESCE(al.request_id,''), al.created_at,
		al.sim_effective_at, COALESCE(al.test_clock_id,'')
		FROM audit_log al
		LEFT JOIN api_keys k ON k.id = al.actor_id AND k.tenant_id = al.tenant_id
		LEFT JOIN customers c ON c.id = al.actor_id AND c.tenant_id = al.tenant_id AND al.actor_type = 'customer'
		LEFT JOIN users u ON u.id = al.actor_id AND al.actor_type = 'user'
		LEFT JOIN users ru ON ru.id = al.resource_id AND al.resource_type = 'user'
		LEFT JOIN member_invitations ri ON ri.id = al.resource_id AND al.resource_type = 'user'
		LEFT JOIN test_clocks tc ON tc.id = al.test_clock_id`

// auditListOrder is newest-first, with the id tiebreaker the cursor predicate
// keys on.
const auditListOrder = " ORDER BY al.created_at DESC, al.id DESC"

// distinctByLooseIndexScan returns the distinct values of one indexed column for a
// tenant/mode, in O(distinct · log n) instead of O(rows).
//
// These two lists populate the audit page's filter dropdowns, and they ran as plain
// `SELECT DISTINCT` on EVERY page open — reading the tenant's ENTIRE audit history
// to produce a few dozen values. Postgres 16 has no btree skip scan, so no index
// could rescue that shape; and audit_log is append-only (0150 revoked DELETE from
// the runtime role), so the history it walks grows forever and can never be
// reclaimed. The cost had no ceiling.
//
// This is the standard loose-index-scan idiom: seek the smallest value, then
// repeatedly seek the smallest value STRICTLY GREATER than the last. Each step is
// one index descent, so the work is proportional to the number of DISTINCT values —
// which is what the dropdown actually wants — not to the number of rows.
//
// col is interpolated, never user input: the only two call sites pass the literals
// below, and both are indexed by 0151.
func distinctByLooseIndexScan(ctx context.Context, tx *sql.Tx, col, tenantID string, livemode bool, limit int) ([]string, error) {
	q := fmt.Sprintf(`
		WITH RECURSIVE t AS (
			SELECT (SELECT MIN(%[1]s) FROM audit_log
			         WHERE tenant_id = $1 AND livemode = $2) AS v
			UNION ALL
			SELECT (SELECT MIN(a.%[1]s) FROM audit_log a
			         WHERE a.tenant_id = $1 AND a.livemode = $2 AND a.%[1]s > t.v)
			FROM t WHERE t.v IS NOT NULL
		)
		SELECT v FROM t WHERE v IS NOT NULL ORDER BY v LIMIT $3`, col)

	rows, err := tx.QueryContext(ctx, q, tenantID, livemode, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanAuditEntry(rows *sql.Rows) (domain.AuditEntry, error) {
	var e domain.AuditEntry
	var metaJSON []byte
	// The sim axis (0148) rides the SHARED select, so the list API and the CSV
	// export can never disagree about which simulation a row belongs to.
	if err := rows.Scan(&e.ID, &e.TenantID, &e.ActorType, &e.ActorID, &e.ActorName,
		&e.Action, &e.ResourceType, &e.ResourceID, &e.ResourceLabel,
		&metaJSON, &e.SubjectDeleted, &e.IPAddress, &e.RequestID, &e.CreatedAt,
		&e.SimEffectiveAt, &e.TestClockID); err != nil {
		return domain.AuditEntry{}, err
	}
	_ = json.Unmarshal(metaJSON, &e.Metadata)
	return e, nil
}

// Stream hands EVERY audit row matching filter to fn, newest first, in one
// snapshot transaction — no limit, no cap, no pagination.
//
// It backs the server-side audit-log CSV export (GET /v1/exports/audit-log.csv).
// The dashboard used to build that file by PAGING THE API IN THE BROWSER and
// stopping at 50,000 rows: a silent truncation of the compliance evidence
// ITSELF, handed to an auditor as if it were the whole record. There is no cap
// here on purpose — a truncated audit export is a lie about the log, and the
// only honest bound is "what the filter selected".
//
// filter.Limit / filter.Offset / the cursor fields are IGNORED — and ENFORCED to
// be, below, by zeroing them. They used to be merely "not set by the caller":
// Stream handed the whole filter to buildListWhere, which happily applies a
// cursor predicate, so a future caller who passed a filter carrying one would
// have received a SILENTLY TRUNCATED export — the precise failure this function
// exists to prevent, arriving through the back door. The export is defined by its
// predicates, not by a page, and now it cannot be otherwise.
//
// Rows are consumed as they arrive off the wire (database/sql + pgx stream
// them), so the handler writes CSV to the socket while Postgres is still
// producing — a million-row export does not first become a million-row slice in
// memory. An error from fn aborts the walk and surfaces to the caller, which is
// how a mid-stream failure reaches exportAbort's EXPORT_INCOMPLETE marker.
func (l *Logger) Stream(ctx context.Context, tenantID string, filter QueryFilter, fn func(domain.AuditEntry) error) error {
	tx, err := l.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Zero the paging fields: see the doc above. An export is the whole selection.
	filter.Limit, filter.Offset = 0, 0
	filter.AfterCreatedAt, filter.AfterID = time.Time{}, ""

	whereClause, args, _, _ := buildListWhere(tenantID, postgres.Livemode(ctx), filter)

	rows, err := tx.QueryContext(ctx, auditListSelect+whereClause+auditListOrder, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		e, err := scanAuditEntry(rows)
		if err != nil {
			return err
		}
		l.decryptActorName(&e)
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// FilterOptions returns the distinct actions and resource_types recorded for
// a tenant. The audit page uses this to build filter dropdowns dynamically,
// so new event types appear automatically without a UI release.
func (l *Logger) FilterOptions(ctx context.Context, tenantID string) (actions, resourceTypes []string, err error) {
	tx, err := l.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, nil, err
	}
	defer postgres.Rollback(tx)

	// Both lists come from a LOOSE INDEX SCAN, not a plain DISTINCT — see
	// distinctByLooseIndexScan. They populate the filter dropdowns on every audit
	// page open, and a plain DISTINCT read the tenant's entire (unprunable) history
	// to produce a few dozen values.
	//
	// Cap at 500 to bound the payload: a tenant should not organically produce more
	// distinct actions than that, and if they do the cap drops the values sorting
	// LAST alphabetically, not the oldest ones.
	actions, err = distinctByLooseIndexScan(ctx, tx, "action", tenantID, postgres.Livemode(ctx), 500)
	if err != nil {
		return nil, nil, err
	}

	resourceTypes, err = distinctByLooseIndexScan(ctx, tx, "resource_type", tenantID, postgres.Livemode(ctx), 500)
	if err != nil {
		return nil, nil, err
	}

	return actions, resourceTypes, nil
}

// SimClock is one entry in the audit log's clock picker.
type SimClock struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SimClocks returns the distinct test clocks that appear on the tenant's audit
// rows, newest activity first, each with the best name the LOG itself knows.
//
// It deliberately does NOT read the test_clocks table. ADR-086 teardown
// hard-deletes a clock when the operator is done with it, and that is exactly
// when a forensic view is wanted — a picker sourced from test_clocks would go
// empty at the moment it becomes useful. The name is recovered from the clock's
// own audit rows (resource_type='test_clock', whose resource_label is the name
// the operator gave it), which survive teardown by design; unnamed or
// pre-0148 clocks fall back to the id in the UI.
func (l *Logger) SimClocks(ctx context.Context, tenantID string) ([]SimClock, error) {
	tx, err := l.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Explicit tenant+livemode predicates for the same reason as Query: the RLS
	// policy's column-free bypass OR-arm blocks index quals. The IS NOT NULL
	// scope is the partial clock index's own predicate.
	rows, err := tx.QueryContext(ctx, `
		SELECT c.test_clock_id, COALESCE(n.name, '') AS name
		FROM (
			SELECT test_clock_id, MAX(created_at) AS last_at
			FROM audit_log
			WHERE tenant_id = $1 AND livemode = $2 AND test_clock_id IS NOT NULL
			GROUP BY test_clock_id
			ORDER BY last_at DESC
			LIMIT 100
		) c
		LEFT JOIN LATERAL (
			SELECT resource_label AS name
			FROM audit_log
			WHERE tenant_id = $1 AND livemode = $2
			  AND resource_type = 'test_clock' AND resource_id = c.test_clock_id
			  AND COALESCE(resource_label, '') <> ''
			ORDER BY created_at DESC
			LIMIT 1
		) n ON true
		ORDER BY c.last_at DESC`, tenantID, postgres.Livemode(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []SimClock
	for rows.Next() {
		var c SimClock
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountCap bounds the "M total" the audit page shows.
//
// audit_log can never be pruned (0150), so an exact COUNT is an O(history) scan
// that grows without bound — paid on page 1 and on every filter change, to print a
// number the operator does not act on. At or above the cap the API reports CountCap
// and the dashboard renders "10,000+": the honest answer, at constant cost.
const CountCap = 10000

type QueryFilter struct {
	ResourceType string
	ResourceID   string
	Action       string
	ActorID      string
	// DateFrom / DateTo are parsed at the handler via timefilter.ParseRange
	// — accepts both RFC3339 and bare YYYY-MM-DD. Zero-value time.Time
	// means "no filter on this end."
	DateFrom time.Time
	DateTo   time.Time
	// Sim axis (ADR-090 §5). TestClockID scopes to one simulation; SimFrom/SimTo
	// window it in SIMULATED time. Both are meaningless for wall-clock rows,
	// whose sim columns are NULL, so filtering on them excludes those rows by
	// construction.
	//
	// There is deliberately NO "order by simulated time". Across clocks it
	// interleaves unrelated simulations into a timeline that never happened,
	// and a second sort means a second cursor axis to keep correct. Within
	// one clock the two orders USED to coincide (one advance = one instant);
	// since the contracted-instant arc (#520/#523) rows inside an advance
	// carry distinct per-boundary instants, so for a multi-subscription
	// advance sim order and wall order can genuinely differ — the original
	// "changes nothing" premise no longer holds. The deletion stands on the
	// cross-clock and cursor grounds alone; re-litigate if an operator needs
	// intra-advance ordering across subscriptions (that is now a real
	// ordering, not a no-op). Ordering stays on created_at (see
	// auditListOrder).
	TestClockID string
	SimFrom     time.Time
	SimTo       time.Time
	Limit       int
	Offset      int
	// Cursor-based pagination (2026-05-29). Seek-method query:
	// WHERE (al.created_at, al.id) < (AfterCreatedAt, AfterID).
	// Mutually exclusive with Offset — handler routes by ?after=
	// query param taking precedence. Audit_log is high-write, and
	// operators paginate deep for forensics, so OFFSET 50000 got
	// slow fast. Cursor-method is O(log N) per page regardless of
	// depth.
	AfterCreatedAt time.Time
	AfterID        string
}

func andClause(idx *int, col string, args *[]any, val string) string {
	clause := fmt.Sprintf(" AND %s = $%d", col, *idx)
	*args = append(*args, val)
	*idx++
	return clause
}
