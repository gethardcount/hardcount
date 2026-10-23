package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// recordingStopper is the Stripe leg of a void (StopCollection), recorded.
type recordingStopper struct{ stopped []string }

func (r *recordingStopper) StopCollection(_ context.Context, _ string, inv domain.Invoice) {
	r.stopped = append(r.stopped, inv.ID)
}

// ExpireCheckoutSessions is a write-off's leg; these tests void.
func (*recordingStopper) ExpireCheckoutSessions(context.Context, string, domain.Invoice) {}

// recordingDunningResolver is the dunning leg of a void, recorded.
type recordingDunningResolver struct {
	invoices    []string
	resolutions []domain.DunningResolution
}

func (r *recordingDunningResolver) ResolveByInvoice(_ context.Context, _, invoiceID string, resolution domain.DunningResolution) error {
	r.invoices = append(r.invoices, invoiceID)
	r.resolutions = append(r.resolutions, resolution)
	return nil
}

// Every committed void stops collection at Stripe and closes the invoice's
// dunning run, whichever entry voided it: the operator and dunning-resolve
// void (Service.Void) and the cancel or plan swap's relief void
// (VoidUnchangedTx, then AfterVoid once the change commits). Before, only the
// two HTTP handlers ran these legs, so a relief void left the invoice payable
// on a live Checkout session and its dunning run active.
//
// Mutation: drop either call from AfterVoid and both cases fail.
func TestVoid_EveryPathStopsAndResolvesOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Void Effects")
	store := invoice.NewPostgresStore(db)

	paths := []struct {
		name string
		void func(t *testing.T, svc *invoice.Service, id string)
	}{
		{"Service.Void", func(t *testing.T, svc *invoice.Service, id string) {
			if _, err := svc.Void(ctx, tenantID, id); err != nil {
				t.Fatalf("void: %v", err)
			}
		}},
		{"VoidUnchangedTx then AfterVoid", func(t *testing.T, svc *invoice.Service, id string) {
			snap, err := store.Get(ctx, tenantID, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer postgres.Rollback(tx)
			voided, err := svc.VoidUnchangedTx(ctx, tx, tenantID, snap)
			if err != nil {
				t.Fatalf("void in tx: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			svc.AfterVoid(ctx, tenantID, voided)
		}},
	}
	for i, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-VOID-FX-"+string(rune('A'+i)))
			stopper := &recordingStopper{}
			resolver := &recordingDunningResolver{}
			svc := invoice.NewService(store, nil, nil)
			svc.SetCollectionStopper(stopper)
			svc.SetDunningResolver(resolver)

			p.void(t, svc, inv.ID)

			if got, err := store.Get(ctx, tenantID, inv.ID); err != nil || got.Status != domain.InvoiceVoided {
				t.Fatalf("invoice status %s (%v), want voided", got.Status, err)
			}
			if len(stopper.stopped) != 1 || stopper.stopped[0] != inv.ID {
				t.Errorf("StopCollection calls = %v, want exactly one for %s: a voided invoice must not stay payable at Stripe", stopper.stopped, inv.ID)
			}
			if len(resolver.invoices) != 1 || resolver.invoices[0] != inv.ID || resolver.resolutions[0] != domain.ResolutionInvoiceVoided {
				t.Errorf("ResolveByInvoice calls = %v %v, want exactly one for %s as invoice_voided", resolver.invoices, resolver.resolutions, inv.ID)
			}
		})
	}
}
