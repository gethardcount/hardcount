package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// PostgresWebhookStore persists Stripe webhook events for audit trail.
type PostgresWebhookStore struct {
	db *postgres.DB
}

func NewPostgresWebhookStore(db *postgres.DB) *PostgresWebhookStore {
	return &PostgresWebhookStore{db: db}
}

// WasProcessed reports whether an event with this stripe_event_id has
// already been ingested for the tenant+livemode. Read-only idempotency
// pre-check used by HandleWebhook before running side-effects; the dedup
// row itself is written by IngestEvent only after processing succeeds, so
// "row present" implies "side-effect committed."
//
// The (tenant_id, livemode, stripe_event_id) tuple matches IngestEvent's
// ON CONFLICT target, so this check sees exactly the rows that would block
// an insert. A concurrent redelivery that passes this read and races into
// IngestEvent is still de-duplicated by the ON CONFLICT DO NOTHING there;
// double-processing is safe because the side-effects are idempotent.
func (s *PostgresWebhookStore) WasProcessed(ctx context.Context, tenantID, stripeEventID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	livemode, err := postgres.Livemode(ctx)

	if err != nil {

		return false, err

	}

	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM stripe_webhook_events
			WHERE tenant_id = $1 AND livemode = $2 AND stripe_event_id = $3
		)
	`, tenantID, livemode, stripeEventID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check webhook event processed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return exists, nil
}

func (s *PostgresWebhookStore) IngestEvent(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) (domain.StripeWebhookEvent, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.StripeWebhookEvent{}, false, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_swe")
	now := time.Now().UTC()
	payloadJSON, _ := json.Marshal(event.Payload)
	if event.Payload == nil {
		payloadJSON = []byte("{}")
	}

	var amountCents *int64
	if event.AmountCents != nil {
		amountCents = event.AmountCents
	}

	livemode, err := postgres.Livemode(ctx)

	if err != nil {

		return domain.StripeWebhookEvent{}, false, err

	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO stripe_webhook_events (id, tenant_id, livemode, stripe_event_id, event_type, object_type,
			invoice_id, customer_external_id, payment_intent_id, payment_status,
			amount_cents, currency, failure_message, payload, received_at, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (tenant_id, livemode, stripe_event_id) DO NOTHING
		RETURNING id
	`, id, tenantID, livemode, event.StripeEventID, event.EventType, event.ObjectType,
		postgres.NullableString(event.InvoiceID), postgres.NullableString(event.CustomerExternalID),
		postgres.NullableString(event.PaymentIntentID), postgres.NullableString(event.PaymentStatus),
		amountCents, postgres.NullableString(event.Currency),
		postgres.NullableString(event.FailureMessage), payloadJSON, now, now,
	).Scan(&event.ID)

	if err != nil {
		// ON CONFLICT DO NOTHING → Scan gets ErrNoRows → duplicate.
		// Any other error (FK violation, etc.) is a real failure.
		if err == sql.ErrNoRows {
			event.ID = ""
			if err := tx.Commit(); err != nil {
				return event, false, err
			}
			return event, false, nil
		}
		_ = tx.Rollback()
		return domain.StripeWebhookEvent{}, false, fmt.Errorf("insert webhook event: %w", err)
	}

	event.TenantID = tenantID
	event.ReceivedAt = now
	if err := tx.Commit(); err != nil {
		return domain.StripeWebhookEvent{}, false, err
	}
	return event, true, nil
}

// ListByInvoice returns all webhook events for an invoice, ordered chronologically.
func (s *PostgresWebhookStore) ListByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.StripeWebhookEvent, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, stripe_event_id, event_type, object_type,
			COALESCE(invoice_id,''), COALESCE(customer_external_id,''),
			COALESCE(payment_intent_id,''), COALESCE(payment_status,''),
			amount_cents, COALESCE(currency,''), COALESCE(failure_message,''),
			received_at, occurred_at
		FROM stripe_webhook_events
		WHERE invoice_id = $1
		ORDER BY occurred_at ASC
	`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []domain.StripeWebhookEvent
	for rows.Next() {
		var e domain.StripeWebhookEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.StripeEventID, &e.EventType, &e.ObjectType,
			&e.InvoiceID, &e.CustomerExternalID, &e.PaymentIntentID, &e.PaymentStatus,
			&e.AmountCents, &e.Currency, &e.FailureMessage,
			&e.ReceivedAt, &e.OccurredAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
