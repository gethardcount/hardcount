package api

import (
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The "send" audit row of a credit note on a test clock carries that clock
// and its frozen time: the handler emits it on the request's context, which
// the API's door decided from the credit note (ofCreditNote).
func TestE2E_CreditNoteSend_AuditRowIsOnTheNotesClock(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.cn-send-audit.invalid")
	db, ts, _, register := newRefundServer(t)
	f := newRefundFlow(t, db, ts, "Credit note send audit Co")
	register(f)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clockID := flowStr(f.call("POST", "/v1/test-clocks", map[string]any{
		"name": "Send audit clock", "frozen_time": frozen.Format(time.RFC3339),
	}).want(t, 201, "create test clock").body, "id")
	f.customerID = flowStr(f.call("POST", "/v1/customers", map[string]any{
		"external_id": "cn-send-audit", "display_name": "Send audit", "email": "ap@send-audit.test", "test_clock_id": clockID,
	}).want(t, 201, "create customer on the clock").body, "id")
	invID, _ := f.paidInvoice("cn_send_audit", 10000, 0)
	cnID := flowStr(f.call("POST", "/v1/credit-notes", map[string]any{
		"invoice_id": invID, "reason": "goodwill", "auto_issue": true, "credit_amount_cents": 5000,
		"lines": []map[string]any{{"description": "Goodwill", "quantity": 1, "unit_amount_cents": 5000}},
	}).want(t, 201, "credit note").body, "id")
	f.call("POST", "/v1/credit-notes/"+cnID+"/send", map[string]any{"email": "ap@send-audit.test"}).want(t, 200, "send")

	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var (
		simAt sql.NullTime
		clk   sql.NullString
	)
	if err := tx.QueryRowContext(t.Context(), `
		SELECT sim_effective_at, test_clock_id FROM audit_log
		WHERE tenant_id = $1 AND resource_type = 'credit_note' AND resource_id = $2 AND action = 'send'`,
		f.tenantID, cnID).Scan(&simAt, &clk); err != nil {
		t.Fatalf("read the send row: %v", err)
	}
	if !clk.Valid || clk.String != clockID || !simAt.Valid || !simAt.Time.Equal(frozen) {
		t.Errorf("send row: test_clock_id %v, sim_effective_at %v; want %s at %s", clk, simAt, clockID, frozen)
	}
}
