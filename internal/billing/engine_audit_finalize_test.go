package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// fakeEngineAudit records the engine's finalize emissions. It captures the ctx
// CLOCK BINDING, not just the entry: the sim axis is resolved by the audit writer
// from ctx (audit.simColumns) and by nothing else, so "did this row land on the
// sim axis?" is a question about the ctx the emission ran under. Asserting
// hand-stamped metadata keys instead would pin a mechanism that no longer exists.
type fakeEngineAudit struct {
	rows []auditRow
	// inTx records whether the emission arrived via LogInTx (the shared-fate
	// path) rather than the post-commit Log. The finalize migration (ADR-090)
	// exists precisely to move this off Log, so the test pins the channel.
	inTx []bool
}

type auditRow struct {
	entry audit.Entry
	sim   clock.Sim
	bound bool
}

func (f *fakeEngineAudit) record(ctx context.Context, e audit.Entry, inTx bool) {
	sim, bound := clock.SimOf(ctx)
	f.rows = append(f.rows, auditRow{e, sim, bound})
	f.inTx = append(f.inTx, inTx)
}

// Log satisfies AuditWriter. The engine's finalize path no longer uses it (it
// emits in-tx via LogInTx); routing it to the same recorder means an accidental
// regression back to a post-commit Log still shows up in these assertions —
// flagged by inTx=false.
func (f *fakeEngineAudit) Log(ctx context.Context, _, action, resourceType, resourceID, resourceLabel string, meta map[string]any) error {
	f.record(ctx, audit.Entry{Action: action, ResourceType: resourceType, ResourceID: resourceID, ResourceLabel: resourceLabel, Metadata: meta}, false)
	return nil
}

func (f *fakeEngineAudit) LogInTx(ctx context.Context, _ *sql.Tx, e audit.Entry) error {
	f.record(ctx, e, true)
	return nil
}

// erroringEngineAudit fails the in-tx emission, so a test can prove the failure
// PROPAGATES (shared fate) rather than being swallowed the way the old
// post-commit Log discarded its error.
type erroringEngineAudit struct{}

func (erroringEngineAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}
func (erroringEngineAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("audit down")
}

// TestFinalizeAuditEmission pins the engine-side finalize-audit contract after
// the ADR-090 migration: an engine-created FINALIZED invoice writes exactly one
// finalize row, IN THE INVOICE'S TRANSACTION;
// drafts (tax-pending / pause-collection) are skipped — they get their row from
// service.Finalize when the tax-retry chain finalizes them, and writing here too
// would double-count; clock-pinned subs inherit the sim axis from ctx (ADR-030:
// created_at stays wall-clock), never from hand-stamped metadata.
func TestFinalizeAuditEmission(t *testing.T) {
	now := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	inv := domain.Invoice{
		ID: "vlx_inv_1", InvoiceNumber: "VLX-000042", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, TotalAmountCents: 12345, Currency: "USD",
		BillingReason: domain.BillingReasonSubscriptionCycle,
	}

	t.Run("finalized invoice → one IN-TX row with trigger", func(t *testing.T) {
		e, aud := &Engine{}, &fakeEngineAudit{}
		e.SetAuditLogger(aud)
		if err := e.emitFinalizeAuditTx(context.Background(), nil, inv); err != nil {
			t.Fatalf("emit: %v", err)
		}
		if len(aud.rows) != 1 {
			t.Fatalf("rows: got %d, want 1", len(aud.rows))
		}
		if !aud.inTx[0] {
			t.Error("finalize row emitted via post-commit Log, not LogInTx — ADR-090 shared fate regressed")
		}
		r := aud.rows[0]
		if r.entry.Action != string(domain.AuditActionFinalize) || r.entry.ResourceType != "invoice" || r.entry.ResourceID != "vlx_inv_1" {
			t.Errorf("entry = %+v, want finalize/invoice/vlx_inv_1", r.entry)
		}
		if r.entry.Metadata["triggered_by"] != string(domain.BillingReasonSubscriptionCycle) {
			t.Errorf("triggered_by: got %v, want subscription_cycle", r.entry.Metadata["triggered_by"])
		}
		if r.bound {
			t.Errorf("wall-clock sub emitted under a clock binding: %+v", r.sim)
		}
	})

	t.Run("draft invoice → no row (service.Finalize writes it later)", func(t *testing.T) {
		e, aud := &Engine{}, &fakeEngineAudit{}
		e.SetAuditLogger(aud)
		draft := inv
		draft.Status = domain.InvoiceDraft
		if err := e.emitFinalizeAuditTx(context.Background(), nil, draft); err != nil {
			t.Fatalf("emit: %v", err)
		}
		if len(aud.rows) != 0 {
			t.Fatalf("rows: got %d, want 0 for a draft", len(aud.rows))
		}
		if _, ok := finalizeAuditEntry(draft); ok {
			t.Error("finalizeAuditEntry returned ok=true for a draft — it must be skipped")
		}
	})

	// Under a catchup the ctx arrives ALREADY bound (testclock.RunCatchup binds
	// it once for the advance; the engine never binds). This emission must run
	// under that binding and must not hand-stamp the axis into metadata — the
	// writer owns those keys, and an emitter that also wrote them would be
	// silently overwritten (that is how the trial-end instant used to get lost).
	t.Run("clock-pinned sub under a catchup ctx → the row inherits the binding", func(t *testing.T) {
		e, aud := &Engine{}, &fakeEngineAudit{}
		e.SetAuditLogger(aud)
		catchup := clock.WithSim(context.Background(), clock.Sim{At: now, TestClockID: "tclk_9"})

		if err := e.emitFinalizeAuditTx(catchup, nil, inv); err != nil {
			t.Fatalf("emit: %v", err)
		}
		if len(aud.rows) != 1 {
			t.Fatalf("rows: got %d, want 1", len(aud.rows))
		}
		r := aud.rows[0]
		if !r.bound {
			t.Fatal("the emission ran on an UNBOUND ctx — the finalize row would land with NULL sim columns")
		}
		if r.sim.TestClockID != "tclk_9" || !r.sim.At.Equal(now) {
			t.Errorf("inherited sim = %+v, want {%s tclk_9}", r.sim, now)
		}
		for _, k := range []string{"sim_effective_at", "test_clock_id"} {
			if _, hand := r.entry.Metadata[k]; hand {
				t.Errorf("emitter hand-stamped %q — the audit writer owns that key and overwrites it", k)
			}
		}
	})

	t.Run("emit error PROPAGATES (shared fate — the invoice tx must roll back)", func(t *testing.T) {
		e := &Engine{}
		e.SetAuditLogger(erroringEngineAudit{})
		if err := e.emitFinalizeAuditTx(context.Background(), nil, inv); err == nil {
			t.Fatal("emitFinalizeAuditTx swallowed the audit error — the finalized invoice would commit with no record of it")
		}
	})

	t.Run("nil logger → no panic, no error (engine runs unaudited)", func(t *testing.T) {
		if err := (&Engine{}).emitFinalizeAuditTx(context.Background(), nil, inv); err != nil {
			t.Fatalf("nil-logger emit returned %v, want nil", err)
		}
	})
}
