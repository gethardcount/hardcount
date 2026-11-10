package paymentmethods

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Store is the persistence contract. Kept narrow — each operation maps to
// exactly one handler action so we don't grow a god-store.
type Store interface {
	List(ctx context.Context, tenantID, customerID string) ([]PaymentMethod, error)
	Get(ctx context.Context, tenantID, pmID string) (PaymentMethod, error)

	// Upsert writes a payment_methods row keyed by stripe_payment_method_id.
	// Webhooks can fire more than once for the same setup intent (Stripe
	// retries on 5xx), so the webhook path must be idempotent. The row is
	// made the default in the same tx when makeDefault is set (a card saved
	// through a setup flow becomes the default, ADR-021), when the customer
	// has no active default, or when the bank refused the current default
	// for good (ADR-126). replacedDefault reports that another card was the
	// default and this one took its place. A card already removed (detached)
	// stays removed: the row is returned unchanged.
	//
	// eventType ("" for none) is the payment_method.* webhook event the write
	// reports, queued in its transaction with the card and extra as its
	// data, only when the write changed what the API shows of the card.
	Upsert(ctx context.Context, tenantID string, pm PaymentMethod, makeDefault bool, eventType string, extra map[string]any) (out PaymentMethod, replacedDefault bool, err error)

	// SetDefault atomically clears any existing default for (customerID)
	// and flags pmID as the new default. Fails with ErrNotFound if pmID is
	// detached or not owned by customerID. Safe to call concurrently: it
	// waits for any other change to the customer's default.
	SetDefault(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error)

	// DetachAndRebalance detaches pmID and, in the SAME transaction,
	// promotes the newest remaining active PM to default when the detached
	// card was the default — so there is never a committed "active cards
	// but no default" window (a transient failure on the promote rolls the
	// detach back too). Returns the detached row plus the promoted default
	// (nil when the detached card wasn't the default, or no active PM
	// remained to promote). Idempotent: re-detaching an already-detached
	// row promotes nothing (a detached row is never the default). Safe to
	// call concurrently: it waits for any other change to the customer's
	// default, so two removals at once still leave one default.
	DetachAndRebalance(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, *PaymentMethod, error)

	// CustomersWithDefault reports, in one read, which of customerIDs have
	// an active (not detached) default payment method — the same row List
	// puts first and the payment-setup view calls ready. Customers without
	// one are absent from the map.
	CustomersWithDefault(ctx context.Context, tenantID string, customerIDs []string) (map[string]bool, error)
}

type PostgresStore struct {
	db     *postgres.DB
	outbox OutboxEnqueuer
}

func NewPostgresStore(db *postgres.DB) *PostgresStore { return &PostgresStore{db: db} }

// OutboxEnqueuer inserts a webhook-event row on the caller's tx, so the event
// commits or rolls back with the change that caused it (ADR-040
// transactional outbox). Satisfied by *webhook.OutboxStore; declared here so
// this store needs no webhook import. Same seam as the invoice, subscription
// and dunning stores.
type OutboxEnqueuer interface {
	Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error)
}

// SetOutboxEnqueuer wires payment_method.attached and .updated into the
// transaction of the attach each reports (Upsert). Optional: when unset
// (narrow tests), no event is queued. Production boot refuses an unset one
// (webhook.MustWiredOutbox).
func (s *PostgresStore) SetOutboxEnqueuer(o OutboxEnqueuer) { s.outbox = o }

const pmSelectCols = `id, tenant_id, livemode, customer_id, stripe_payment_method_id,
	type, COALESCE(card_brand,''), COALESCE(card_last4,''),
	COALESCE(card_exp_month,0), COALESCE(card_exp_year,0),
	COALESCE(card_fingerprint,''),
	is_default, detached_at, created_at, updated_at`

func scanPM(row interface {
	Scan(dest ...any) error
}, pm *PaymentMethod) error {
	return row.Scan(&pm.ID, &pm.TenantID, &pm.Livemode, &pm.CustomerID, &pm.StripePaymentMethodID,
		&pm.Type, &pm.CardBrand, &pm.CardLast4, &pm.CardExpMonth, &pm.CardExpYear,
		&pm.CardFingerprint,
		&pm.IsDefault, &pm.DetachedAt, &pm.CreatedAt, &pm.UpdatedAt)
}

