package usage_test

// Review finding (PR #1376, round 2), on real Postgres: a held row fixes its
// meter's aggregation (ADR-130 §2), so a hold must serialize with an
// aggregation PATCH the way an ingest does. An ingest's foreign key holds
// KEY SHARE on the meter row, which the PATCH's FOR UPDATE waits for. A held
// row has no foreign key to the meter, so a hold took no lock: it committed
// while a PATCH that had already read "no usage" was still running, and the
// PATCH then changed the aggregation of a meter with a committed held row.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// pausedAudit pauses a meter PATCH inside its transaction: after the meter
// lock and the usage read, before the commit. It is the concurrent resolver:
// the hold races the PATCH exactly there.
type pausedAudit struct {
	inTx    chan struct{}
	release chan struct{}
}

func (p pausedAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	close(p.inTx)
	<-p.release
	return nil
}

// Mutation-verify: drop the meter lock from Hold and HoldCall → the hold
// returns while the PATCH is paused, and this fails.
func TestHold_WaitsForAnAggregationPatch(t *testing.T) {
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for name, hold := range map[string]func(f heldFixture, eventName string) error{
		"Hold": func(f heldFixture, eventName string) error {
			_, err := f.svc.Hold(f.ctx, f.tenantID, usage.HoldInput{
				Source: domain.HeldUsageSourceLiteLLM, ExternalCustomerID: "ext_later", EventName: eventName,
				Quantity: decimal.NewFromInt(5), IdempotencyKey: "lock-" + eventName, EventAt: &at,
				Reason: domain.HeldReasonCustomerNotFound,
			})
			return err
		},
		"HoldCall": func(f heldFixture, eventName string) error {
			_, err := f.svc.HoldCall(f.ctx, f.tenantID, usage.HoldCallInput{
				Source: domain.HeldUsageSourceLiteLLM, ExternalCustomerID: "ext_later", EventName: eventName,
				Reason: domain.HeldReasonCustomerNotFound,
				Events: []usage.CallEvent{
					{Quantity: decimal.NewFromInt(5), IdempotencyKey: "lock-" + eventName + ":input", Timestamp: &at},
					{Quantity: decimal.NewFromInt(7), IdempotencyKey: "lock-" + eventName + ":output", Timestamp: &at},
				},
			})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newHeldFixture(t, "Held meter lock "+name)
			eventName := "tokens_" + strings.ToLower(name)
			psvc := pricing.NewService(pricing.NewPostgresStore(f.db))
			m, err := psvc.CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
				EventName: eventName, Name: eventName, Unit: "tokens", Aggregation: "sum",
			})
			if err != nil {
				t.Fatalf("create meter: %v", err)
			}

			paused := pausedAudit{inTx: make(chan struct{}), release: make(chan struct{})}
			psvc.SetAuditLogger(paused)
			toMax := "max"
			patched := make(chan error, 1)
			go func() {
				_, err := psvc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: &toMax})
				patched <- err
			}()
			select {
			case <-paused.inTx:
			case err := <-patched:
				t.Fatalf("the PATCH finished before its audit write: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("the PATCH never reached its audit write")
			}

			held := make(chan error, 1)
			go func() { held <- hold(f, eventName) }()
			select {
			case err := <-held:
				close(paused.release)
				<-patched
				t.Fatalf("the hold returned (%v) while an aggregation PATCH that read no usage was still running; it must wait for the PATCH's meter lock", err)
			case <-time.After(500 * time.Millisecond):
			}

			close(paused.release)
			if err := <-patched; err != nil {
				t.Fatalf("PATCH: %v", err)
			}
			select {
			case err := <-held:
				if err != nil {
					t.Fatalf("hold after the PATCH: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the hold never finished")
			}

			// The held row came after the change, and now it fixes the
			// aggregation.
			psvc.SetAuditLogger(nil)
			sum := "sum"
			if _, err := psvc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Aggregation: &sum}); err == nil {
				t.Error("aggregation change with a held row: want refused")
			}
		})
	}
}
