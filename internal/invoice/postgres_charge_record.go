package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// The charge record (ADR-127, #976). Just before Velox calls Stripe, the
// charge writes four columns on the invoice in a commit of its own: when, the
// charge_attempt_seq its idempotency key was built from, the exact key, and
// the Stripe customer. While they are set no charge path claims the invoice
// (chargeNotHeldSQL) and Invoice.PaymentInFlight is true. The charge clears
// them once its result is saved on the invoice, or when nothing was sent. A
// crash, a deploy or a deadline that ends during the Stripe call leaves them,
// and the reconciler's interrupted-charge check asks Stripe about it.
//
// Every write below is a CAS. The record's own columns are its only writers;
// no other write in this store touches them.

// BeginChargeAttempt writes the charge record, and only for a charge that may
// still call Stripe: no record is there yet, the seq is the one the charge
// read (no outcome was saved since), the invoice is still finalized and
// pending or failed, and the charge hold is live — this charger still holds
// the invoice, right before the call that cannot be undone. That last term
// closes the "charger stalled past five minutes" gap: a charger whose hold
// lapsed, so that another may hold the invoice now, gets false and must not
// call Stripe. now() is the database's clock, as for the hold.
func (s *PostgresStore) BeginChargeAttempt(ctx context.Context, tenantID, id string, seq int64, key, stripeCustomerID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE invoices
		SET charge_started_at = now(), charge_started_seq = $2,
		    charge_started_key = $3, charge_started_stripe_customer = $4
		WHERE id = $1
		  AND charge_started_at IS NULL
		  AND charge_attempt_seq = $2
		  AND status = 'finalized'
		  AND payment_status IN ('pending', 'failed')
	`, id, seq, key, stripeCustomerID)
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

// EndChargeAttempt clears the charge record of attempt seq. The charge calls
// it once the attempt's result is saved on the invoice, or when nothing was
// sent; the interrupted-charge check, once it has saved what Stripe said.
// false = no record of that attempt was there.
func (s *PostgresStore) EndChargeAttempt(ctx context.Context, tenantID, id string, seq int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE invoices
		SET charge_started_at = NULL, charge_started_seq = NULL,
		    charge_started_key = NULL, charge_started_stripe_customer = NULL
		WHERE id = $1 AND charge_started_seq = $2
	`, id, seq)
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

// ListInterruptedCharges is the interrupted-charge check's input: this mode's
// charge records older than minAge whose last failed check (provider_synced_at,
// stamped by RecordProviderSync) is older than minAge too, never-checked
// first, then oldest first. Both ages are measured with the database's now(),
// the clock the record was written with. PastGiveUp marks a record older than
// giveUpAge.
//
// TxBypass crosses tenants, as the reconciler's other sweeps do; livemode is
// explicit, so a test-mode record is never checked with live keys (#13). Not
// gated on is_simulated: Stripe's truth is on the wall clock, and a charge
// interrupted during a test-clock advance must be checked too. The partial
// index idx_invoices_charge_started (migration 00187) holds only invoices
// with a record.
func (s *PostgresStore) ListInterruptedCharges(ctx context.Context, minAge, giveUpAge time.Duration, limit int) ([]domain.InterruptedCharge, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT tenant_id, id, charge_started_at, charge_started_seq, charge_started_key,
		       charge_started_stripe_customer,
		       charge_started_at < now() - make_interval(secs => $3) AS past_give_up
		FROM invoices
		WHERE charge_started_at IS NOT NULL
		  AND livemode = $1
		  AND charge_started_at < now() - make_interval(secs => $2)
		  AND (provider_synced_at IS NULL OR provider_synced_at < now() - make_interval(secs => $2))
		ORDER BY provider_synced_at ASC NULLS FIRST, charge_started_at ASC
		LIMIT $4
	`, postgres.Livemode(ctx), minAge.Seconds(), giveUpAge.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.InterruptedCharge
	for rows.Next() {
		var c domain.InterruptedCharge
		if err := rows.Scan(&c.TenantID, &c.InvoiceID, &c.StartedAt, &c.Seq, &c.Key, &c.StripeCustomerID, &c.PastGiveUp); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// lockChargeRecordTx locks the invoice row holding exactly this record (seq
// and start time) and returns its charge_attempt_seq. found=false: the record
// changed or cleared since it was listed.
func lockChargeRecordTx(ctx context.Context, tx *sql.Tx, id string, seq int64, startedAt time.Time) (attemptSeq int64, found bool, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT charge_attempt_seq FROM invoices
		WHERE id = $1 AND charge_started_seq = $2 AND charge_started_at = $3
		FOR UPDATE
	`, id, seq, startedAt).Scan(&attemptSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lock charge record: %w", err)
	}
	return attemptSeq, true, nil
}

