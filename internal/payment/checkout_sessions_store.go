package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// CheckoutClaim is one row of the checkout_sessions claim ledger (0125,
// ADR-068). A claim is created BEFORE the Stripe session exists
// (StripeSessionID empty) and filled via CAS once the create returns; the
// claim ID derives the Stripe idempotency key, so any re-drive — crash
// recovery, the concurrent-double-POST loser — converges on the same
// Stripe session.
type CheckoutClaim struct {
	ID              string
	TenantID        string
	InvoiceID       string
	Livemode        bool
	StripeSessionID string
	URL             string
	AmountCents     int64
	Currency        string
	Status          string
	ExpiresAt       *time.Time
	CreatedAt       time.Time
}

// ErrInvoiceNotPayable is returned by ClaimOpen when the payable re-check
// inside the claim transaction fails — the invoice settled or voided under
// the caller. The mint-vs-settle TOCTOU killer: the FOR SHARE read
// serializes against the paid-flip's FOR UPDATE, so either the claim aborts
// here, or it commits first and the settle's in-tx close is guaranteed to
// see the row. errs.InvalidState-kinded so peer-import-free handlers map it
// to an honest 409 via the standard taxonomy.
var ErrInvoiceNotPayable = errs.InvalidState("invoice is not in a payable state").WithCode("not_payable")

// ErrChargeInFlight is the in-flight guard: a PaymentIntent is processing
// (dunning auto-retry racing the customer's Pay click — the most likely
// double-charge collision), or a drifted session the customer just
// completed is settling. Parity with the void/uncollectible/offline guards.
var ErrChargeInFlight = errs.InvalidState("a payment for this invoice is already in progress").WithCode("payment_in_progress")

// CheckoutSessionStore persists checkout-session claims. All methods are
// tenant-scoped via TxTenant (RLS-fenced).
type CheckoutSessionStore struct {
	db *postgres.DB
}

func NewCheckoutSessionStore(db *postgres.DB) *CheckoutSessionStore {
	return &CheckoutSessionStore{db: db}
}

// ClaimOpen inserts the open claim for an invoice, re-verifying payability
// in the same transaction. Returns (claim, winner=true) when this call
// created the claim; (existing, winner=false) when an open claim already
// exists (the partial unique index is the concurrent-double-POST guard —
// the loser gets the winner's row and re-drives its Stripe create with the
// winner's claim-derived idempotency key).
func (s *CheckoutSessionStore) ClaimOpen(ctx context.Context, tenantID, invoiceID string, amountCents int64, currency string, livemode bool) (CheckoutClaim, bool, error) {
	return s.ClaimOpenAudited(ctx, tenantID, invoiceID, amountCents, currency, livemode, nil)
}