func (s *PostgresStore) List(ctx context.Context, tenantID, customerID string) ([]PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+pmSelectCols+` FROM payment_methods
		WHERE tenant_id = $1 AND customer_id = $2 AND detached_at IS NULL
		ORDER BY is_default DESC, created_at DESC, id DESC`, tenantID, customerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []PaymentMethod
	for rows.Next() {
		var pm PaymentMethod
		if err := scanPM(rows, &pm); err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func (s *PostgresStore) CustomersWithDefault(ctx context.Context, tenantID string, customerIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(customerIDs))
	if len(customerIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT customer_id FROM payment_methods
		WHERE tenant_id = $1 AND customer_id = ANY($2::text[])
		  AND is_default AND detached_at IS NULL`, tenantID, customerIDs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, pmID string) (PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, err
	}
	defer postgres.Rollback(tx)

	var pm PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		SELECT `+pmSelectCols+` FROM payment_methods
		WHERE tenant_id = $1 AND id = $2`, tenantID, pmID), &pm); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, errs.ErrNotFound
		}
		return PaymentMethod{}, err
	}
	return pm, tx.Commit()
}

// Upsert inserts a row keyed by (tenant_id, livemode, stripe_payment_method_id).
// On conflict we refresh card metadata and keep is_default unless this attach
// makes the card the default. If no active default exists for the customer,
// the new row is promoted to default in the same tx (enforces "first PM is
// default").
//
// A card saved through a setup flow becomes the default (makeDefault, ADR-021):
// the customer, or the operator for them, gave this card to be charged, and
// the setup and update pages say Hardcount will use it for upcoming invoices.
// The other active defaults are cleared first, in this tx, on the insert and
// the conflict path alike, so the one-default-per-customer index never sees
// two and a redelivery of a card already saved beside still promotes it.
//
// Without makeDefault, a default the bank refused for good is replaced
// (ADR-126, #966): when a charge attempt recorded a hard decline (domain.IsHardDecline) on the
// customer's current default card, the card being saved becomes the
// default, on the insert and the conflict path alike. That card cannot be
// charged again, and Stripe Billing's rule is the same: after a hard decline
// a retry runs only once it detects a new payment method. Otherwise a card
// the customer saved through the update link, asked for a different card,
// sat beside the refused one while every retry skipped it until dunning ran
// out. A working default is not replaced: a card saved by paying on the
// hosted page, or by a SetupIntent Hardcount did not create, is added beside it.
//
// A card already removed stays removed. A late webhook (Stripe redelivers
// setup_intent.succeeded after a failed first delivery, for up to three
// days) can name a card the operator has since removed, or an older copy of
// a card the dedupe below replaced. Stripe does not let a detached
// PaymentMethod be used or attached again, so the row is returned unchanged
// and nothing else is touched: bringing it back, above all as the default,
// would point every retry at a card Stripe refuses.
//
// replacedDefault is true when another card was the customer's default and
// this attach moved the default to this card (a setup flow, or a refused
// default replaced). The first card, a card added beside, a same-card
// dedupe and a redelivery of the current default report false.
//
// Dedupe-by-fingerprint (ADR-0099): when a customer re-runs Add and
// produces a new pm_xxx with a fingerprint that already exists as an
// active row for this customer, the old row is detached in the same
// transaction and the new row inherits its is_default flag. Industry
// standard — Stripe explicitly recommends this pattern because each
// Checkout completion mints a fresh PaymentMethod even for the same
// physical card. Without dedupe, the customer sees "Visa ····4242"
// twice (or more) and the default-PM semantics drift across the
// duplicates. Skipped when fingerprint is empty (legacy rows /
// non-card types) — those keep current behavior.
//
// eventType (payment_method.attached or .updated, "" for none) is queued in
// this transaction with the card as it commits (ADR-131) and extra beside
// it, so the card and its event are saved together or not at all. It is
// queued only when the write changed what the API shows of the card: a new
// row, or a change to its default flag, brand, last 4 digits or expiry. A
// Stripe event delivered again, or the hosted sweep reporting a card the
// webhook already saved, changes nothing and sends nothing, so a receiver
// never gets one change twice under two event ids. The second of the two
// events one setup session sends for its card sends one only if it changes
// the card, for example by making it the default. A removed card sends
// nothing either. A failed insert fails the attach.
func (s *PostgresStore) Upsert(ctx context.Context, tenantID string, pm PaymentMethod, makeDefault bool, eventType string, extra map[string]any) (PaymentMethod, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, false, err
	}
	defer postgres.Rollback(tx)

	// One attach at a time per customer. One setup session sends two
	// webhooks for the same card within milliseconds
	// (checkout.session.completed and setup_intent.succeeded), and both
	// attach it. Run side by side, both inserts miss each other: one then
	// fails on the active-fingerprint unique index instead of taking the
	// ON CONFLICT path, and when that one is the setup flow's attach the
	// card stays beside the old default until Stripe redelivers. Serialized,
	// the second attach reads the first's committed row and takes the update
	// path.
	if err := lockCustomerCards(ctx, tx, tenantID, pm.CustomerID); err != nil {
		return PaymentMethod{}, false, err
	}

	// A card already removed stays removed (see above). Read under the lock,
	// so a removal committed before this attach is seen.
	var removed PaymentMethod
	switch err := scanPM(tx.QueryRowContext(ctx, `
		SELECT `+pmSelectCols+` FROM payment_methods
		WHERE tenant_id = $1 AND stripe_payment_method_id = $2
		  AND detached_at IS NOT NULL`,
		tenantID, pm.StripePaymentMethodID), &removed); {
	case err == nil:
		return removed, false, tx.Commit()
	case errors.Is(err, sql.ErrNoRows):
	default:
		return PaymentMethod{}, false, fmt.Errorf("check removed card: %w", err)
	}

	// The card as it stands before this write, if it is saved already: the
	// event below is queued only when the write changes it. Read under the
	// lock, so a concurrent attach of the same card has committed first.
	var before PaymentMethod
	saved := true
	switch err := scanPM(tx.QueryRowContext(ctx, `
		SELECT `+pmSelectCols+` FROM payment_methods
		WHERE tenant_id = $1 AND stripe_payment_method_id = $2
		  AND detached_at IS NULL`,
		tenantID, pm.StripePaymentMethodID), &before); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		saved = false
	default:
		return PaymentMethod{}, false, fmt.Errorf("read saved card: %w", err)
	}

	// Dedupe-by-fingerprint: detach any active row for this customer
	// with the same fingerprint. Capture is_default off the detached
	// row so the new attach can inherit it — otherwise re-adding the
	// default card would unset its default flag (regression hazard).
	// Skip when fingerprint is empty (no key to dedupe on).
	carryDefault := false
	if pm.CardFingerprint != "" {
		var oldDefault bool
		var oldID string
		switch err := tx.QueryRowContext(ctx, `
			SELECT id, is_default FROM payment_methods
			WHERE tenant_id = $1 AND customer_id = $2
			  AND card_fingerprint = $3
			  AND detached_at IS NULL
			  AND stripe_payment_method_id <> $4
			LIMIT 1`, tenantID, pm.CustomerID, pm.CardFingerprint, pm.StripePaymentMethodID,
		).Scan(&oldID, &oldDefault); {
		case err == nil:
			// Match found — detach it. Same-tx so a concurrent List
			// never observes both the old and new active.
			if _, err := tx.ExecContext(ctx, `
				UPDATE payment_methods
				SET detached_at = now(), is_default = false, updated_at = now()
				WHERE tenant_id = $1 AND id = $2`, tenantID, oldID); err != nil {
				return PaymentMethod{}, false, fmt.Errorf("dedupe-detach: %w", err)
			}
			carryDefault = oldDefault
		case errors.Is(err, sql.ErrNoRows):
			// No prior row with this fingerprint — clean path.
		default:
			return PaymentMethod{}, false, fmt.Errorf("dedupe lookup: %w", err)
		}
	}

	// Does this customer already have an active default after the
	// dedupe pass? If not, promote the new row.
	var hasDefault bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM payment_methods
			WHERE tenant_id = $1 AND customer_id = $2
			  AND is_default = true AND detached_at IS NULL
		)`, tenantID, pm.CustomerID).Scan(&hasDefault); err != nil {
		return PaymentMethod{}, false, fmt.Errorf("check default: %w", err)
	}
	promote := !hasDefault || carryDefault

	// The current default was refused for good on one of this customer's
	// invoices: the card being saved takes its place. Asked only when this
	// attach would otherwise leave the current default in place.
	replaceRefused := false
	if !makeDefault && hasDefault && !carryDefault {
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM payment_methods pm
				JOIN invoices i ON i.tenant_id = pm.tenant_id AND i.customer_id = pm.customer_id
				JOIN invoice_charge_attempts a ON a.tenant_id = i.tenant_id AND a.invoice_id = i.id
					AND a.stripe_payment_method_id = pm.stripe_payment_method_id
				WHERE pm.tenant_id = $1 AND pm.customer_id = $2
				  AND pm.is_default AND pm.detached_at IS NULL
				  AND pm.stripe_payment_method_id <> $3
				  AND a.decline_code = ANY($4::text[]))`,
			tenantID, pm.CustomerID, pm.StripePaymentMethodID, domain.HardDeclineCodes(),
		).Scan(&replaceRefused); err != nil {
			return PaymentMethod{}, false, fmt.Errorf("check refused default: %w", err)
		}
	}
	// This card takes the default: clear the others first, in this tx, so
	// the one-default-per-customer index never sees two.
	force := makeDefault || replaceRefused
	replacedDefault := false
	if force {
		res, err := tx.ExecContext(ctx, `
			UPDATE payment_methods SET is_default = false, updated_at = now()
			WHERE tenant_id = $1 AND customer_id = $2
			  AND is_default = true AND detached_at IS NULL
			  AND stripe_payment_method_id <> $3`,
			tenantID, pm.CustomerID, pm.StripePaymentMethodID)
		if err != nil {
			return PaymentMethod{}, false, fmt.Errorf("clear other default: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return PaymentMethod{}, false, fmt.Errorf("clear other default: %w", err)
		}
		replacedDefault = n > 0
		promote = true
	}

	var out PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		INSERT INTO payment_methods
			(tenant_id, customer_id, stripe_payment_method_id, type,
			 card_brand, card_last4, card_exp_month, card_exp_year,
			 card_fingerprint,
			 is_default, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), NULLIF($6,''),
		        NULLIF($7,0), NULLIF($8,0), NULLIF($9,''),
		        $10, now(), now())
		ON CONFLICT (tenant_id, livemode, stripe_payment_method_id) DO UPDATE
		SET card_brand       = EXCLUDED.card_brand,
		    card_last4       = EXCLUDED.card_last4,
		    card_exp_month   = EXCLUDED.card_exp_month,
		    card_exp_year    = EXCLUDED.card_exp_year,
		    card_fingerprint = COALESCE(EXCLUDED.card_fingerprint, payment_methods.card_fingerprint),
		    -- Kept, except when this attach takes the default (a setup flow,
		    -- or a refused default replaced): the old default was cleared
		    -- above, so this row must take its place or the customer would be
		    -- left with none.
		    is_default       = payment_methods.is_default OR $11,
		    updated_at       = now()
		-- A removed row is never updated back to active. Every removal takes
		-- the customer's card lock, so none commits after the removed-card
		-- check above. If one ever did, no row is updated: the attach fails
		-- and rolls back, and the webhook's retry finds the card removed.
		WHERE payment_methods.detached_at IS NULL
		RETURNING `+pmSelectCols,
		tenantID, pm.CustomerID, pm.StripePaymentMethodID, defaultString(pm.Type, "card"),
		pm.CardBrand, pm.CardLast4, pm.CardExpMonth, pm.CardExpYear,
		pm.CardFingerprint,
		promote, force), &out); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, false, fmt.Errorf("upsert payment method: card %s was removed while it was being saved", pm.StripePaymentMethodID)
		}
		return PaymentMethod{}, false, fmt.Errorf("upsert payment method: %w", err)
	}

	if eventType != "" && s.outbox != nil && (!saved || changedForTheAPI(before, out)) {
		if _, err := s.outbox.Enqueue(ctx, tx, tenantID, eventType, domain.PaymentMethodWebhookData(out.View(), extra)); err != nil {
			return PaymentMethod{}, false, fmt.Errorf("enqueue %s: %w", eventType, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return PaymentMethod{}, false, err
	}
	return out, replacedDefault, nil
}

// changedForTheAPI reports whether the card's write changed a field the API
// shows, and the payment_method.* events carry (PaymentMethod.View): its
// default flag or its card details. The id, customer, type and creation
// time of a saved card do not change on a write.
func changedForTheAPI(before, after PaymentMethod) bool {
	return before.IsDefault != after.IsDefault ||
		before.CardBrand != after.CardBrand || before.CardLast4 != after.CardLast4 ||
		before.CardExpMonth != after.CardExpMonth || before.CardExpYear != after.CardExpYear
}

// SetDefault clears the customer's default and sets pmID as the default in
// one transaction, under the customer's card lock (lockCustomerCards).
func (s *PostgresStore) SetDefault(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, err
	}
	defer postgres.Rollback(tx)

	if err := lockCustomerCards(ctx, tx, tenantID, customerID); err != nil {
		return PaymentMethod{}, err
	}

	// Clear any other default for this customer first. Postgres validates
	// the partial unique index per-row; clearing before setting avoids a
	// transient violation that would rollback the whole tx.
	if _, err := tx.ExecContext(ctx, `
		UPDATE payment_methods SET is_default = false, updated_at = now()
		WHERE tenant_id = $1 AND customer_id = $2
		  AND is_default = true AND detached_at IS NULL
		  AND id <> $3`, tenantID, customerID, pmID); err != nil {
		return PaymentMethod{}, fmt.Errorf("clear existing default: %w", err)
	}

	var out PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		UPDATE payment_methods SET is_default = true, updated_at = now()
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3
		  AND detached_at IS NULL
		RETURNING `+pmSelectCols,
		tenantID, customerID, pmID), &out); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, errs.ErrNotFound
		}
		return PaymentMethod{}, fmt.Errorf("set default: %w", err)
	}

	return out, tx.Commit()
}

