package dunning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/leader"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type PostgresStore struct {
	db     *postgres.DB
	outbox OutboxEnqueuer
}

// OutboxEnqueuer inserts a webhook-event row on the caller's tx, so the event
// commits or rolls back with the state change that caused it (ADR-040
// transactional outbox). Satisfied by *webhook.OutboxStore; declared here so
// this store needs no webhook import. Same seam as the subscription, invoice,
// and credit stores.
type OutboxEnqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// SetOutboxEnqueuer wires the dunning.* webhook events into the transactions
// of the run writes they report: dunning.started (StartRun), .escalated
// (EscalateRun) and .resolved (ResolveRun). Optional: when unset (narrow
// tests), no event is enqueued. Production boot refuses an unset one
// (webhook.MustWiredOutbox).
func (s *PostgresStore) SetOutboxEnqueuer(o OutboxEnqueuer) { s.outbox = o }

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// scanPolicy is the shared row decoder for the policy SELECTs below, which
// return policyColumns in that order.
func scanPolicy(row interface {
	Scan(dest ...any) error
}) (domain.DunningPolicy, error) {
	var p domain.DunningPolicy
	var daysJSON []byte
	if err := row.Scan(&p.ID, &p.TenantID, &p.Enabled, &daysJSON,
		&p.FinalSubscriptionAction, &p.FinalInvoiceAction,
		&p.CreatedAt, &p.UpdatedAt); err != nil {
		return domain.DunningPolicy{}, err
	}
	// The column's CHECK keeps retry_days a list of 1 to 15 whole days, so
	// a decode error here is a broken database, not "no retries": fail.
	if err := json.Unmarshal(daysJSON, &p.RetryDays); err != nil {
		return domain.DunningPolicy{}, fmt.Errorf("decode retry_days for dunning policy %s: %w", p.ID, err)
	}
	return p, nil
}

const policyColumns = `id, tenant_id, enabled, retry_days, final_subscription_action, final_invoice_action, created_at, updated_at`

// GetPolicy returns the account's policy for the mode on ctx. Row-level
// security scopes the read to the tenant and mode, and the unique index on
// (tenant_id, livemode) leaves at most one row.
func (s *PostgresStore) GetPolicy(ctx context.Context, tenantID string) (domain.DunningPolicy, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	defer postgres.Rollback(tx)

	p, err := scanPolicy(tx.QueryRowContext(ctx, `SELECT `+policyColumns+` FROM dunning_policies`))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DunningPolicy{}, errs.ErrNotFound
	}
	return p, err
}

