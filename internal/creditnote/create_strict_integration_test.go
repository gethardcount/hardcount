package creditnote_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestCreateCreditNote_UnknownFieldRefused: `refund_amount` (a typo for
// refund_amount_cents) on a paid invoice was dropped by the decoder, so the
// operator who meant to refund got a credit-only note, a used-up number and a
// 201. POST /v1/credit-notes now refuses the unknown key with a 422 naming it
// before anything is written, and the number sequence does not move. The
// corrected body under the same Idempotency-Key then creates the note.
func TestCreateCreditNote_UnknownFieldRefused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Strict Create")
	_, inv := seedInvoiceForCN(t, db, tenantID, "strict", domain.InvoicePaid, domain.PaymentSucceeded, "")

	svc := creditnote.NewService(creditnote.NewPostgresStore(db), invoice.NewPostgresStore(db), nil)
	svc.SetNumberGenerator(tenant.NewSettingsStore(db))
	srv := middleware.Idempotency(db, nil)(creditnote.NewHandler(svc).Routes())

	post := func(body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)))
		return rr
	}
	state := func() (notes, nextSeq int) {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM credit_notes WHERE invoice_id = $1`, inv.ID).Scan(&notes); err != nil {
			t.Fatalf("count credit notes: %v", err)
		}
		err = tx.QueryRowContext(ctx, `SELECT credit_note_next_seq_test FROM tenant_settings WHERE tenant_id = $1`, tenantID).Scan(&nextSeq)
		if errors.Is(err, sql.ErrNoRows) {
			nextSeq = 1 // no settings row yet: the column default
		} else if err != nil {
			t.Fatalf("read credit note sequence: %v", err)
		}
		return notes, nextSeq
	}
	_, seqBefore := state()

	lines := `"lines":[{"description":"over-billed seat","quantity":1,"unit_amount_cents":3000}]`
	rr := post(`{"invoice_id":"`+inv.ID+`","reason":"order_change","refund_amount":3000,`+lines+`}`, "idem-cn-strict")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refund_amount: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if env.Error.Param != "refund_amount" {
		t.Errorf("param: got %q, want refund_amount", env.Error.Param)
	}
	if notes, seq := state(); notes != 0 || seq != seqBefore {
		t.Fatalf("refused create left notes=%d next_seq=%d, want 0 and %d (no row, no number used)", notes, seq, seqBefore)
	}

	// Control: the corrected body (credit allocation, the channel that needs
	// no payment provider) under the same key creates the note.
	rr = post(`{"invoice_id":"`+inv.ID+`","reason":"order_change","credit_amount_cents":3000,`+lines+`}`, "idem-cn-strict")
	if rr.Code != http.StatusCreated {
		t.Fatalf("corrected create: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	if notes, seq := state(); notes != 1 || seq != seqBefore+1 {
		t.Fatalf("after the corrected create notes=%d next_seq=%d, want 1 and %d", notes, seq, seqBefore+1)
	}
}
