package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// livemodeCapturingStore embeds the package memStore and records the ctx
// livemode observed at the first store call Dispatch makes (CreateEvent) — the
// point that stamps webhook_event.livemode and drives the same-mode endpoint
// filter.
type livemodeCapturingStore struct {
	*memStore
	captured     bool
	createCalled bool
}

// The P5 dispatch path creates the event via CreateEventWithDeliveries
// (one tx); Go embedding doesn't virtual-dispatch, so the override must
// sit on the method Dispatch actually calls.
func (s *livemodeCapturingStore) CreateEventWithDeliveries(ctx context.Context, tenantID string, e domain.WebhookEvent, endpointIDs []string, birthLease time.Duration, outboxRowID string) (domain.WebhookEvent, []domain.WebhookDelivery, error) {
	s.captured = postgres.Livemode(ctx)
	s.createCalled = true
	return s.memStore.CreateEventWithDeliveries(ctx, tenantID, e, endpointIDs, birthLease, outboxRowID)
}

// TestDispatcher_Handle_PropagatesRowLivemodeIntoCtx pins the cross-mode-leak
// guard. The background outbox dispatcher has no intrinsic mode, so handle()
// must stamp each outbox row's Livemode into ctx BEFORE Service.Dispatch — else
// a test-mode producer's event would match/deliver to LIVE endpoints (or vice
// versa) and writes would land in the wrong RLS partition. The ctx handed in
// deliberately carries NO livemode (the dispatcher's default), so a pass proves
// the value came from row.Livemode, not the caller. Asserts both modes; without
// the `ctx = postgres.WithLivemode(ctx, row.Livemode)` line at the top of
// handle's body (dispatcher.go), ctx
// would carry the default (false) regardless of the row.
func TestDispatcher_Handle_PropagatesRowLivemodeIntoCtx(t *testing.T) {
	for _, mode := range []bool{true, false} {
		name := "testmode"
		if mode {
			name = "livemode"
		}
		t.Run(name, func(t *testing.T) {
			store := &livemodeCapturingStore{memStore: newMemStore()}
			d := &Dispatcher{svc: NewService(store, nil, "local")}

			err := d.handle(context.Background(), OutboxRow{
				TenantID:  "t1",
				Livemode:  mode,
				EventType: "invoice.paid",
				Payload:   map[string]any{"invoice_id": "inv_1"},
			})
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			if !store.createCalled {
				t.Fatal("Dispatch never reached CreateEvent — test is vacuous")
			}
			if store.captured != mode {
				t.Errorf("ctx livemode inside Dispatch = %v, want %v (row.Livemode must propagate)", store.captured, mode)
			}
		})
	}
}