// PutPolicy creates the account's policy for the mode on ctx, or replaces
// every field of the one it has. One statement against the unique index on
// (tenant_id, livemode), so two puts at once leave one row, the later put's.
// The set_livemode trigger stamps the inserted row's mode before the
// conflict check.
func (s *PostgresStore) PutPolicy(ctx context.Context, tenantID string, p domain.DunningPolicy) (domain.DunningPolicy, error) {
	// A dunning policy is tenant-level operator config, not a billing event:
	// its created_at/updated_at record the machine's time of the edit.
	now := time.Now().UTC() //nolint:forbidigo // wall-clock: operator config write
	daysJSON, err := json.Marshal(p.RetryDays)
	if err != nil {
		return domain.DunningPolicy{}, fmt.Errorf("encode retry_days: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	defer postgres.Rollback(tx)

	stored, err := scanPolicy(tx.QueryRowContext(ctx, `
		INSERT INTO dunning_policies (id, tenant_id, enabled, retry_days,
			final_subscription_action, final_invoice_action, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
		ON CONFLICT (tenant_id, livemode) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			retry_days = EXCLUDED.retry_days,
			final_subscription_action = EXCLUDED.final_subscription_action,
			final_invoice_action = EXCLUDED.final_invoice_action,
			updated_at = EXCLUDED.updated_at
		RETURNING `+policyColumns,
		postgres.NewID("hc_dpol"), tenantID, p.Enabled, daysJSON,
		p.FinalSubscriptionAction, p.FinalInvoiceAction, now,
	))
	if err != nil {
		return domain.DunningPolicy{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.DunningPolicy{}, err
	}
	return stored, nil
}

// SeedDefaultPoliciesTx inserts p once per mode (test, then live) on the
// caller's account-creation tx (ADR-123). Each insert runs with
// app.livemode set to its mode, so the livemode trigger stamps the row and
// the RLS check passes; onSeeded runs in the same mode, so an audit row
// lands in the matching log. The caller's app.livemode is restored before
// returning. If the account somehow has a policy already, the unique index
// rejects the insert and the account is not created.
func (s *PostgresStore) SeedDefaultPoliciesTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.DunningPolicy, onSeeded func(p domain.DunningPolicy, livemode bool) error) error {
	var prevMode sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('app.livemode', true)`).Scan(&prevMode); err != nil {
		return fmt.Errorf("read livemode: %w", err)
	}
	now := time.Now().UTC() //nolint:forbidigo // wall-clock: operator config write
	daysJSON, err := json.Marshal(p.RetryDays)
	if err != nil {
		return fmt.Errorf("encode retry_days: %w", err)
	}
	for _, livemode := range []bool{false, true} {
		mode := "off"
		if livemode {
			mode = "on"
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, mode); err != nil {
			return fmt.Errorf("set livemode: %w", err)
		}
		stored, err := scanPolicy(tx.QueryRowContext(ctx, `
			INSERT INTO dunning_policies (id, tenant_id, enabled,
				retry_days, final_subscription_action, final_invoice_action, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$7)
			RETURNING `+policyColumns,
			postgres.NewID("hc_dpol"), tenantID, p.Enabled, daysJSON,
			p.FinalSubscriptionAction, p.FinalInvoiceAction, now,
		))
		if err != nil {
			return fmt.Errorf("seed %s-mode dunning policy: %w", mode, err)
		}
		if onSeeded != nil {
			if err := onSeeded(stored, livemode); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, prevMode.String); err != nil {
		return fmt.Errorf("restore livemode: %w", err)
	}
	return nil
}

// CreateRun inserts a run row and nothing else. Production starts a run with
// StartRun, which also writes its started event and dunning.started; this is
// the bare insert, for seeding a run in a known state.
func (s *PostgresStore) CreateRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	run, err = s.createRunTx(ctx, tx, tenantID, run)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	return run, nil
}

// StartRun starts dunning for an invoice in ONE transaction: the run row, its
// dunning_started timeline event, and the dunning.started webhook (an outbox
// row the dispatcher delivers later). All three commit together or none do.
// Before, they were three commits: a crash after the first left a run with no
// timeline row and no webhook, and nothing re-sent them, because StartDunning
// is idempotent on the existing run. The event takes the run's reason and
// created_at, so the timeline row sits at the failure instant.
func (s *PostgresStore) StartRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	run, err = s.createRunTx(ctx, tx, tenantID, run)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	if _, err := s.createEventTx(ctx, tx, tenantID, domain.InvoiceDunningEvent{
		RunID:     run.ID,
		InvoiceID: run.InvoiceID,
		EventType: domain.DunningEventStarted,
		State:     run.State,
		Reason:    run.Reason,
		CreatedAt: run.CreatedAt,
	}); err != nil {
		return domain.InvoiceDunningRun{}, fmt.Errorf("started event: %w", err)
	}
	if s.outbox != nil {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, domain.EventDunningStarted, domain.DunningRunWebhookData(run, nil)); err != nil {
			return domain.InvoiceDunningRun{}, fmt.Errorf("enqueue %s: %w", domain.EventDunningStarted, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	return run, nil
}

func (s *PostgresStore) createRunTx(ctx context.Context, tx *sql.Tx, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	id := postgres.NewID("hc_drun")
	now := run.CreatedAt
	if now.IsZero() {
		now = clock.Now(ctx)
	}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id,
			state, reason, attempt_count, next_action_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)
		RETURNING id, tenant_id, invoice_id, COALESCE(customer_id,''), state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
	`, id, tenantID, run.InvoiceID, postgres.NullableString(run.CustomerID),
		run.State, postgres.NullableString(run.Reason),
		run.AttemptCount, postgres.NullableTime(run.NextActionAt), now,
	).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	return run, nil
}

func (s *PostgresStore) GetRun(ctx context.Context, tenantID, id string) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	var run domain.InvoiceDunningRun
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, invoice_id, COALESCE(customer_id,''), state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
		FROM invoice_dunning_runs WHERE id = $1
	`, id).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return run, err
}

// GetRunByInvoice returns the dunning run for the given invoice
// regardless of state. With the migration-0085 UNIQUE index there
// is at most one run per (tenant_id, invoice_id) — this method
// returns it (or ErrNotFound). Used by StartDunning's lifetime
// idempotency check.
func (s *PostgresStore) GetRunByInvoice(ctx context.Context, tenantID, invoiceID string) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	var run domain.InvoiceDunningRun
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, invoice_id, COALESCE(customer_id,''), state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
		FROM invoice_dunning_runs
		WHERE invoice_id = $1
		LIMIT 1
	`, invoiceID).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return run, err
}

// ListRunsByInvoices returns the dunning runs of the given invoices in one
// read — at most one per invoice (idx_dunning_runs_one_per_invoice), the same
// row GetRunByInvoice returns. Invoices with no run are simply absent. Backs
// the invoice list's attention banners, which read the run of many rows at
// once (issue #1142).
func (s *PostgresStore) ListRunsByInvoices(ctx context.Context, tenantID string, invoiceIDs []string) ([]domain.InvoiceDunningRun, error) {
	if len(invoiceIDs) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// The retry count of the policy for the run's mode (the length of its
	// retry_days) rides along (LEFT JOIN: a run is never dropped for its
	// policy), so a reader can tell a run with a retry still to come from
	// one whose next step is only its final action
	// (InvoiceDunningRun.RetryScheduled).
	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.tenant_id, r.invoice_id, COALESCE(r.customer_id,''), r.state,
			COALESCE(r.reason,''), r.attempt_count, r.last_attempt_at, r.next_action_at,
			r.paused, r.resolved_at, COALESCE(r.resolution,''), r.created_at, r.updated_at,
			COALESCE(jsonb_array_length(p.retry_days), 0)
		FROM invoice_dunning_runs r
		LEFT JOIN dunning_policies p ON p.tenant_id = r.tenant_id AND p.livemode = r.livemode
		WHERE r.invoice_id = ANY($1::text[])
	`, invoiceIDs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.InvoiceDunningRun
	for rows.Next() {
		var run domain.InvoiceDunningRun
		if err := rows.Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID,
			&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
			&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt,
			&run.MaxRetryAttempts); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetActiveRunByInvoice(ctx context.Context, tenantID, invoiceID string) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	var run domain.InvoiceDunningRun
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, invoice_id, COALESCE(customer_id,''), state,
			COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
			paused, resolved_at, COALESCE(resolution,''), created_at, updated_at
		FROM invoice_dunning_runs
		-- Exclude only already-resolved runs. Escalated (retries-exhausted) runs
		-- MUST still be returned so ResolveByInvoice can resolve them when the
		-- customer pays out-of-band after escalation; otherwise the run is stuck
		-- in 'escalated' forever and never emits dunning.resolved.
		WHERE invoice_id = $1 AND state != 'resolved'
		LIMIT 1
	`, invoiceID).Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	if err == sql.ErrNoRows {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return run, err
}

