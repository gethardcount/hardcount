package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/crypto"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestListInvoices_SearchByCustomerWithEncryption: GET /v1/invoices?search=
// matches the invoice number or the customer's name or email (#1483). With
// HARDCOUNT_ENCRYPTION_KEY set, as production requires, the customer's name
// and email are ciphertext in the database, and the search compared them with
// ILIKE: a name or an email found nothing, and "enc:", the ciphertext prefix,
// matched every invoice. Customers are now matched after decrypting, by the
// customer store, and the invoices by their ids. The same holds without a key.
//
// Mutation: put the ILIKE on customers.display_name/email back in the list
// query (or drop SetCustomerSearcher), and the name and email searches find
// nothing with the key, and "enc:" finds both invoices.
func TestListInvoices_SearchByCustomerWithEncryption(t *testing.T) {
	enc, err := crypto.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]*crypto.Encryptor{"encrypted": enc, "plaintext": nil} {
		t.Run(name, func(t *testing.T) {
			r := newSnapRigWith(t, "Search "+name, key) // customer "Buyer Display", ap@buyer.test
			r.svc.SetCustomerSearcher(r.customers)
			buyer := r.createFinalized(t, "SRCH-BUYER-1")

			other, err := r.customers.Create(r.ctx, r.tenantID, domain.Customer{
				ExternalID: "cus_other", DisplayName: "Zephyr Holdings", Email: "billing@zephyr.example",
			})
			if err != nil {
				t.Fatalf("create other customer: %v", err)
			}
			p := r.protoInvoice("SRCH-ZEPHYR-1", domain.InvoiceFinalized, domain.BillingReasonSubscriptionCycle)
			p.CustomerID = other.ID
			zephyr, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, nil)
			if err != nil {
				t.Fatalf("create other invoice: %v", err)
			}

			routes := NewHandler(r.svc, r.customers, r.settings).Routes()
			search := func(t *testing.T, q string) []string {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, "/?search="+url.QueryEscape(q), nil)
				ctx := context.WithValue(postgres.WithLivemode(req.Context(), false), auth.TestTenantIDKey(), r.tenantID)
				rr := httptest.NewRecorder()
				routes.ServeHTTP(rr, req.WithContext(ctx))
				if rr.Code != http.StatusOK {
					t.Fatalf("search %q: status %d, body=%s", q, rr.Code, rr.Body.String())
				}
				var body struct {
					Data []domain.Invoice `json:"data"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				var nums []string
				for _, inv := range body.Data {
					nums = append(nums, inv.InvoiceNumber)
				}
				sort.Strings(nums)
				return nums
			}

			for _, tc := range []struct {
				q    string
				want []string
			}{
				{"buyer disp", []string{buyer.InvoiceNumber}},    // name, any case
				{"AP@BUYER.TEST", []string{buyer.InvoiceNumber}}, // email, any case
				{"zephyr", []string{zephyr.InvoiceNumber}},       // name and email
				{"srch-zephyr", []string{zephyr.InvoiceNumber}},  // invoice number
				{"SRCH-", []string{buyer.InvoiceNumber, zephyr.InvoiceNumber}},
				{"enc:", nil}, // the ciphertext prefix matches no one
				{"nobody", nil},
			} {
				if got := search(t, tc.q); strings.Join(got, ",") != strings.Join(tc.want, ",") {
					t.Errorf("search %q = %v, want %v", tc.q, got, tc.want)
				}
			}
		})
	}
}
