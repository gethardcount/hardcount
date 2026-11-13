package testclock

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

const clockCols = `id, tenant_id, name, frozen_time, status,
	created_at, updated_at,
	COALESCE(last_failure_reason,''), last_advance_summary`

func (s *PostgresStore) Create(ctx context.Context, tenantID string, clk domain.TestClock, auditRow AuditRow) (domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx, `
		INSERT INTO test_clocks (tenant_id, name, frozen_time, status)
		VALUES ($1, $2, $3, 'ready')
		RETURNING `+clockCols,
		tenantID, clk.Name, clk.FrozenTime,
	).Scan(scanDest(&clk)...)
	if err != nil {
		// 23514 = check_violation; the livemode CHECK rejects any attempt to
		// insert a test clock from a live-mode session. Surface as 400 invalid
		// instead of leaking a raw SQL error string.
		if postgres.IsCheckViolation(err) {
			return domain.TestClock{}, errs.Invalid("livemode", "test clocks can only be created in test mode")
		}
		return domain.TestClock{}, err
	}
	if err := auditRow(tx, clk); err != nil {
		return domain.TestClock{}, fmt.Errorf("audit emission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.TestClock{}, err
	}
	return clk, nil
}

// ResolveSim answers "is this clock attached-able, and what instant does it
// stand at?" in ONE read — used by the customer service at customer-create
// time, which must both validate the pin and bind the new customer's audit row
// onto the sim axis (ADR-090 §5).
//
// It returns the pair from a single Get precisely so the two halves cannot
// drift: an "exists?" bool would send the caller back for the instant, and a
// clock id paired with a separately-fetched instant is the half-truth
// clock.Sim is designed to make unrepresentable.
//
// ErrNotFound → (zero, false, nil); other errors propagate. Soft-deleted clocks
// count as not-existing. A clock in any status is attach-able: an advancing
// or failed clock's frozen_time is already its new time, so the customer
// joins at it (domain.TestClockStatus says why no status blocks this).
func (s *PostgresStore) ResolveSim(ctx context.Context, tenantID, id string) (clock.Sim, bool, error) {
	clk, err := s.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return clock.Sim{}, false, nil
		}
		return clock.Sim{}, false, err
	}
	return clock.Sim{At: clk.FrozenTime, TestClockID: clk.ID}, true, nil
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	var clk domain.TestClock
	err = tx.QueryRowContext(ctx, `SELECT `+clockCols+` FROM test_clocks WHERE id = $1`, id).
		Scan(scanDest(&clk)...)
	if err == sql.ErrNoRows {
		return domain.TestClock{}, errs.ErrNotFound
	}
	return clk, err
}

