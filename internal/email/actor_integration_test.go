package email

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestOutboxEnqueue_StampsActor: every email row records who caused it,
// from the request identity at the one insert chokepoint (Enqueue, which
// EnqueueStandalone also goes through), the way the audit log does
// (audit.ResolveActor). The activity lines name it later ("by
// sagar@…", "Automatic"); before this, an email row could only say who sent
// it when its type or trigger proved it (ADR-128 §4, PR 6).
func TestOutboxEnqueue_StampsActor(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outbox Actor")
	store := NewOutboxStore(db)
	admin := testutil.AdminPool(t)

	cases := []struct {
		name             string
		ctx              context.Context
		inTx             bool
		wantType, wantID string
	}{
		{"background worker", base, false, "system", "system"},
		{"dashboard operator", auth.WithUserID(base, "hc_usr_actor"), false, "user", "hc_usr_actor"},
		{"api key", auth.WithKeyID(base, "hc_key_actor"), false, "api_key", "hc_key_actor"},
		{"customer on a public page", auth.WithCustomerActor(base, "hc_cus_actor"), false, "customer", "hc_cus_actor"},
		{"in the business transaction", auth.WithUserID(base, "hc_usr_intx"), true, "user", "hc_usr_intx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"to": "billing@actor.test", "invoice_number": "ACT-1"}
			var id string
			var err error
			if tc.inTx {
				tx, terr := db.BeginTx(tc.ctx, postgres.TxTenant, tenantID)
				if terr != nil {
					t.Fatalf("begin: %v", terr)
				}
				defer postgres.Rollback(tx)
				id, err = store.Enqueue(tc.ctx, tx, tenantID, TypeInvoice, payload)
				if err == nil {
					err = tx.Commit()
				}
			} else {
				id, err = store.EnqueueStandalone(tc.ctx, tenantID, TypeInvoice, payload)
			}
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			var gotType, gotID *string
			if err := admin.QueryRowContext(context.Background(),
				`SELECT actor_type, actor_id FROM email_outbox WHERE id = $1`, id,
			).Scan(&gotType, &gotID); err != nil {
				t.Fatalf("read actor: %v", err)
			}
			if gotType == nil || gotID == nil || *gotType != tc.wantType || *gotID != tc.wantID {
				t.Errorf("actor = (%v, %v), want (%q, %q)", deref(gotType), deref(gotID), tc.wantType, tc.wantID)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<NULL>"
	}
	return *s
}