func (s *PostgresStore) ListRuns(ctx context.Context, filter RunListFilter) ([]domain.InvoiceDunningRun, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	args := []any{}
	clauses := []string{}
	idx := 1

	if filter.InvoiceID != "" {
		clauses = append(clauses, fmt.Sprintf("r.invoice_id = $%d", idx))
		args = append(args, filter.InvoiceID)
		idx++
	}
	if filter.State != "" {
		clauses = append(clauses, fmt.Sprintf("r.state = $%d", idx))
		args = append(args, filter.State)
		idx++
	}

	whereClause := ""
	if len(clauses) > 0 {
		whereClause = " WHERE "
		for i, c := range clauses {
			if i > 0 {
				whereClause += " AND "
			}
			whereClause += c
		}
	}

	var total int
	countQuery := `SELECT COUNT(*) FROM invoice_dunning_runs r` + whereClause
	if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// LEFT JOIN denormalizes invoice fields onto each row, so the /dunning
	// page is one query, not 1 list + N invoice fetches. LEFT JOINs because
	// an invoice reference can be unresolvable (RLS gap) without dropping
	// the dunning row itself. The retry count of the run's policy rides along
	// too, so the page can tell a run with a retry still to come from one
	// whose next step is only its final action
	// (InvoiceDunningRun.RetryScheduled).
	query := `SELECT r.id, r.tenant_id, r.invoice_id, COALESCE(r.customer_id,''), r.state,
		COALESCE(r.reason,''), r.attempt_count, r.last_attempt_at, r.next_action_at,
		r.paused, r.resolved_at, COALESCE(r.resolution,''), r.created_at, r.updated_at,
		COALESCE(i.invoice_number, ''), COALESCE(i.amount_due_cents, 0), COALESCE(i.currency, ''),
		COALESCE(jsonb_array_length(p.retry_days), 0)
		FROM invoice_dunning_runs r
		LEFT JOIN invoices i ON i.id = r.invoice_id
		LEFT JOIN dunning_policies p ON p.tenant_id = r.tenant_id AND p.livemode = r.livemode` + whereClause + ` ORDER BY r.created_at DESC, r.id DESC`
	// Default 50, clamp to 100 — no-silent-fallbacks principle.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", idx, idx+1)
	args = append(args, limit, filter.Offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var runs []domain.InvoiceDunningRun
	for rows.Next() {
		var r domain.InvoiceDunningRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.InvoiceID, &r.CustomerID,
			&r.State, &r.Reason, &r.AttemptCount, &r.LastAttemptAt, &r.NextActionAt,
			&r.Paused, &r.ResolvedAt, &r.Resolution, &r.CreatedAt, &r.UpdatedAt,
			&r.InvoiceNumber, &r.InvoiceAmountDue, &r.InvoiceCurrency,
			&r.MaxRetryAttempts); err != nil {
			return nil, 0, err
		}
		runs = append(runs, r)
	}
	return runs, total, rows.Err()
}

// ResolveRun applies the run's resolved fields as a CAS — only when the row is not
// already 'resolved' — and returns whether THIS call won the transition. Same field
// set as UpdateRun plus the `state <> 'resolved'` guard. The winner's transaction
// also queues dunning.resolved, carrying the run as this write left it (ADR-040,
// ADR-131), so the resolve and its event commit together and a loser queues
// nothing. A failed insert fails the resolve, and the run stays as it was.
func (s *PostgresStore) ResolveRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, fromActiveOnly bool) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	// A resolved run waits on no payment (#1289).
	written, err := scanRun(tx.QueryRowContext(ctx, `
		UPDATE invoice_dunning_runs SET state=$1, reason=$2, attempt_count=$3,
			last_attempt_at=$4, next_action_at=$5, paused=$6, resolved_at=$7, resolution=$8, updated_at=$9,
			awaiting_payment_intent_id = NULL
		WHERE id=$10 AND state <> 'resolved' AND (NOT $11 OR state = 'active')
		RETURNING `+runColumns,
		run.State, postgres.NullableString(run.Reason), run.AttemptCount,
		postgres.NullableTime(run.LastAttemptAt), postgres.NullableTime(run.NextActionAt),
		run.Paused, postgres.NullableTime(run.ResolvedAt), postgres.NullableString(string(run.Resolution)),
		now, run.ID, fromActiveOnly))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.enqueueRunEvent(ctx, tx, tenantID, domain.EventDunningResolved, written, nil); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// enqueueRunEvent queues a dunning.* webhook event on tx, carrying run as
// GET /v1/dunning/runs/{id} returns it, with extra beside it (ADR-131).
// Without an outbox (narrow tests) it queues nothing.
func (s *PostgresStore) enqueueRunEvent(ctx context.Context, tx *sql.Tx, tenantID, eventType string, run domain.InvoiceDunningRun, extra map[string]any) error {
	if s.outbox == nil {
		return nil
	}
	if _, err := s.outbox.Enqueue(ctx, tx, tenantID, eventType, domain.DunningRunWebhookData(run, extra)); err != nil {
		return fmt.Errorf("enqueue %s: %w", eventType, err)
	}
	return nil
}

