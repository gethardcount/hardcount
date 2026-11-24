package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

var auditWriteErrors *prometheus.CounterVec

// Audit-write failure outcomes. The counter used to carry NO outcome label at all,
// so an on-call engineer paging on hardcount_audit_write_errors_total (docs/ops/runbook/audit-log-gaps.md:
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
		Name: "hardcount_audit_write_errors_total",
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
// so with HARDCOUNT_ENCRYPTION_KEY set — i.e. in production — every customer-actor
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
// etc. Pass "" when no label is available (event happens
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

	id := postgres.NewID("hc_aud")
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

	ipAddress := request.ClientIPIfAny(ctx)
	requestID := chimw.GetReqID(ctx)

	// created_at is wall-clock: the audit row answers "when did the
	// operator click" or "when did the engine emit", both real-time events.
	_, err = tx.ExecContext(writeCtx, `
		INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action,
			resource_type, resource_id, resource_label, metadata, ip_address,
			request_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, id, tenantID, actorType, actorID, action, resourceType, resourceID, resourceLabel, metaJSON,
		nullIfEmpty(ipAddress), nullIfEmpty(requestID), time.Now().UTC())
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
type Entry struct {
	Action        string
	ResourceType  string
	ResourceID    string
	ResourceLabel string
	Metadata      map[string]any
}

// LogInTxFields is LogInTx with the entry's fields as arguments, for a
// package that cannot import audit: auth, which audit imports for the actor.
func (l *Logger) LogInTxFields(ctx context.Context, tx *sql.Tx, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error {
	return l.LogInTx(ctx, tx, Entry{Action: action, ResourceType: resourceType, ResourceID: resourceID, ResourceLabel: resourceLabel, Metadata: metadata})
}

// LogInTx writes an audit row on the caller's transaction, so the caller's
// change and its audit row commit or roll back together (shared fate; see
// ADR-090). It returns an error only when the insert fails. The caller must
// return that error, which rolls its change back. Never discard it.
//
// The transaction must set the app.tenant_id and app.livemode settings
// (GUCs), or the insert fails. postgres.BeginTx sets both on a TxTenant
// transaction. A bypass transaction must set them itself. The row's tenant
// is that app.tenant_id value, so it always matches row-level security. A
// table trigger sets the row's livemode from app.livemode.
//
// No Entry may make the insert fail (LogInTx is total by construction).
// Otherwise a bug in audit data, such as telemetry metadata, would roll back
// a money change. Metadata that cannot be encoded as JSON is stored as
// {"marshal_error": ...} instead. The integration test
// TestLogInTx_TotalityAndVocabulary lists every domain.AuditAction* constant
// and inserts each one, so an action the schema rejects fails CI.
//
// After the insert succeeds, LogInTx also marks the request as audited for
// the mw.AuditCoverage detector.
func (l *Logger) LogInTx(ctx context.Context, tx *sql.Tx, e Entry) error {
	metadata := e.Metadata
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		// Keep LogInTx total: store the marshal error as the row's metadata,
		// so the row is still written. A nil metaJSON would violate the NOT
		// NULL constraint on the JSONB metadata column, fail the insert and
		// roll back the caller's change.
		metaJSON, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	if metadata == nil {
		metaJSON = []byte("{}")
	}

	actorType, actorID := ResolveActor(ctx)

	// created_at is wall-clock time (ADR-030). The tenant comes from the
	// transaction's app.tenant_id setting.
	// current_setting returns NULL on a connection that never set it. It
	// returns an empty string on a connection where an earlier transaction
	// set it with is_local, the usual case in a warm pool. NULLIF turns both
	// into NULL, so the NOT NULL constraint rejects a transaction with no
	// tenant. Without NULLIF, only the foreign key to tenants would catch the
	// empty string.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action,
			resource_type, resource_id, resource_label, metadata, ip_address,
			request_id, created_at)
		VALUES ($1, NULLIF(current_setting('app.tenant_id', true), ''), $2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, postgres.NewID("hc_aud"), actorType, actorID, e.Action, e.ResourceType,
		e.ResourceID, e.ResourceLabel, metaJSON,
		nullIfEmpty(request.ClientIPIfAny(ctx)), nullIfEmpty(chimw.GetReqID(ctx)),
		time.Now().UTC())
	if err != nil {
		// Count this as OutcomeMutationRefused, not OutcomeRowLost. Returning
		// this error refuses the caller's change, which rolls back with the
		// row. Nothing committed, so no evidence is missing. Log is the
		// opposite case: it writes after the change commits, so its failure
		// loses the row. The on-call runbook must tell the two apart from the
		// metric alone, so each gets its own outcome label.
		auditWriteErrors.WithLabelValues(OutcomeMutationRefused).Inc()
		return fmt.Errorf("audit log (in-tx): insert: %w", err)
	}

	// Mark only after the insert succeeds. If a caller ignored a failed
	// insert's error and still answered 2xx, the AuditCoverage detector
	// reports the request, because no row exists. Marking before the caller
	// commits is safe: a change that rolls back answers non-2xx, and the
	// detector reads only 2xx answers. The public checkout routes write audit
	// rows only through LogInTx, so nothing else marks them. One case looks
	// covered but is not: a route that writes no row of its own but runs
	// another flow's write, such as the proration-fallback grant inside a
	// change-plan request. See the amendments in ADR-090, section 4.
	markEmitted(ctx)
	return nil
}

// ResolveActor determines who performed an audited action from the request
// context, in this order:
//   - customer: a stamped customer actor (public token flows, auth.WithCustomerActor)
//   - the caller, as auth.Caller.Actor names it: user for a session, api_key
//     for any caller with a key
//   - system: a background job, with no caller
//
// A new kind must also be added to every actor_type CHECK (audit_log,
// email_outbox, invoice_charge_attempts) and to the contexts in
// TestResolveActorKinds_MatchEveryActorCheck.
func ResolveActor(ctx context.Context) (actorType, actorID string) {
	if custID := auth.CustomerActorID(ctx); custID != "" {
		return "customer", custID
	}
	if kind, id := auth.CallerOf(ctx).Actor(); kind != "" {
		return kind, id
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
func buildListWhere(tenantID string, livemode bool, filter QueryFilter) (whereClause string, args []any, nextIdx int) {
	args = []any{tenantID, livemode}
	idx := 3
	where := " AND al.tenant_id = $1 AND al.livemode = $2"

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

	return " WHERE " + where[5:], args, idx // strip leading " AND "
}

// Query reads audit entries for a tenant.
func (l *Logger) Query(ctx context.Context, tenantID string, filter QueryFilter) ([]domain.AuditEntry, int, error) {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return nil, 0, err
	}
	tx, err := l.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// The handler sends a limit of 1 to 100 (respond.ParsePage). 50 is the
	// same default for an internal caller that sends none.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	whereClause, args, idx := buildListWhere(tenantID, live, filter)

	// The count is BOUNDED. An unbounded COUNT(*) has to visit every matching
	// row, and audit_log is append-only — 0150 revoked DELETE from the runtime
	// role, so the set it walks grows forever and can never be reclaimed. The
	// page that renders 50 rows was paying an O(history) scan to print a number
	// nobody acts on.
	//
	// Stop at CountCap+1 rows: below the cap the total is exact; at or above it the
	// caller is told "at least this many" and the UI says "10,000+".
	var total int
	countQuery := `SELECT COUNT(*) FROM (SELECT 1 FROM audit_log al` + whereClause +
		fmt.Sprintf(" LIMIT %d) capped", CountCap+1)
	if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Order by (created_at, id) DESC — the id tiebreaker keeps the order
	// stable across pages when two rows share a microsecond.
	query := auditListSelect + whereClause + auditListOrder +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", idx, idx+1)
	args = append(args, limit, filter.Offset)

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
// raw actor_id (e.g., "Production" vs "hc_secret_live_abc123…"). Join is
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
		COALESCE(al.ip_address,''), COALESCE(al.request_id,''), al.created_at
		FROM audit_log al
		LEFT JOIN api_keys k ON k.id = al.actor_id AND k.tenant_id = al.tenant_id
		LEFT JOIN customers c ON c.id = al.actor_id AND c.tenant_id = al.tenant_id AND al.actor_type = 'customer'
		LEFT JOIN users u ON u.id = al.actor_id AND al.actor_type = 'user'
		LEFT JOIN users ru ON ru.id = al.resource_id AND al.resource_type = 'user'
		LEFT JOIN member_invitations ri ON ri.id = al.resource_id AND al.resource_type = 'user'`

// auditListOrder is newest-first, with an id tiebreaker so rows that share a
// created_at keep one order across pages.
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
	if err := rows.Scan(&e.ID, &e.TenantID, &e.ActorType, &e.ActorID, &e.ActorName,
		&e.Action, &e.ResourceType, &e.ResourceID, &e.ResourceLabel,
		&metaJSON, &e.IPAddress, &e.RequestID, &e.CreatedAt); err != nil {
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
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return err
	}
	tx, err := l.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Zero the paging fields: see the doc above. An export is the whole selection.
	filter.Limit, filter.Offset = 0, 0

	whereClause, args, _ := buildListWhere(tenantID, live, filter)

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
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return nil, nil, err
	}
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
	actions, err = distinctByLooseIndexScan(ctx, tx, "action", tenantID, live, 500)
	if err != nil {
		return nil, nil, err
	}

	resourceTypes, err = distinctByLooseIndexScan(ctx, tx, "resource_type", tenantID, live, 500)
	if err != nil {
		return nil, nil, err
	}

	return actions, resourceTypes, nil
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
	Limit    int
	Offset   int
}

func andClause(idx *int, col string, args *[]any, val string) string {
	clause := fmt.Sprintf(" AND %s = $%d", col, *idx)
	*args = append(*args, val)
	*idx++
	return clause
}