// List returns one page of the tenant's test clocks, newest first, and how
// many clocks the tenant has.
func (s *PostgresStore) List(ctx context.Context, tenantID string, limit, offset int) ([]domain.TestClock, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM test_clocks`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT `+clockCols+` FROM test_clocks
		ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var clocks []domain.TestClock
	for rows.Next() {
		var clk domain.TestClock
		if err := rows.Scan(scanDest(&clk)...); err != nil {
			return nil, 0, err
		}
		clocks = append(clocks, clk)
	}
	return clocks, total, rows.Err()
}

// clockTeardownStatements are the ordered DELETEs that tear down a test
// clock's ENTIRE simulated customer graph (ADR-086, Design B). Order is
// bottom-up so the customer_id / invoice_id RESTRICT FKs (migration 0015)
// never trip — every child is gone before its parent. Every statement is
// transitively keyed on the clock's customer set `test_clock_id = $1`, so
// only livemode=false rows are ever reachable (a live customer has
// test_clock_id NULL; a test clock is always livemode=false by CHECK).
//
// Tables with ON DELETE CASCADE from a parent (subscription_items /
// _changes / _thresholds via subscriptions; payment_methods / portal via
// customers) would be removed automatically, but the payment_methods and
// portal rows are deleted explicitly so this list is the complete,
// self-documenting record the completeness arch-test asserts against.
//
// NOT torn down (survivors): the audit log and all tenant config — meters,
// plans and their prices, dunning_policies, api_keys,
// stripe_webhook_events. Keep this list in sync with
// TestTeardownCoversEverySimulatedTable.
var clockTeardownStatements = []string{
	// Invoice sub-graph — children first (leaves have no customer_id; keyed via parent).
	`DELETE FROM invoice_dunning_events WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM invoice_dunning_runs   WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM tax_calculations       WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	// Refunds made in Stripe (ADR-134) go before the credit notes: a recorded
	// one names its note (credit_note_id, ON DELETE RESTRICT). They would
	// cascade with their invoice, but the notes are deleted first.
	`DELETE FROM invoice_stripe_refunds WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM credit_note_line_items WHERE credit_note_id IN (SELECT id FROM credit_notes WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM credit_notes           WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM invoice_line_items     WHERE invoice_id IN (SELECT id FROM invoices WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1))`,
	`DELETE FROM payment_update_tokens  WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM invoices               WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// Usage + credit — the ledger MUST precede subscriptions: its
	// source_subscription_item_id RESTRICT-references subscription_items,
	// which cascades away when subscriptions is deleted.
	`DELETE FROM usage_events           WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_credit_ledger WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// Subscriptions — CASCADE removes subscription_items / _changes /
	// _thresholds / billing_intervals (ADR-101; the intervals FK is the
	// ADR-086 teardown carve-out).
	`DELETE FROM subscriptions          WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// Per-customer config + portal (payment_methods / portal also ON
	// DELETE CASCADE from the customer; deleted explicitly so
	// the set is the complete self-documenting record the arch-test asserts).
	`DELETE FROM customer_price_overrides    WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// The clock's stamps on plan price versions (ADR-130 §7). The plan, its
	// prices and their versions survive: they are tenant config.
	`DELETE FROM plan_price_version_clocks WHERE test_clock_id = $1`,
	`DELETE FROM customer_billing_profiles   WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM payment_methods             WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_portal_sessions    WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	`DELETE FROM customer_portal_magic_links WHERE customer_id IN (SELECT id FROM customers WHERE test_clock_id = $1)`,
	// Clock-anchored emails (ADR-104): outbox rows carry the clock id
	// directly (their invoices are keyed by number in jsonb, not FK, so the
	// invoice-graph deletes can't reach them). A surviving row would keep a
	// deleted simulation's email visible on the customer page and on any
	// re-used invoice number — the "simulation must be fully disposable"
	// half of the operator contract.
	`DELETE FROM email_outbox WHERE test_clock_id = $1`,
	// The customers themselves LAST (before the clock): customers.test_clock_id
	// is ON DELETE SET NULL, so deleting the clock first would silently detach
	// every customer and leak the whole set.
	`DELETE FROM customers WHERE test_clock_id = $1`,
}

// Delete tears down a test clock and its ENTIRE simulated customer graph in
// one transaction (ADR-086, Design B — complete teardown, supersedes ADR-016's
// soft-delete + detach). After it runs no simulated row survives, so no
// wall-clock plane (auto-charge, dunning, usage-aggregation, credit-expiry,
// analytics) can ever act on one — the class the ADR dissolves.
//
// Safety: see clockTeardownStatements — every DELETE is keyed transitively on
// `test_clock_id = $clock`, and a test clock is always livemode=false, so no
// live-mode row is reachable; the clock row's own DELETE adds `livemode=false`
// as belt. Runs under TxTenant (RLS confines it to the tenant).
//
// Idempotent: re-deleting a gone clock returns errs.ErrNotFound.
func (s *PostgresStore) Delete(ctx context.Context, tenantID, id string, auditRow AuditRow) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// The clock row first, FOR UPDATE (ADR-130 §8). Teardown deletes
	// bottom-up for the foreign keys, outside the lock order every other
	// writer follows; a price write or override write reads this clock FOR
	// SHARE before it touches a row teardown deletes (the clock's stamps, an
	// override of its customer), and a subscription create reads it FOR KEY
	// SHARE before its insert locks the customer, so one of the two waits for
	// the other here and they cannot deadlock. Existence and the row the
	// audit row describes come from the same read: tearing down a gone clock
	// hits an empty customer set (harmless), but the contract is ErrNotFound.
	var deleted domain.TestClock
	err = tx.QueryRowContext(ctx, `SELECT `+clockCols+` FROM test_clocks WHERE id = $1 FOR UPDATE`, id).
		Scan(scanDest(&deleted)...)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock test clock: %w", err)
	}

	for i, stmt := range clockTeardownStatements {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("clock teardown step %d: %w", i, err)
		}
	}

	// The clock row itself, last — its customers are already gone, so the
	// ON DELETE SET NULL fires on nothing. Guarded livemode=false.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM test_clocks WHERE id = $1 AND livemode = false`, id); err != nil {
		return fmt.Errorf("delete test clock: %w", err)
	}
	if err := auditRow(tx, deleted); err != nil {
		return fmt.Errorf("audit emission: %w", err)
	}

	return tx.Commit()
}

func (s *PostgresStore) MarkAdvancing(ctx context.Context, tenantID, id string, newFrozenTime time.Time, auditRow AuditRow) (domain.TestClock, error) {
	// The summary restarts with the time the advance starts from, read off
	// the row before this statement changes it (the right-hand side of SET
	// sees the old row). The catch-up reads it back from the clock, so a
	// Retry and a run another replica picks up keep it.
	return s.transition(ctx, tenantID, id, auditRow, "can only advance a clock in status=ready", `
		UPDATE test_clocks SET status = 'advancing', frozen_time = $2,
			last_advance_summary = jsonb_build_object('advanced_from', frozen_time),
			updated_at = now()
		WHERE id = $1 AND status = 'ready'
		RETURNING `+clockCols, id, newFrozenTime)
}

