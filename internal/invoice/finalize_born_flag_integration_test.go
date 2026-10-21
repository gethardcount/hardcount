package invoice

import (
	"encoding/json"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestFinalizeAuditRow_ServiceFinalizeIsNotBornFinalized is the negative
// half of P-1 (ADR-128): the finalize audit row of an invoice that was a
// draft first never carries born_finalized. Service.Finalize is that row's
// only writer, for the operator's Finalize and for the tax retry's
// auto-finalize alike. The engine's rows for invoices created finalized carry
// the flag (TestActivityDates_BornFinalized in internal/api).
func TestFinalizeAuditRow_ServiceFinalizeIsNotBornFinalized(t *testing.T) {
	r := newSnapRig(t, "Born Finalized Negative Corp")
	r.svc.SetAuditLogger(audit.NewLogger(r.db))

	draft, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, r.protoInvoice("P1-DRAFT", domain.InvoiceDraft, domain.BillingReasonManual), nil)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := r.svc.Finalize(r.ctx, r.tenantID, draft.ID); err != nil {
		t.Fatalf("operator finalize: %v", err)
	}

	taxDraft := r.taxPendingDraft(t, "P1-TAX")
	if inv, err := r.svc.RetryTax(r.ctx, r.tenantID, taxDraft); err != nil || inv.Status != domain.InvoiceFinalized {
		t.Fatalf("tax retry: status=%s err=%v, want finalized", inv.Status, err)
	}

	admin := testutil.AdminPool(t)
	for name, id := range map[string]string{"operator finalize": draft.ID, "tax-retry finalize": taxDraft} {
		rows, err := admin.Query(`SELECT metadata FROM audit_log WHERE resource_type = 'invoice' AND resource_id = $1 AND action = $2`, id, domain.AuditActionFinalize)
		if err != nil {
			t.Fatalf("%s: read audit rows: %v", name, err)
		}
		var metas []map[string]any
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			m := map[string]any{}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("%s: metadata %s: %v", name, raw, err)
			}
			metas = append(metas, m)
		}
		_ = rows.Close()
		if len(metas) != 1 {
			t.Fatalf("%s: %d finalize rows, want 1", name, len(metas))
		}
		if v, ok := metas[0]["born_finalized"]; ok {
			t.Errorf("%s: finalize row carries born_finalized=%v; the invoice was a draft first", name, v)
		}
	}
}