// UpdateRunIfActive applies the run's fields only when the row is still
// ACTIVE and still carries the attempt_count the caller's write was derived
// from. The two guards and one refusal, each for its own defect:
//
//   - state = 'active' (SB-2, 2026-10-08): escalated is terminal for the
//     automated path just as resolved is. The old `state <> 'resolved'`
//     admitted escalated rows, so a second processor could escalate a run
//     twice (a second final-notice email, a second dunning.escalated) or
//     flip an escalated run back to active. ResolveRun stays
//     `state <> 'resolved'` on purpose: escalated -> resolved after a late
//     payment is legitimate.
//   - attempt_count = expected (ha-8, 2026-08-31), below.
//   - an ACTIVE run is never written with next_action_at NULL (SB-1,
//     2026-10-08): both due-run pickers select on next_action_at, so NULL
//     removes an active run from processing forever.
//
// ha-8: The state guard alone left the COUNT a
// blind read-modify-write: two processors of one run — a superseded-but-running
// dunning tick beside the new leader's (the ADR-114 frozen-process window;
// ListDueRuns' claim tx is rolled back before processing, so rows are not
// leased) — both read N, both wrote N+1, and the charge-lease loser's rewind
// then clobbered the winner's recorded attempt back to N: one charge burned
// zero budget, and a tenant's retry count over-ran by one charge per
// overlap. The money itself was never double-charged (ClaimChargeForDunningRetry
// is the per-invoice charge lease); the CAS makes the BOOKKEEPING honest, and
// email-money-in-tx builds on it.
// then (nil = none) runs INSIDE the same transaction, after the CAS applied,
// under a SAVEPOINT: the dunning state change and its customer email commit
// together (ADR-040 — a failover between the two used to lose the email with
// the state standing), but an email-side failure only rolls back to the
// savepoint and is logged loud — it never vetoes the money write.
//
// Every write here clears the PaymentIntent the run waits on (#1289): the
// next attempt, a counted failure, the final action and its re-attempt all
// mean the run no longer waits on that payment. Only RecordRetryInFlight
// sets it.
func (s *PostgresStore) UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, then func(tx *sql.Tx) error) (bool, error) {
	return s.writeRunIfActive(ctx, tenantID, run, expectedAttempts, nil, then)
}

// EscalateRun is UpdateRunIfActive for the escalation, run being the
// escalated run. When the write applies, its transaction also queues
// dunning.escalated, carrying the run as the write left it with extra
// beside it (ADR-040, ADR-131). A failed insert fails the escalation, so the
// two commit together or not at all. The event is queued before then's
// SAVEPOINT: an email failure skips only the email. The caller writes the
// escalated timeline row after the commit (see EscalateRunTx for why).
func (s *PostgresStore) EscalateRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, extra map[string]any, then func(tx *sql.Tx) error) (bool, error) {
	if run.State != domain.DunningEscalated {
		return false, fmt.Errorf("dunning run %s: EscalateRun writes an escalated run, not one in state %q", run.ID, run.State)
	}
	return s.writeRunIfActive(ctx, tenantID, run, expectedAttempts, func(tx *sql.Tx, written domain.InvoiceDunningRun) error {
		return s.enqueueRunEvent(ctx, tx, tenantID, domain.EventDunningEscalated, written, extra)
	}, then)
}

// EscalateRunTx is EscalateRun on the caller's transaction tx, which commits
// it: the write-off's (invoice.Service.MarkUncollectibleThen), so the
// write-off and the escalation commit together. When the write applies, tx
// also queues dunning.escalated with extra beside the run and inserts event,
// the escalated timeline row, then runs then under the email SAVEPOINT. A
// failed insert fails the escalation, and the write-off with it.
//
// Lock order: tx must already hold the invoice's row lock (FOR UPDATE)
// before this writes the run, the order the failure settle and
// RecordRetryInFlight follow. The timeline row's foreign key takes a
// key-share lock on that invoice, which tx then already holds. A caller
// without the invoice's lock uses EscalateRun and writes the row after its
// commit: writing the run and then waiting on the invoice would deadlock
// against a settle that holds the invoice and waits on the run.
func (s *PostgresStore) EscalateRunTx(ctx context.Context, tx *sql.Tx, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, extra map[string]any, event domain.InvoiceDunningEvent, then func(tx *sql.Tx) error) (bool, error) {
	if run.State != domain.DunningEscalated {
		return false, fmt.Errorf("dunning run %s: EscalateRunTx writes an escalated run, not one in state %q", run.ID, run.State)
	}
	return s.writeRunIfActiveTx(ctx, tx, run, expectedAttempts, func(tx *sql.Tx, written domain.InvoiceDunningRun) error {
		if err := s.enqueueRunEvent(ctx, tx, tenantID, domain.EventDunningEscalated, written, extra); err != nil {
			return err
		}
		if _, err := s.createEventTx(ctx, tx, tenantID, event); err != nil {
			return fmt.Errorf("escalated timeline row: %w", err)
		}
		return nil
	}, then)
}

// writeRunIfActive is writeRunIfActiveTx in its own transaction, which it
// commits when the write applies: the guarded write of UpdateRunIfActive and
// EscalateRun.
func (s *PostgresStore) writeRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, onWrite func(tx *sql.Tx, written domain.InvoiceDunningRun) error, then func(tx *sql.Tx) error) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	applied, err := s.writeRunIfActiveTx(ctx, tx, run, expectedAttempts, onWrite, then)
	if err != nil || !applied {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// writeRunIfActiveTx writes run on tx only while the row is still active at
// expectedAttempts (UpdateRunIfActive's guards), and reports whether it
// applied. onWrite (nil = none) runs on tx right after the write applies,
// with the row it wrote, and its error fails the whole write. then runs after
// it, under the email SAVEPOINT. The caller commits.
func (s *PostgresStore) writeRunIfActiveTx(ctx context.Context, tx *sql.Tx, run domain.InvoiceDunningRun, expectedAttempts int, onWrite func(tx *sql.Tx, written domain.InvoiceDunningRun) error, then func(tx *sql.Tx) error) (bool, error) {
	if err := refuseStrandingWrite(run); err != nil {
		return false, err
	}
	now := clock.Now(ctx)
	written, err := scanRun(tx.QueryRowContext(ctx, `
		UPDATE invoice_dunning_runs SET state=$1, reason=$2, attempt_count=$3,
			last_attempt_at=$4, next_action_at=$5, paused=$6, resolved_at=$7, resolution=$8, updated_at=$9,
			awaiting_payment_intent_id = NULL
		WHERE id=$10 AND state = 'active' AND attempt_count = $11
		RETURNING `+runColumns,
		run.State, postgres.NullableString(run.Reason), run.AttemptCount,
		postgres.NullableTime(run.LastAttemptAt), postgres.NullableTime(run.NextActionAt),
		run.Paused, postgres.NullableTime(run.ResolvedAt), postgres.NullableString(string(run.Resolution)),
		now, run.ID, expectedAttempts))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if onWrite != nil {
		if err := onWrite(tx, written); err != nil {
			return false, err
		}
	}
	if then != nil {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT dunning_email"); err != nil {
			return false, err
		}
		if herr := then(tx); herr != nil {
			if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT dunning_email"); rerr != nil {
				return false, rerr
			}
			slog.Error("dunning email enqueue failed inside the state transaction — the email is skipped, the state change goes ahead",
				"run_id", run.ID, "invoice_id", run.InvoiceID, "error", herr)
		} else if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT dunning_email"); err != nil {
			return false, err
		}
	}
	return true, nil
}

