package invoice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The invoice store queues every invoice.* webhook event on the transaction
// that writes its change (ADR-040), with the invoice as that transaction
// leaves it (ADR-131). The change and its event commit together or not at
// all, so a crash, a deploy or a cancelled request right after the commit
// cannot lose the event. An enqueue error fails the change.

// enqueueInvoiceEventTx queues eventType on tx, with inv and extra as its
// data. With no outbox wired (narrow tests) it queues nothing; the router
// always wires one, and webhook.MustWiredOutbox refuses to boot without it.
func (s *PostgresStore) enqueueInvoiceEventTx(ctx context.Context, tx *sql.Tx, tenantID, eventType string, inv domain.Invoice, extra map[string]any) error {
	if s.outbox == nil {
		return nil
	}
	if _, err := s.outbox.Enqueue(ctx, tx, tenantID, eventType, domain.InvoiceWebhookData(inv, extra)); err != nil {
		return fmt.Errorf("enqueue %s: %w", eventType, err)
	}
	return nil
}

// bornFinalizedTx queues invoice.finalized for an invoice inserted already
// finalized: a cycle close, a first invoice, a threshold fire, a cancel's
// final invoice, a plan swap or a proration. A draft queues nothing here. It
// is sent when the draft is finalized (finalizedTx).
func (s *PostgresStore) bornFinalizedTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice) error {
	if inv.Status != domain.InvoiceFinalized {
		return nil
	}
	return s.enqueueInvoiceEventTx(ctx, tx, tenantID, domain.EventInvoiceFinalized, inv, nil)
}

// finalizedTx is the last step of every write that takes a draft out of
// draft. It gives the invoice its hosted-page token if it has none, then
// queues invoice.finalized with the row as tx leaves it, and returns that
// row. The event is sent even when a finalize hook has already marked the
// invoice paid (a held relief that covers it): the invoice was finalized
// first.
func (s *PostgresStore) finalizedTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice) (domain.Invoice, error) {
	inv, err := s.mintPublicTokenTx(ctx, tx, inv)
	if err != nil {
		return domain.Invoice{}, err
	}
	if err := s.enqueueInvoiceEventTx(ctx, tx, tenantID, domain.EventInvoiceFinalized, inv, nil); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// mintPublicTokenTx stores a hosted-page token for inv on tx when it has
// none, and returns the row. A finalized invoice always has one, the rule
// createWithLineItemsInTx applies to an invoice born finalized. The write
// leaves updated_at alone: it is part of the finalize, which stamped it.
func (s *PostgresStore) mintPublicTokenTx(ctx context.Context, tx *sql.Tx, inv domain.Invoice) (domain.Invoice, error) {
	if inv.PublicToken != "" {
		return inv, nil
	}
	token, err := GeneratePublicToken()
	if err != nil {
		return domain.Invoice{}, err
	}
	encToken, tokenHash := s.encodeToken(token)
	// The hash is the token's presence: an invoice whose token could not be
	// encrypted has a hash and no readable token, and keeps the token it has.
	var out domain.Invoice
	err = tx.QueryRowContext(ctx, `
		UPDATE invoices SET public_token_encrypted = $1, public_token_hash = $2
		WHERE id = $3 AND public_token_hash IS NULL
		RETURNING `+invCols,
		postgres.NullableString(encToken), tokenHash, inv.ID,
	).Scan(s.scanInvDest(&out)...)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return inv, nil
	case postgres.IsUniqueViolation(err):
		// 256 bits of entropy make a collision astronomically unlikely. If one
		// happens, fail rather than give this invoice another's URL.
		return domain.Invoice{}, fmt.Errorf("mint public token: collision: %w", err)
	case err != nil:
		return domain.Invoice{}, fmt.Errorf("mint public token: %w", err)
	}
	return out, nil
}

// statusChangedTx queues the event of a status flip on its transaction and
// returns the invoice as tx leaves it: invoice.finalized (finalizedTx),
// invoice.voided or invoice.marked_uncollectible. It reads the row again,
// so the event shows what the caller's in-transaction writes did to it. A
// void's tax reversal stamps the row after the commit, so the event never
// carries that stamp.
func (s *PostgresStore) statusChangedTx(ctx context.Context, tx *sql.Tx, tenantID, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	var inv domain.Invoice
	if err := tx.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, id).Scan(s.scanInvDest(&inv)...); err != nil {
		return domain.Invoice{}, fmt.Errorf("read the invoice after its move to %s: %w", status, err)
	}
	// Each branch names its event type as a constant, so the webhook catalog
	// test (internal/arch) can check it.
	var err error
	switch status {
	case domain.InvoiceFinalized:
		return s.finalizedTx(ctx, tx, tenantID, inv)
	case domain.InvoiceVoided:
		err = s.enqueueInvoiceEventTx(ctx, tx, tenantID, domain.EventInvoiceVoided, inv, nil)
	case domain.InvoiceUncollectible:
		err = s.enqueueInvoiceEventTx(ctx, tx, tenantID, domain.EventInvoiceMarkedUncollectible, inv, nil)
	default:
		err = fmt.Errorf("statusChangedTx: no event for status %q", status)
	}
	if err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}