// DetachAndRebalance does the detach and the replacement-default promotion
// in one transaction so the invariant "≥1 active PM ⇒ exactly one default"
// holds across the whole operation — no committed window with active cards
// but no default. See the Store interface for the contract.
func (s *PostgresStore) DetachAndRebalance(ctx context.Context, tenantID, customerID, pmID string) (PaymentMethod, *PaymentMethod, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return PaymentMethod{}, nil, err
	}
	defer postgres.Rollback(tx)

	// The customer's card lock orders this removal against every other
	// change to the customer's cards, a second removal included.
	if err := lockCustomerCards(ctx, tx, tenantID, customerID); err != nil {
		return PaymentMethod{}, nil, err
	}

	// Read whether the target is the default before the UPDATE clears the
	// flag (RETURNING gives the value after it). FOR UPDATE locks only this
	// row; it does not order removals of the customer's other cards.
	var wasDefault bool
	if err := tx.QueryRowContext(ctx, `
		SELECT is_default FROM payment_methods
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3
		FOR UPDATE`, tenantID, customerID, pmID).Scan(&wasDefault); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, nil, errs.ErrNotFound
		}
		return PaymentMethod{}, nil, fmt.Errorf("lock payment method: %w", err)
	}

	var detached PaymentMethod
	if err := scanPM(tx.QueryRowContext(ctx, `
		UPDATE payment_methods
		SET detached_at = COALESCE(detached_at, now()),
		    is_default  = false,
		    updated_at  = now()
		WHERE tenant_id = $1 AND customer_id = $2 AND id = $3
		RETURNING `+pmSelectCols,
		tenantID, customerID, pmID), &detached); err != nil {
		return PaymentMethod{}, nil, fmt.Errorf("detach: %w", err)
	}

	var newDefault *PaymentMethod
	if wasDefault {
		// Promote the newest remaining active card; the just-detached row is
		// excluded. Under the customer's card lock no other removal commits
		// between the subquery's read and the UPDATE. The outer
		// detached_at IS NULL still keeps the promote off a removed card,
		// the state the doctor's payment_methods_detached_but_still_default
		// check flags. The old default is already cleared, so the one-default
		// unique index sees no other default. ErrNoRows means no active card
		// is left, and the customer has no default.
		var nd PaymentMethod
		err := scanPM(tx.QueryRowContext(ctx, `
			UPDATE payment_methods SET is_default = true, updated_at = now()
			WHERE tenant_id = $1 AND customer_id = $2 AND detached_at IS NULL AND id = (
			    SELECT id FROM payment_methods
			    WHERE tenant_id = $1 AND customer_id = $2 AND detached_at IS NULL
			    ORDER BY created_at DESC, id DESC
			    LIMIT 1
			)
			RETURNING `+pmSelectCols,
			tenantID, customerID), &nd)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return PaymentMethod{}, nil, fmt.Errorf("promote replacement default: %w", err)
		}
		if err == nil {
			newDefault = &nd
		}
	}

	if err := tx.Commit(); err != nil {
		return PaymentMethod{}, nil, err
	}
	return detached, newDefault, nil
}

// lockCustomerCards takes the customer's card lock on tx, held until the
// transaction ends. Every write that can change which card is the
// customer's default takes it first: Upsert, SetDefault and
// DetachAndRebalance. Without it, two such writes at once each read the
// cards before the other commits. The second flip then misses the first's
// new default and fails on the one-default unique index, or a removal
// promotes a card another removal has just removed and leaves no default.
// The key is this table's own; the credit ledger's per-customer lock is a
// different key.
func lockCustomerCards(ctx context.Context, tx *sql.Tx, tenantID, customerID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"payment_methods:"+tenantID+":"+customerID); err != nil {
		return fmt.Errorf("acquire payment method lock: %w", err)
	}
	return nil
}

func defaultString(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Compile-time assertion that PostgresStore satisfies Store.
var _ Store = (*PostgresStore)(nil)
