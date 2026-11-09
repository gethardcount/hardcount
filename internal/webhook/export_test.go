package webhook

import (
	"context"
	"fmt"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// DispatchFromOutbox exposes dispatchFromOutbox to the tests in webhook_test,
// which create an event the way the outbox drain does.
func (s *Service) DispatchFromOutbox(ctx context.Context, outboxRowID, tenantID, eventType string, payload map[string]any) error {
	return s.dispatchFromOutbox(ctx, outboxRowID, tenantID, eventType, payload)
}

// EnqueueCommitted queues a pending row with Enqueue and commits it, for the
// tests that need a row to drain and have no state change to save with it.
// Production code has no such method: a producer calls Enqueue on the
// transaction that writes its change (ADR-040).
func (s *OutboxStore) EnqueueCommitted(ctx context.Context, tenantID, eventType string, payload map[string]any) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "", fmt.Errorf("outbox: begin tx: %w", err)
	}
	defer postgres.Rollback(tx)
	id, err := s.Enqueue(ctx, tx, tenantID, eventType, payload)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("outbox: commit: %w", err)
	}
	return id, nil
}
