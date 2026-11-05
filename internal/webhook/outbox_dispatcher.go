package webhook

import (
	"context"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
)

// OutboxDispatcher satisfies domain.EventDispatcher by queuing each event as a
// pending webhook_outbox row, in a transaction of its own. The Dispatcher
// worker drains the table.
//
// That row commits after the caller's business transaction, not with it. A
// crash or a cancelled ctx between the two commits loses the event, and
// nothing sends it again. A producer with a transaction should queue the
// event on it with OutboxStore.Enqueue instead (ADR-040).
// internal/arch/webhook_emit_catalog_test.go lists the sites that still come
// through here.
type OutboxDispatcher struct {
	outbox *OutboxStore
}

func NewOutboxDispatcher(outbox *OutboxStore) *OutboxDispatcher {
	return &OutboxDispatcher{outbox: outbox}
}

// Dispatch queues a pending outbox row in its own transaction. When the insert
// fails it returns the error and counts the event in
// hardcount_webhook_outbox_enqueue_failures_total. The caller's change has
// already committed by then, so the event is lost; callers log the error.
func (d *OutboxDispatcher) Dispatch(ctx context.Context, tenantID, eventType string, payload map[string]any) error {
	_, err := d.outbox.EnqueueStandalone(ctx, tenantID, eventType, payload)
	if err != nil {
		mw.RecordWebhookOutboxEnqueueFailure(eventType)
	}
	return err
}