// refuseStrandingWrite is the liveness invariant both store implementations
// enforce on every UpdateRunIfActive write: an active run must carry
// next_action_at. (CreateRun is not guarded; its only production caller,
// StartDunning, always sets it.)
func refuseStrandingWrite(run domain.InvoiceDunningRun) error {
	if run.State == domain.DunningActive && run.NextActionAt == nil {
		return fmt.Errorf("dunning run %s: refusing to write an active run with no next_action_at (both due-run pickers would never select it again)", run.ID)
	}
	return nil
}

// ClaimExhaustion is the dunning-level claim taken before any terminal action
// fires on the exhaust-on-entry path. attempt_count does not move during
// exhaustion, so the version that tells two processors apart is
// next_action_at: the caller passes the value it just listed, and the claim
// moves it to lease. Exactly one processor wins; the loser fires nothing.
// expectedNext is always a value scanned from this column, so equality is
// exact at the column's microsecond precision.
func (s *PostgresStore) ClaimExhaustion(ctx context.Context, tenantID, runID string, expectedAttempts int, expectedNext, lease time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(ctx, `
		UPDATE invoice_dunning_runs SET next_action_at=$1, updated_at=$2
		WHERE id=$3 AND state = 'active' AND attempt_count = $4 AND next_action_at = $5`,
		lease, clock.Now(ctx), runID, expectedAttempts, expectedNext)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// runColumns is the run row as the single-run reads return it, in the order
// scanRun reads.
const runColumns = `id, tenant_id, invoice_id, COALESCE(customer_id,''), state,
	COALESCE(reason,''), attempt_count, last_attempt_at, next_action_at,
	paused, resolved_at, COALESCE(resolution,''), created_at, updated_at`

func scanRun(row interface{ Scan(dest ...any) error }) (domain.InvoiceDunningRun, error) {
	var run domain.InvoiceDunningRun
	err := row.Scan(&run.ID, &run.TenantID, &run.InvoiceID, &run.CustomerID,
		&run.State, &run.Reason, &run.AttemptCount, &run.LastAttemptAt, &run.NextActionAt,
		&run.Paused, &run.ResolvedAt, &run.Resolution, &run.CreatedAt, &run.UpdatedAt)
	return run, err
}

// RecordRetryInFlight writes a retry whose payment is still processing
// (#1289): the run's fields under UpdateRunIfActive's guard (active, at
// expectedAttempts), the PaymentIntent it now waits on, and its
// retry_attempted event, all in one transaction. So a run resolved in the
// meantime leaves no stray event behind.
//
// Lock order: the invoice row first (the money-path playbook, item 8).
// The failure settle locks the invoice FOR UPDATE and then writes this run
// (TakeAwaitedRetryTx), and the event insert below takes a key-share lock on
// the invoice for its foreign key. Writing the run first would deadlock
// against that settle. FOR SHARE also waits for the settle to commit, so the
// check that follows reads its outcome: when the invoice no longer waits on
// paymentIntentID with its payment in flight, the payment succeeded or failed
// before this write and decided the attempt, and nothing is written.
func (s *PostgresStore) RecordRetryInFlight(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, paymentIntentID string, event domain.InvoiceDunningEvent) (RetryInFlightResult, error) {
	if paymentIntentID == "" {
		return RetryInFlightRunMoved, fmt.Errorf("dunning run %s: a retry whose payment is in flight needs its PaymentIntent id", run.ID)
	}
	if err := refuseStrandingWrite(run); err != nil {
		return RetryInFlightRunMoved, err
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return RetryInFlightRunMoved, err
	}
	defer postgres.Rollback(tx)

	var inv domain.Invoice
	var pi sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT payment_status, stripe_payment_intent_id FROM invoices WHERE id = $1 FOR SHARE`, run.InvoiceID,
	).Scan(&inv.PaymentStatus, &pi)
	if errors.Is(err, sql.ErrNoRows) {
		return RetryInFlightRunMoved, errs.ErrNotFound
	}
	if err != nil {
		return RetryInFlightRunMoved, fmt.Errorf("lock the run's invoice: %w", err)
	}
	inv.StripePaymentIntentID = pi.String
	if !inv.PaymentInFlight() || inv.StripePaymentIntentID != paymentIntentID {
		return RetryInFlightPaymentMoved, nil
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE invoice_dunning_runs SET state=$1, reason=$2, attempt_count=$3,
			last_attempt_at=$4, next_action_at=$5, paused=$6, resolved_at=$7, resolution=$8, updated_at=$9,
			awaiting_payment_intent_id = $12
		WHERE id=$10 AND state = 'active' AND attempt_count = $11`,
		run.State, postgres.NullableString(run.Reason), run.AttemptCount,
		postgres.NullableTime(run.LastAttemptAt), postgres.NullableTime(run.NextActionAt),
		run.Paused, postgres.NullableTime(run.ResolvedAt), postgres.NullableString(string(run.Resolution)),
		clock.Now(ctx), run.ID, expectedAttempts, paymentIntentID)
	if err != nil {
		return RetryInFlightRunMoved, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return RetryInFlightRunMoved, err
	}
	if n == 0 {
		return RetryInFlightRunMoved, nil
	}
	if _, err := s.createEventTx(ctx, tx, tenantID, event); err != nil {
		return RetryInFlightRunMoved, fmt.Errorf("retry in flight event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RetryInFlightRunMoved, err
	}
	return RetryInFlightRecorded, nil
}

// GetRunAwaitingPayment returns the active run waiting on paymentIntentID, or
// errs.ErrNotFound. No production path decides on it (the failure hook's
// compare-and-set does, under the invoice lock); the integration tests read
// the wait with it.
func (s *PostgresStore) GetRunAwaitingPayment(ctx context.Context, tenantID, invoiceID, paymentIntentID string) (domain.InvoiceDunningRun, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningRun{}, err
	}
	defer postgres.Rollback(tx)

	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+`
		FROM invoice_dunning_runs
		WHERE invoice_id = $1 AND awaiting_payment_intent_id = $2 AND state = 'active'`,
		invoiceID, paymentIntentID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.InvoiceDunningRun{}, errs.ErrNotFound
	}
	return run, err
}