// CompleteAdvance flips advancing → ready and clears any prior
// last_failure_reason. The clear matters when the operator
// retried a failed advance and it succeeded — the dashboard
// shouldn't keep showing yesterday's error. It writes the advance's summary
// in the same statement. A catch-up write: fenced on the testclock_catchup
// lease (fencedMiss gives its errors).
func (s *PostgresStore) CompleteAdvance(ctx context.Context, tenantID, id string, summary domain.AdvanceSummary) (domain.TestClock, error) {
	return s.transitionWithReason(ctx, tenantID, id, "ready", "", true, summary)
}

// MarkFailed flips advancing → internal_failure and persists the
// reason, cut to about 500 bytes between two characters; the full
// payload stays in structured slog. It writes the advance's summary so far
// in the same statement. A catch-up write: fenced on the testclock_catchup
// lease (fencedMiss gives its errors).
func (s *PostgresStore) MarkFailed(ctx context.Context, tenantID, id, reason string, summary domain.AdvanceSummary) (domain.TestClock, error) {
	return s.transitionWithReason(ctx, tenantID, id, "internal_failure", reason, false, summary)
}

// RetryFromFailed flips internal_failure → advancing, clearing
// the failure reason. Frozen_time stays at its current value —
// the catchup loop is idempotent on subs whose next_billing_at
// has already passed it. ADR-018.
func (s *PostgresStore) RetryFromFailed(ctx context.Context, tenantID, id string, auditRow AuditRow) (domain.TestClock, error) {
	return s.transition(ctx, tenantID, id, auditRow, "can only retry from status=internal_failure", `
		UPDATE test_clocks SET status = 'advancing', last_failure_reason = NULL, updated_at = now()
		WHERE id = $1 AND status = 'internal_failure'
		RETURNING `+clockCols, id)
}

