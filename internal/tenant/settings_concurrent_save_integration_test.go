package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// changeRecorder keeps the field names of each settings audit row.
type changeRecorder struct {
	mu   sync.Mutex
	rows [][]string
}

func (c *changeRecorder) Log(_ context.Context, _, _, _, _, _ string, metadata map[string]any) error {
	changed, _ := metadata["changed"].(map[string]any)
	var fields []string
	for f := range changed {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, fields)
	return nil
}

// TestSettingsSave_ConcurrentSavesKeepEachOthersFields: two settings saves
// that run at the same time, each sending one field, both land. A save reads
// the stored settings, merges its body over them and writes them back. If one
// save reads before the other one writes, its write puts back the value the
// other changed, such as the old tax rate, which every later invoice would
// then use.
//
// An invoice being numbered holds the settings row (NextInvoiceNumberTx), so
// the test leaves one open while a tax-rate save and a company-name save
// queue behind it. On a tenant with no settings row yet, the numbering
// creates the row, and the two first saves queue behind that insert. Postgres
// does not promise which save goes first, so no check depends on the order.
//
// Mutation-verify: drop FOR UPDATE from Update's read. Both saves read before
// either writes, and the one that writes second puts back the other's old
// value.
func TestSettingsSave_ConcurrentSavesKeepEachOthersFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seeded bool
	}{
		{name: "stored settings", seeded: true},
		{name: "no settings row yet", seeded: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			admin := testutil.AdminPool(t)
			ctx := postgres.WithLivemode(context.Background(), false)
			tenantID := testutil.CreateTestTenant(t, db, "Settings Race")
			store := NewSettingsStore(db)
			h := NewSettingsHandler(store)

			if tc.seeded {
				rr, req := settingsRequest(t, tenantID, `{"tax_rate":8}`)
				h.upsert(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("seed tax rate: status %d body=%s", rr.Code, rr.Body.String())
				}
			}
			audit := &changeRecorder{}
			h.SetAuditLogger(audit)

			numbering, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
			if err != nil {
				t.Fatalf("begin numbering: %v", err)
			}
			defer postgres.Rollback(numbering)
			if _, err := store.NextInvoiceNumberTx(ctx, numbering, tenantID); err != nil {
				t.Fatalf("number an invoice: %v", err)
			}

			start := func(body string) <-chan *httptest.ResponseRecorder {
				done := make(chan *httptest.ResponseRecorder, 1)
				rr, req := settingsRequest(t, tenantID, body)
				go func() {
					h.upsert(rr, req)
					done <- rr
				}()
				return done
			}
			taxSave := start(`{"tax_rate":10}`)
			nameSave := start(`{"company_name":"Acme GmbH"}`)
			testutil.WaitForLockWaiters(t, ctx, admin, "tenant_settings", 2)
			if err := numbering.Commit(); err != nil {
				t.Fatalf("commit numbering: %v", err)
			}

			taxRR, nameRR := <-taxSave, <-nameSave
			for _, rr := range []*httptest.ResponseRecorder{taxRR, nameRR} {
				if rr.Code != http.StatusOK {
					t.Fatalf("save: status %d body=%s", rr.Code, rr.Body.String())
				}
			}

			got, err := store.Get(ctx, tenantID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.TaxRate != 10 || got.CompanyName != "Acme GmbH" {
				t.Errorf("stored tax_rate=%v company_name=%q, want 10 and Acme GmbH: one save undid the other",
					got.TaxRate, got.CompanyName)
			}
			if seq, err := store.NextInvoiceSequence(ctx, tenantID); err != nil || seq != 2 {
				t.Errorf("next invoice sequence = %d (err %v), want 2: a save must not move the counter", seq, err)
			}
			// The save that ran second answers with both fields, so the page
			// that sent it shows the other save's value too.
			bothInAnAnswer := false
			for _, rr := range []*httptest.ResponseRecorder{taxRR, nameRR} {
				var answer struct {
					TaxRate     float64 `json:"tax_rate"`
					CompanyName string  `json:"company_name"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &answer); err != nil {
					t.Fatalf("decode answer: %v", err)
				}
				bothInAnAnswer = bothInAnAnswer || (answer.TaxRate == 10 && answer.CompanyName == "Acme GmbH")
			}
			if !bothInAnAnswer {
				t.Errorf("no save answered with tax_rate 10 and company_name Acme GmbH: %s | %s", taxRR.Body, nameRR.Body)
			}
			// Each audit row lists only the field its save sent: its "before"
			// is the row it locked, not an older read.
			audit.mu.Lock()
			defer audit.mu.Unlock()
			rows := map[string]bool{}
			for _, r := range audit.rows {
				b, _ := json.Marshal(r)
				rows[string(b)] = true
			}
			if len(audit.rows) != 2 || !rows[`["tax_rate"]`] || !rows[`["company_name"]`] {
				t.Errorf("audit rows %v, want one [tax_rate] and one [company_name]", audit.rows)
			}
		})
	}
}