// TakeAwaitedRetryTx clears the run's awaited PaymentIntent, only while it is
// paymentIntentID and the run is active, and returns the run and the policy
// for its mode when this call did. The UPDATE is the compare-and-set: the webhook,
// a redelivery and the reconciler can each report the failure, and one wins.
// The caller's transaction locks the invoice row before this one (the
// failure settle's FOR UPDATE), so the lock order matches RecordRetryInFlight.
func (s *PostgresStore) TakeAwaitedRetryTx(ctx context.Context, tx *sql.Tx, invoiceID, paymentIntentID string) (domain.InvoiceDunningRun, domain.DunningPolicy, bool, error) {
	if paymentIntentID == "" {
		return domain.InvoiceDunningRun{}, domain.DunningPolicy{}, false, nil
	}
	run, err := scanRun(tx.QueryRowContext(ctx, `
		UPDATE invoice_dunning_runs SET awaiting_payment_intent_id = NULL, updated_at = $3
		WHERE invoice_id = $1 AND awaiting_payment_intent_id = $2 AND state = 'active'
		RETURNING `+runColumns,
		invoiceID, paymentIntentID, clock.Now(ctx)))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.InvoiceDunningRun{}, domain.DunningPolicy{}, false, nil
	}
	if err != nil {
		return domain.InvoiceDunningRun{}, domain.DunningPolicy{}, false, fmt.Errorf("take the run waiting on the payment: %w", err)
	}
	// The run's own tenant and mode pick the policy, so the read does not
	// depend on how the caller's transaction is scoped.
	policy, err := scanPolicy(tx.QueryRowContext(ctx, `SELECT `+policyColumns+` FROM dunning_policies
		WHERE (tenant_id, livemode) = (SELECT tenant_id, livemode FROM invoice_dunning_runs WHERE id = $1)`, run.ID))
	if err != nil {
		return domain.InvoiceDunningRun{}, domain.DunningPolicy{}, false, fmt.Errorf("read the dunning policy for run %s: %w", run.ID, err)
	}
	return run, policy, true, nil
}