// transitionWithReason is the catch-up's end of an advance: it flips the
// clock from advancing to `to` and either writes or clears
// last_failure_reason. clearReason=true means SET last_failure_reason =
// NULL; false means SET last_failure_reason = $reason. Used by
// CompleteAdvance and MarkFailed. The summary is stored in the same
// statement, and the statement carries the testclock_catchup lease's fence
// (ADR-140), so a write that changes no row is reported by fencedMiss.
func (s *PostgresStore) transitionWithReason(
	ctx context.Context, tenantID, id, to, reason string, clearReason bool, summary domain.AdvanceSummary,
) (domain.TestClock, error) {
	token, err := leader.Fence(ctx, leader.RoleTestClockCatchup)
	if err != nil {
		return domain.TestClock{}, err
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	set := `last_failure_reason = NULL`
	args := []any{to, id}
	if !clearReason {
		// The dashboard shows the reason in one panel: keep 500 bytes, or
		// 499 and "…" when the reason is longer. The full text is in the log.
		stored := errs.Clip(reason, 500)
		if len(reason) > 500 {
			stored = errs.Clip(reason, 499) + "…"
		}
		args = append(args, stored)
		set = fmt.Sprintf("last_failure_reason = $%d", len(args))
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return domain.TestClock{}, fmt.Errorf("encode advance summary: %w", err)
	}
	args = append(args, b)
	set += fmt.Sprintf(", last_advance_summary = $%d::jsonb", len(args))
	args = append(args, string(leader.RoleTestClockCatchup), token)
	fence := fmt.Sprintf("leader_fence($%d, $%d)", len(args)-1, len(args))
	query := `UPDATE test_clocks SET status = $1, ` + set + `, updated_at = now()
		WHERE id = $2 AND status = 'advancing' AND ` + fence + `
		RETURNING ` + clockCols

	var clk domain.TestClock
	err = tx.QueryRowContext(ctx, query, args...).Scan(scanDest(&clk)...)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TestClock{}, fencedMiss(ctx, tx, id)
	}
	if err != nil {
		return domain.TestClock{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TestClock{}, err
	}
	return clk, nil
}

// fencedMiss explains a fenced catch-up write that changed no row. A clock
// that is gone, or no longer advancing (another run completed or failed it,
// or the tenant deleted it), gives ErrClockNoLongerAdvancing. A clock still
// advancing means the fence failed: this run no longer holds the
// testclock_catchup lease, and it gives ErrCatchupFenceLost. Neither is a
// failure of the advance; the run stops and leaves the clock as it is.
func fencedMiss(ctx context.Context, tx *sql.Tx, id string) error {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT status FROM test_clocks WHERE id = $1`, id).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: the clock is gone", ErrClockNoLongerAdvancing)
	case err != nil:
		return err
	case current != string(domain.TestClockStatusAdvancing):
		return fmt.Errorf("%w: the clock is %s", ErrClockNoLongerAdvancing, current)
	default:
		return ErrCatchupFenceLost
	}
}

// transition is the operator's status change: query is one UPDATE … WHERE
// id = $1 AND status = <the status it may come from>, returning the row
// (a CAS). The change's audit row is written on the same transaction, so
// neither is saved without the other. Zero rows back is ambiguous (missing
// row vs wrong state), so a second lookup distinguishes.
func (s *PostgresStore) transition(ctx context.Context, tenantID, id string, auditRow AuditRow, wrongStateMsg, query string, args ...any) (domain.TestClock, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	var clk domain.TestClock
	err = tx.QueryRowContext(ctx, query, args...).Scan(scanDest(&clk)...)
	if err == sql.ErrNoRows {
		var current string
		err2 := tx.QueryRowContext(ctx, `SELECT status FROM test_clocks WHERE id = $1`, id).Scan(&current)
		if err2 == sql.ErrNoRows {
			return domain.TestClock{}, errs.ErrNotFound
		}
		if err2 != nil {
			return domain.TestClock{}, err2
		}
		return domain.TestClock{}, errs.InvalidState(fmt.Sprintf("%s, current status: %s", wrongStateMsg, current))
	}
	if err != nil {
		return domain.TestClock{}, err
	}
	if err := auditRow(tx, clk); err != nil {
		return domain.TestClock{}, fmt.Errorf("audit emission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.TestClock{}, err
	}
	return clk, nil
}

// ListAdvancingForCatchup is the testclock_catchup role's claim list
// (ADR-140, ADR-114): the clocks in status='advancing' across every tenant,
// the one whose pass started longest ago first, at most limit. A pass starts
// with TouchAdvancing, so a clock whose pass was cut off goes to the back.
// A pass runs to its end, so a long one still holds up the clocks behind it;
// fairness between clocks comes with the stepped catch-up. The statement
// proves the lease's fence, so a deposed leader lists nothing.
// RLS-bypassed (TxBypass): the tick runs for every tenant, before any tenant
// ctx exists.
func (s *PostgresStore) ListAdvancingForCatchup(ctx context.Context, limit int) ([]domain.TestClock, error) {
	token, err := leader.Fence(ctx, leader.RoleTestClockCatchup)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+clockCols+` FROM test_clocks
		WHERE status = 'advancing'
		  AND leader_fence($1, $2)
		ORDER BY updated_at ASC, id ASC
		LIMIT $3
	`, string(leader.RoleTestClockCatchup), token, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var clocks []domain.TestClock
	for rows.Next() {
		var clk domain.TestClock
		if err := rows.Scan(scanDest(&clk)...); err != nil {
			return nil, err
		}
		clocks = append(clocks, clk)
	}
	return clocks, rows.Err()
}

func scanDest(c *domain.TestClock) []any {
	return []any{
		&c.ID, &c.TenantID, &c.Name, &c.FrozenTime, &c.Status,
		&c.CreatedAt, &c.UpdatedAt,
		&c.LastFailureReason, advanceSummaryScan{&c.LastAdvanceSummary},
	}
}

// advanceSummaryScan adapts the nullable last_advance_summary JSONB column to
// the *domain.AdvanceSummary field: NULL (or empty) → nil, otherwise unmarshal.
type advanceSummaryScan struct{ dst **domain.AdvanceSummary }

func (a advanceSummaryScan) Scan(src any) error {
	if src == nil {
		*a.dst = nil
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("last_advance_summary: unexpected scan type %T", src)
	}
	if len(b) == 0 {
		*a.dst = nil
		return nil
	}
	var s domain.AdvanceSummary
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("decode last_advance_summary: %w", err)
	}
	*a.dst = &s
	return nil
}

// TouchAdvancing starts a catch-up pass: it moves the clock's updated_at to
// now, which puts it at the back of the claim list (ListAdvancingForCatchup
// orders by updated_at), so a clock whose pass keeps being cut off does not
// hold up the others. A catch-up write: fenced on the testclock_catchup
// lease and on the clock still advancing (fencedMiss gives its errors).
func (s *PostgresStore) TouchAdvancing(ctx context.Context, tenantID, id string) error {
	token, err := leader.Fence(ctx, leader.RoleTestClockCatchup)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx,
		`UPDATE test_clocks SET updated_at = now()
		 WHERE id = $1 AND status = 'advancing' AND leader_fence($2, $3)`,
		id, string(leader.RoleTestClockCatchup), token,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fencedMiss(ctx, tx, id)
	}
	return tx.Commit()
}
