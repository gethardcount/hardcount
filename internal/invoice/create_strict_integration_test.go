package invoice_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestCreateInvoice_UnknownFieldRefused: `amount_cents` on a composer line
// (the line's total, not its unit price) was dropped by the decoder. POST
// /v1/invoices and POST /v1/invoices/{id}/line-items now refuse an unknown
// key, at any depth, with a 422 naming it, before an invoice number is spent.
func TestCreateInvoice_UnknownFieldRefused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Invoice Strict Create")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_inv_strict", DisplayName: "Strict"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	svc := invoice.NewService(invoice.NewPostgresStore(db), nil, tenant.NewSettingsStore(db))
	srv := invoice.NewHandler(svc, nil, nil).Routes()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)))
		return rr
	}
	param := func(rr *httptest.ResponseRecorder) string {
		var env struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		return env.Error.Param
	}
	state := func() (invoices, lines, nextSeq int) {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invoices WHERE customer_id = $1`, cust.ID).Scan(&invoices); err != nil {
			t.Fatalf("count invoices: %v", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invoice_line_items li JOIN invoices i ON i.id = li.invoice_id WHERE i.customer_id = $1`, cust.ID).Scan(&lines); err != nil {
			t.Fatalf("count lines: %v", err)
		}
		err = tx.QueryRowContext(ctx, `SELECT invoice_next_seq_test FROM tenant_settings WHERE tenant_id = $1`, tenantID).Scan(&nextSeq)
		if errors.Is(err, sql.ErrNoRows) {
			nextSeq = 1 // no settings row yet: the column default
		} else if err != nil {
			t.Fatalf("read invoice sequence: %v", err)
		}
		return invoices, lines, nextSeq
	}
	_, _, seqBefore := state()

	head := `"customer_id":"` + cust.ID + `","currency":"USD"`
	rr := post("/", `{`+head+`,"line_items":[{"description":"setup","line_type":"add_on","quantity":1,"amount_cents":5000}]}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("line_items[0].amount_cents: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if p := param(rr); p != "amount_cents" {
		t.Errorf("param: got %q, want amount_cents", p)
	}
	if n, _, seq := state(); n != 0 || seq != seqBefore {
		t.Fatalf("refused create left invoices=%d next_seq=%d, want 0 and %d", n, seq, seqBefore)
	}

	// Control: the known body is created.
	rr = post("/", `{`+head+`,"line_items":[{"description":"setup","line_type":"add_on","quantity":1,"unit_amount_cents":5000}]}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("known body: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	var inv domain.Invoice
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatalf("decode invoice: %v", err)
	}

	// The add-line route on the same draft refuses an unknown key too.
	rr = post("/"+inv.ID+"/line-items", `{"description":"extra","line_type":"add_on","quantity":1,"unit_amount_cents":100,"amount":100}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("line-items with amount: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if p := param(rr); p != "amount" {
		t.Errorf("line-items param: got %q, want amount", p)
	}
	if n, lines, _ := state(); n != 1 || lines != 1 {
		t.Fatalf("after the refused add-line invoices=%d lines=%d, want 1 and 1", n, lines)
	}
}