// ClaimOpenAudited is ClaimOpen with an in-tx audit emission hook (ADR-090).
// emit fires ONLY on the winner path (the call that actually created the
// claim) — the loser/reuse paths mutate nothing new, so they record nothing.
func (s *CheckoutSessionStore) ClaimOpenAudited(ctx context.Context, tenantID, invoiceID string, amountCents int64, currency string, livemode bool, emit func(tx *sql.Tx, claim CheckoutClaim) error) (CheckoutClaim, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return CheckoutClaim{}, false, err
	}
	defer postgres.Rollback(tx)

	// Payable re-check under FOR SHARE: serializes vs the settle's FOR
	// UPDATE paid-flip. A session minted after expire-on-settle already ran
	// would otherwise survive on a paid invoice with no remaining expire
	// trigger.
	var status, paymentStatus string
	var amountDue int64
	var charging bool
	err = tx.QueryRowContext(ctx, `
		SELECT status, payment_status, amount_due_cents,
		       COALESCE(auto_charge_claimed_until > now(), false)
		FROM invoices WHERE id = $1 FOR SHARE
	`, invoiceID).Scan(&status, &paymentStatus, &amountDue, &charging)
	if err != nil {
		return CheckoutClaim{}, false, fmt.Errorf("payable re-check: %w", err)
	}
	if status != "finalized" || amountDue <= 0 {
		return CheckoutClaim{}, false, ErrInvoiceNotPayable
	}
	// IsInFlight semantics, not just 'processing' (domain.InvoicePaymentStatus.
	// IsInFlight): 'unknown' means a PaymentIntent MAY be live and charging, so
	// letting the customer pay through the hosted page is the same double charge
	// by a customer-facing route.
	if paymentStatus == "processing" || paymentStatus == "unknown" {
		return CheckoutClaim{}, false, ErrChargeInFlight
	}
	// A held charge lease is a Velox charge on its way to Stripe: its
	// payment_status is still pending/failed until the call returns. The
	// charge claims lock this row FOR UPDATE before taking the lease, so this
	// FOR SHARE read either waits for that claim and sees its lease, or wins
	// and the claim sees the session this transaction inserts.
	if charging {
		return CheckoutClaim{}, false, ErrChargeInFlight
	}
	// The customer already paid a session whose payment is not recorded yet:
	// a new page would let them pay twice. Settling the invoice ends this.
	var paidUnrecorded bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM checkout_sessions
			WHERE invoice_id = $1 AND status = 'completed'
			  AND COALESCE(expires_at, created_at + interval '1 hour') + interval '24 hours' > now()
		)
	`, invoiceID).Scan(&paidUnrecorded); err != nil {
		return CheckoutClaim{}, false, fmt.Errorf("completed session check: %w", err)
	}
	if paidUnrecorded {
		return CheckoutClaim{}, false, ErrChargeInFlight
	}
	if amountCents != amountDue {
		// The caller minted its intent from a stale read; re-anchor on the
		// locked row's truth so the session amount can never drift from the
		// invoice within this claim.
		amountCents = amountDue
	}

	claim := CheckoutClaim{
		ID:          postgres.NewID("vlx_cks"),
		TenantID:    tenantID,
		InvoiceID:   invoiceID,
		Livemode:    livemode,
		AmountCents: amountCents,
		Currency:    currency,
		Status:      "open",
	}
	// created_at comes back because it anchors the Stripe-side expiry: every
	// re-drive of this claim must send identical params (see mintForClaim).
	err = tx.QueryRowContext(ctx, `
		INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'open')
		RETURNING created_at
	`, claim.ID, tenantID, invoiceID, livemode, amountCents, currency).Scan(&claim.CreatedAt)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			// Loser protocol. The violation ABORTED this tx (25P02 — no
			// further statements run on it), so the winner's row must be
			// read on a FRESH tx. If the winner's claim vanished in the gap
			// (settled/superseded already), retry the whole claim — bounded,
			// because each retry either wins the insert or finds a row.
			postgres.Rollback(tx)
			for attempt := 0; attempt < 3; attempt++ {
				existing, gErr := s.GetOpenForInvoice(ctx, tenantID, invoiceID)
				if gErr == nil {
					return existing, false, nil
				}
				if !errors.Is(gErr, errs.ErrNotFound) {
					return CheckoutClaim{}, false, fmt.Errorf("fetch winning claim after unique violation: %w", gErr)
				}
				// Winner's claim already closed — re-attempt the claim.
				c, w, rErr := s.ClaimOpenAudited(ctx, tenantID, invoiceID, amountCents, currency, livemode, emit)
				if rErr == nil || !postgres.IsUniqueViolation(rErr) {
					return c, w, rErr
				}
			}
			return CheckoutClaim{}, false, fmt.Errorf("claim contention did not converge for invoice %s", invoiceID)
		}
		return CheckoutClaim{}, false, fmt.Errorf("insert claim: %w", err)
	}
	if emit != nil {
		if err := emit(tx, claim); err != nil {
			return CheckoutClaim{}, false, fmt.Errorf("audit emission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return CheckoutClaim{}, false, err
	}
	return claim, true, nil
}

// GetOpenForInvoice returns the invoice's open claim, errs.ErrNotFound when
// none.
func (s *CheckoutSessionStore) GetOpenForInvoice(ctx context.Context, tenantID, invoiceID string) (CheckoutClaim, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return CheckoutClaim{}, err
	}
	defer postgres.Rollback(tx)
	return scanOpenClaim(ctx, tx, invoiceID)
}

func scanOpenClaim(ctx context.Context, tx *sql.Tx, invoiceID string) (CheckoutClaim, error) {
	var c CheckoutClaim
	var sessID, url sql.NullString
	var expiresAt sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, invoice_id, livemode, stripe_session_id, url, amount_cents, currency, status, expires_at, created_at
		FROM checkout_sessions WHERE invoice_id = $1 AND status = 'open'
	`, invoiceID).Scan(&c.ID, &c.TenantID, &c.InvoiceID, &c.Livemode, &sessID, &url,
		&c.AmountCents, &c.Currency, &c.Status, &expiresAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckoutClaim{}, errs.ErrNotFound
	}
	if err != nil {
		return CheckoutClaim{}, err
	}
	c.StripeSessionID = sessID.String
	c.URL = url.String
	if expiresAt.Valid {
		t := expiresAt.Time
		c.ExpiresAt = &t
	}
	return c, nil
}