// RecordRetryFailureTx writes the run's next_action_at, still active at its
// attempt, and the failed attempt's event on the caller's transaction, then
// runs then under a SAVEPOINT: an email-side failure is logged and skipped,
// never the run write.
func (s *PostgresStore) RecordRetryFailureTx(ctx context.Context, tx *sql.Tx, run domain.InvoiceDunningRun, event domain.InvoiceDunningEvent, then func(tx *sql.Tx) error) error {
	if run.NextActionAt == nil { // the active run would leave both due-run pickers for good (SB-1)
		return fmt.Errorf("dunning run %s: refusing to write an active run with no next_action_at", run.ID)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE invoice_dunning_runs SET next_action_at = $1, updated_at = $2
		WHERE id = $3 AND state = 'active' AND attempt_count = $4`,
		*run.NextActionAt, clock.Now(ctx), run.ID, run.AttemptCount)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 { // unreachable: TakeAwaitedRetryTx holds the row's lock in this transaction
		return fmt.Errorf("dunning run %s changed under its own row lock", run.ID)
	}
	if _, err := s.createEventTx(ctx, tx, run.TenantID, event); err != nil {
		return fmt.Errorf("retry failed event: %w", err)
	}
	if then == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT dunning_retry_email"); err != nil {
		return err
	}
	if herr := then(tx); herr != nil {
		if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT dunning_retry_email"); rerr != nil {
			return rerr
		}
		slog.ErrorContext(ctx, "dunning warning email enqueue failed inside the failure settle — the failure is recorded, the email skipped",
			"run_id", run.ID, "invoice_id", run.InvoiceID, "error", herr)
		return nil
	}
	_, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT dunning_retry_email")
	return err
}

// ListDueRuns lists the runs whose next action is due, for the cron path.
func (s *PostgresStore) ListDueRuns(ctx context.Context, tenantID string, dueBefore time.Time, limit int) ([]domain.InvoiceDunningRun, error) {
	// Leader-only funnel (ADR-114): see subscription.GetDueBilling.
	token, err := leader.Fence(ctx, leader.RoleDunning)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 20
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, r.tenant_id, r.invoice_id, COALESCE(r.customer_id,''), r.state,
			COALESCE(r.reason,''), r.attempt_count, r.last_attempt_at, r.next_action_at,
			r.paused, r.resolved_at, COALESCE(r.resolution,''), r.created_at, r.updated_at
		FROM invoice_dunning_runs r
		WHERE r.next_action_at <= $1 AND r.paused = false
			-- Mode isolation is RLS's job here, on purpose: this query runs
			-- under TxTenant, whose row policy already scopes livemode = GUC.
			-- A 2026-08-02 walk briefly "found" cross-mode selection and added
			-- an explicit predicate — the observation was a malformed fixture
			-- (a bypass-inserted live-mode run on a test invoice), and the
			-- predicate was provably redundant: its mutation survived the RLS
			-- isolation test. The #13-class explicit clauses belong on
			-- TxBypass queries, where RLS does NOT apply; adding one here
			-- would be belt-and-suspenders asserting a guarantee RLS owns.
			-- The property is pinned by TestListDueRuns_LivemodeIsolation.
			AND r.state NOT IN ('resolved', 'escalated')
			-- Payment in flight. ClaimChargeForDunningRetry requires
			-- payment_status IN ('pending','failed'), so while the payment is
			-- 'processing' or 'unknown' the claim CANNOT succeed: the adapter
			-- returns ErrTransientSkip and the transient handler deliberately
			-- does NOT reschedule (a Stripe blip must not burn a tenant's retry
			-- budget). Selecting the run is therefore provably wasted work — it
			-- writes an attempt and immediately rewinds it, every tick, for as
			-- long as the payment stays in flight.
			--
			-- Worse than the waste: next_action_at is frozen while that happens
			-- and this scan is next_action_at ASC with LIMIT, so these runs
			-- migrate to the HEAD of the queue and stay there — at LIMIT of them
			-- real dunning stops for everyone. Same starvation shape ADR-107 hit
			-- in ListUnknownPayments, same cause (oldest-first over rows that do
			-- not move).
			--
			-- WHY THE PREDICATE WIDENED (2026-07-31). It first shipped keyed on
			-- the ADR-107 PARKED shape ('unknown' with no PaymentIntent id) and
			-- carried a comment claiming a 'processing' charge "does resolve on
			-- its own, so skipping it is correct". Both halves were wrong. The
			-- reconciler settles only TERMINAL provider outcomes — reconciler.go
			-- skips processing/requires_action/requires_confirmation/
			-- requires_capture every sweep — so an 'unknown' invoice WITH an id
			-- whose PI sits at requires_action (a PI stalled non-terminal that
			-- nothing advances; NOT "off-session SCA", which raises the
			-- authentication_required DECLINE instead — phrase retracted
			-- 2026-07-31, see ADR-107) never resolves either, and spun exactly
			-- like a parked one. Drawing
			-- the line at "do we hold an id" answered the wrong question; the
			-- right one is "can this claim succeed", and the claim's own
			-- predicate answers it for every in-flight shape at once. This is
			-- domain.IsInFlight expressed in SQL, which is also why it needs no
			-- special case: when the payment leaves flight the row is selectable
			-- again on the very next tick.
			--
			-- SCOPED TO OPEN INVOICES ON PURPOSE. A voided/uncollectible source
			-- must STILL be selected: the processing path's own terminal branch
			-- (service.go, "invoice_"+status) is what resolves the run, and
			-- markUncollectible's ResolveByInvoice is explicitly best-effort,
			-- documented as relying on "dunning runs scan the invoice status on
			-- next tick anyway". Excluding terminal rows here would delete that
			-- backstop and strand the run active forever — the exact bug this
			-- predicate's twin in creditnote/postgres.go was fixed for.
			--
			-- A live hosted Checkout page blocks the claim the same way (one
			-- payer at a time, domain.HostedCheckoutLiveSQL), so it is part of
			-- the same exclusion.
			--
			-- So does a set charge hold (ADR-127): a charge running, or one
			-- whose result was lost and waits for the stale-hold sweep. The
			-- claim refuses it, and so does exhaustRun's final-action claim,
			-- so a held run is not picked, not rewound and does not pin the
			-- head of this LIMITed queue. It is picked on the first tick after
			-- the hold clears.
			AND NOT EXISTS (
			  SELECT 1 FROM invoices i
			  WHERE i.id = r.invoice_id
			    AND NOT `+domain.ChargeNotInFlightSQL("i")+`
			    AND i.status NOT IN ('voided', 'uncollectible')
			)
		  AND leader_fence($3, $4)
		ORDER BY r.next_action_at ASC LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, dueBefore, limit, string(leader.RoleDunning), token)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var runs []domain.InvoiceDunningRun
	for rows.Next() {
		var r domain.InvoiceDunningRun
		if err := rows.Scan(&r.ID, &r.TenantID, &r.InvoiceID, &r.CustomerID,
			&r.State, &r.Reason, &r.AttemptCount, &r.LastAttemptAt, &r.NextActionAt,
			&r.Paused, &r.ResolvedAt, &r.Resolution, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *PostgresStore) CreateEvent(ctx context.Context, tenantID string, event domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.InvoiceDunningEvent{}, err
	}
	defer postgres.Rollback(tx)

	event, err = s.createEventTx(ctx, tx, tenantID, event)
	if err != nil {
		return domain.InvoiceDunningEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.InvoiceDunningEvent{}, err
	}
	return event, nil
}

func (s *PostgresStore) createEventTx(ctx context.Context, tx *sql.Tx, tenantID string, event domain.InvoiceDunningEvent) (domain.InvoiceDunningEvent, error) {
	id := postgres.NewID("hc_devt")
	// Honor caller-supplied CreatedAt so each event row carries the
	// instant its fact occurred (an escalation re-driven after an
	// interruption keeps its final retry's instant). Zero means
	// clock.Now(ctx).
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = clock.Now(ctx)
	}
	metaJSON, _ := json.Marshal(event.Metadata)
	if event.Metadata == nil {
		metaJSON = []byte("{}")
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_dunning_events (id, run_id, tenant_id, invoice_id,
			event_type, state, reason, attempt_count, metadata, created_at, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
	`, id, event.RunID, tenantID, event.InvoiceID, event.EventType, event.State,
		postgres.NullableString(event.Reason), event.AttemptCount, metaJSON, createdAt)
	if err != nil {
		return domain.InvoiceDunningEvent{}, err
	}
	event.ID = id
	event.TenantID = tenantID
	event.CreatedAt = createdAt
	return event, nil
}

