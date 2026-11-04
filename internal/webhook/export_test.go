package webhook

import "context"

// DispatchFromOutbox exposes dispatchFromOutbox to the tests in webhook_test,
// which create an event the way the outbox drain does.
func (s *Service) DispatchFromOutbox(ctx context.Context, outboxRowID, tenantID, eventType string, payload map[string]any) error {
	return s.dispatchFromOutbox(ctx, outboxRowID, tenantID, eventType, payload)
}