// ReleaseInterruptedCharge clears a record Stripe has no PaymentIntent for. If
// charge_attempt_seq still equals the record's seq it moves it, because an
// outcome was saved: "no payment was created". The next charge then sends a
// fresh key, and does not get back a 500 Stripe may have saved under the old
// one. This is the one write ADR-105's amendment lets move the seq without a
// PaymentIntent (TestChargeAttemptSeqBumpedByEveryPIStamp admits exactly this
// statement). updated_at is left alone: nothing about the invoice changed.
//
// A CAS on the record as listed (seq and start time): released=false means it
// changed since. seqMoved=false with released=true means another outcome was
// saved meanwhile and already moved the seq.
func (s *PostgresStore) ReleaseInterruptedCharge(ctx context.Context, tenantID, id string, seq int64, startedAt time.Time) (released, seqMoved bool, err error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, false, err
	}
	defer postgres.Rollback(tx)

	attemptSeq, found, err := lockChargeRecordTx(ctx, tx, id, seq, startedAt)
	if err != nil || !found {
		return false, false, err
	}
	if attemptSeq == seq {
		_, err = tx.ExecContext(ctx, `
			UPDATE invoices
			SET charge_attempt_seq = charge_attempt_seq + 1,
			    charge_started_at = NULL, charge_started_seq = NULL,
			    charge_started_key = NULL, charge_started_stripe_customer = NULL
			WHERE id = $1
		`, id)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE invoices
			SET charge_started_at = NULL, charge_started_seq = NULL,
			    charge_started_key = NULL, charge_started_stripe_customer = NULL
			WHERE id = $1
		`, id)
	}
	if err != nil {
		return false, false, fmt.Errorf("release charge record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return true, attemptSeq == seq, nil
}

// ParkInterruptedCharge gives up on a record Stripe could not be asked about
// for the give-up age. If the invoice still waits on that attempt (finalized,
// pending or failed, at the record's seq) it parks it — ADR-107's shape,
// payment_status 'unknown' with no PaymentIntent id, which no charge claim
// accepts — and clears the record, in one statement (parked). The seq moves,
// as for every saved outcome. Otherwise the invoice moved on (paid, or
// another outcome saved), and only the record clears (cleared). Both false:
// the record changed since it was listed.
func (s *PostgresStore) ParkInterruptedCharge(ctx context.Context, tenantID, id string, seq int64, startedAt time.Time, message string) (parked, cleared bool, err error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, false, err
	}
	defer postgres.Rollback(tx)

	if _, found, err := lockChargeRecordTx(ctx, tx, id, seq, startedAt); err != nil || !found {
		return false, false, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE invoices
		SET payment_status = 'unknown', stripe_payment_intent_id = NULL,
		    last_payment_error = $3,
		    charge_attempt_seq = charge_attempt_seq + 1,
		    updated_at = now(),
		    charge_started_at = NULL, charge_started_seq = NULL,
		    charge_started_key = NULL, charge_started_stripe_customer = NULL
		WHERE id = $1
		  AND status = 'finalized'
		  AND payment_status IN ('pending', 'failed')
		  AND charge_attempt_seq = $2
	`, id, seq, postgres.NullableString(message))
	if err != nil {
		return false, false, fmt.Errorf("park interrupted charge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, false, err
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE invoices
			SET charge_started_at = NULL, charge_started_seq = NULL,
			    charge_started_key = NULL, charge_started_stripe_customer = NULL
			WHERE id = $1
		`, id); err != nil {
			return false, false, fmt.Errorf("clear interrupted charge: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return n == 1, true, nil
}