func (s *PostgresStore) ListEvents(ctx context.Context, tenantID, runID string) ([]domain.InvoiceDunningEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, run_id, tenant_id, invoice_id, event_type, state,
			COALESCE(reason,''), attempt_count, metadata, created_at, recorded_at
		FROM invoice_dunning_events WHERE run_id = $1
		ORDER BY created_at ASC, id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []domain.InvoiceDunningEvent
	for rows.Next() {
		var e domain.InvoiceDunningEvent
		var metaJSON []byte
		if err := rows.Scan(&e.ID, &e.RunID, &e.TenantID, &e.InvoiceID,
			&e.EventType, &e.State, &e.Reason, &e.AttemptCount, &metaJSON, &e.CreatedAt, &e.RecordedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaJSON, &e.Metadata)
		events = append(events, e)
	}
	return events, rows.Err()
}

// GetStats computes the dashboard-card payload in one round trip:
// COUNT(*) GROUP BY state for the three states we surface (active,
// escalated, resolved), the resolved runs a payment closed (recovered),
// and SUM(amount_due_cents) on the OPEN invoices behind active+escalated
// runs (the at-risk total).
//
// Both filters exist because a run outlives its invoice's collection. A void
// keeps its amount_due, and a write-off final action leaves the run escalated,
// so without i.status = 'finalized' a voided or written-off invoice stayed
// "at risk"; and a run resolved by a void or a write-off is resolved, not
// recovered (sweep A6/E7, 2026-10-21).
//
// Recovered also needs money paid on the invoice (amount_paid_cents > 0). A
// credit note or customer credit that leaves nothing due marks the invoice
// paid with amount_paid 0 and closes its run as payment_recovered too
// (ADR-061, 2026-10-19), from the note, the dunning retry or the paid check;
// the resolution alone cannot tell a settle with no payment from a payment
// (review of #1328). Credit is not a payment, even credit the customer bought.
//
// Single tenant-scoped query (RLS handles tenant_id), no pagination,
// no client-side derivation. Cards stay accurate regardless of how
// many runs exist.
func (s *PostgresStore) GetStats(ctx context.Context, tenantID string) (Stats, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return Stats{}, err
	}
	defer postgres.Rollback(tx)

	// Resolve the tenant's default currency once. The at-risk total is scoped
	// to it so we never sum amount_due across currencies into a corrupt figure
	// (a EUR invoice's cents are not a USD invoice's cents). Defaults to USD
	// when settings are unset.
	defaultCurrency := "USD"
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(default_currency, ''), 'USD') FROM tenant_settings LIMIT 1`,
	).Scan(&defaultCurrency); err != nil && err != sql.ErrNoRows {
		return Stats{}, err
	}

	var stats Stats
	stats.Currency = defaultCurrency
	err = tx.QueryRowContext(ctx, `
		SELECT
		    COUNT(*) FILTER (WHERE r.state = 'active')                                AS active_count,
		    COUNT(*) FILTER (WHERE r.state = 'escalated')                             AS escalated_count,
		    COUNT(*) FILTER (WHERE r.state = 'resolved')                              AS resolved_count,
		    COUNT(*) FILTER (WHERE r.state = 'resolved' AND r.resolution = $2 AND i.amount_paid_cents > 0) AS recovered_count,
		    COALESCE(SUM(i.amount_due_cents) FILTER (WHERE r.state IN ('active','escalated') AND i.status = $3 AND i.currency = $1), 0) AS at_risk_cents
		FROM invoice_dunning_runs r
		LEFT JOIN invoices i ON i.id = r.invoice_id
	`, defaultCurrency, string(domain.ResolutionPaymentRecovered), string(domain.InvoiceFinalized)).Scan(
		&stats.ActiveCount, &stats.EscalatedCount, &stats.ResolvedCount, &stats.RecoveredCount, &stats.AtRiskCents)
	if err != nil {
		return Stats{}, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT i.currency, SUM(i.amount_due_cents)
		FROM invoice_dunning_runs r
		JOIN invoices i ON i.id = r.invoice_id
		WHERE r.state IN ('active','escalated') AND i.status = $2
		GROUP BY i.currency
		HAVING SUM(i.amount_due_cents) > 0
		ORDER BY (i.currency = $1) DESC, SUM(i.amount_due_cents) DESC, i.currency
	`, defaultCurrency, string(domain.InvoiceFinalized))
	if err != nil {
		return Stats{}, err
	}
	defer func() { _ = rows.Close() }()
	stats.AtRiskByCurrency = []CurrencyAmount{}
	for rows.Next() {
		var a CurrencyAmount
		if err := rows.Scan(&a.Currency, &a.AmountCents); err != nil {
			return Stats{}, err
		}
		stats.AtRiskByCurrency = append(stats.AtRiskByCurrency, a)
	}
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}
	return stats, nil
}