// FillSession records the Stripe session onto the claim via CAS
// (stripe_session_id IS NULL). Losing the CAS is fine — the idempotency key
// guarantees both racers hold the SAME session, so the row already carries
// these values. It returns the claim's status after the write: a claim
// superseded while its create was in flight still records the session (so
// the sweeps can find it), but the caller must not hand its URL out.
func (s *CheckoutSessionStore) FillSession(ctx context.Context, tenantID, claimID, stripeSessionID, url string, expiresAt time.Time) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE checkout_sessions
		SET stripe_session_id = $1, url = $2, expires_at = $3, updated_at = now()
		WHERE id = $4 AND stripe_session_id IS NULL
	`, stripeSessionID, url, expiresAt, claimID); err != nil {
		return "", err
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM checkout_sessions WHERE id = $1`, claimID).Scan(&status); err != nil {
		return "", err
	}
	return status, tx.Commit()
}

// Supersede CAS-flips an open claim to superseded (amount drift or
// time-expiry). Exactly one concurrent caller wins (rowcount=1) and
// proceeds to remint; losers re-read and follow the loser protocol.
func (s *CheckoutSessionStore) Supersede(ctx context.Context, tenantID, claimID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(ctx, `
		UPDATE checkout_sessions SET status = 'superseded', updated_at = now()
		WHERE id = $1 AND status = 'open'
	`, claimID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// MarkCompleted is the checkout.session.completed truth-sync: the session
// finished at Stripe. Keyed on the Stripe session id (webhook-side lookup);
// TxBypass because the webhook carries no tenant ctx and the row is located
// by the globally-unique Stripe id.
func (s *CheckoutSessionStore) MarkCompleted(ctx context.Context, stripeSessionID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE checkout_sessions SET status = 'completed', updated_at = now()
		WHERE stripe_session_id = $1 AND status IN ('open', 'superseded')
	`, stripeSessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListUnresolvedForInvoice returns every claim whose Stripe session may
// still be payable — the settle-time expire sweep's input (all rows not yet
// confirmed expired/completed, not just the open one: a superseded row's
// session stays live at Stripe if its expire call failed).
func (s *CheckoutSessionStore) ListUnresolvedForInvoice(ctx context.Context, tenantID, invoiceID string) ([]CheckoutClaim, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, invoice_id, livemode, stripe_session_id, url, amount_cents, currency, status, expires_at, created_at
		FROM checkout_sessions
		WHERE invoice_id = $1
		  AND stripe_session_id IS NOT NULL
		  AND status IN ('open', 'superseded', 'invoice_settled')
		  AND (expires_at IS NULL OR expires_at > now())
	`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CheckoutClaim
	for rows.Next() {
		var c CheckoutClaim
		var sessID, url sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.TenantID, &c.InvoiceID, &c.Livemode, &sessID, &url,
			&c.AmountCents, &c.Currency, &c.Status, &expiresAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.StripeSessionID = sessID.String
		c.URL = url.String
		if expiresAt.Valid {
			t := expiresAt.Time
			c.ExpiresAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkExpired records a confirmed Stripe-side expire (or an
// "already expired" response, which is the same truth).
func (s *CheckoutSessionStore) MarkExpired(ctx context.Context, tenantID, claimID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE checkout_sessions SET status = 'expired', updated_at = now() WHERE id = $1
	`, claimID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrSessionCompleted: a Stripe expire attempt found the session COMPLETED —
// the customer already paid it. The caller must NOT mint a replacement (a
// second payable vehicle = the double charge); the webhook settle is in
// flight and the amount-mismatch escalation owns any drift consequence.
var ErrSessionCompleted = errors.New("checkout session already completed")

// ListHostedToReconcile is the hosted sweep's input (reconciler_hosted.go):
//   - recorded sessions whose outcome Velox has not seen ('open',
//     'superseded', and 'invoice_settled', where the customer may have paid in
//     the same moment the invoice was settled another way); and
//   - 'completed' sessions whose invoice is still unpaid: the
//     checkout.session.completed webhook arrived but the payment's own did
//     not. They leave this list when the payment settles.
//
// Each row waits until it is checkedBefore old (created, filled or last
// looked at), so the ordinary webhook records a payment first and the sweep
// spends no call on it. Not until the session expires: a paid session is
// final at Stripe as soon as the customer pays, so waiting would only leave
// the invoice unpaid longer. An unpaid session reads 'open' and is touched to
// the back of the queue until it is paid or expires.
//
// Cross-tenant (TxBypass) for the ctx's mode, least recently looked at first
// so a session that keeps erroring cannot hold the head of the queue.
func (s *CheckoutSessionStore) ListHostedToReconcile(ctx context.Context, checkedBefore time.Time, limit int) ([]CheckoutClaim, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)
	if limit <= 0 {
		limit = 50
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT cs.id, cs.tenant_id, cs.invoice_id, cs.livemode, cs.stripe_session_id, cs.url, cs.amount_cents,
		       cs.currency, cs.status, cs.expires_at, cs.created_at
		FROM checkout_sessions cs
		WHERE cs.stripe_session_id IS NOT NULL
		  AND cs.livemode = $2
		  AND cs.updated_at < $1
		  AND (
		    cs.status IN ('open', 'superseded', 'invoice_settled')
		    OR (cs.status = 'completed'
		      AND EXISTS (
		        SELECT 1 FROM invoices i
		        WHERE i.id = cs.invoice_id
		          AND i.status = 'finalized'
		          AND i.payment_status <> 'succeeded'
		      ))
		  )
		ORDER BY cs.updated_at ASC
		LIMIT $3
	`, checkedBefore, postgres.Livemode(ctx), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CheckoutClaim
	for rows.Next() {
		var c CheckoutClaim
		var sessID, url sql.NullString
		var expiresAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.TenantID, &c.InvoiceID, &c.Livemode, &sessID, &url,
			&c.AmountCents, &c.Currency, &c.Status, &expiresAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.StripeSessionID = sessID.String
		c.URL = url.String
		if expiresAt.Valid {
			t := expiresAt.Time
			c.ExpiresAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TouchReconcile moves a session the sweep could not resolve to the back of
// its queue.
func (s *CheckoutSessionStore) TouchReconcile(ctx context.Context, tenantID, claimID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx,
		`UPDATE checkout_sessions SET updated_at = now() WHERE id = $1`, claimID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkSessionCompleted records the hosted sweep's finding that a session was
// paid. Keyed on the claim id and unconditional on the prior status, unlike
// the webhook's MarkCompleted: the sweep also resolves 'invoice_settled' rows,
// and a row it leaves unchanged would be swept again every tick.
func (s *CheckoutSessionStore) MarkSessionCompleted(ctx context.Context, tenantID, claimID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx,
		`UPDATE checkout_sessions SET status = 'completed', updated_at = now() WHERE id = $1`, claimID); err != nil {
		return err
	}
	return tx.Commit()
}
