package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// C10 for the item writers that take no explicit plan lock (D08 §4.21).
// A quantity change and an item removal reference one plan, the item's own,
// and leave its lock to their foreign-key checks: the change row the item
// trigger writes. That is safe only while the item write comes before the
// proration invoice or the clawback credit note, which take a number from
// the tenant's sequence row. If the number came first, three requests could
// wait on each other:
//
//   - W, a draft activation on plan hi, holds hi and is about to number its
//     day-1 invoice;
//   - the meter edit (D05) holds lo and waits for hi;
//   - the item change on lo holds the sequence row and waits for lo;
//   - W waits for the sequence row.
//
// Postgres would abort one of them with a deadlock error (40P01). The test
// runs the real request over HTTP and stages W at the store, stopped between
// its plan lock and its number, as the cross-interval swap test does
// (internal/billing).
func TestE2E_ItemChange_ReachesItsPlanBeforeTheNumberRow(t *testing.T) {
	skipShort(t)
	changeAt := lagP0.AddDate(0, 0, 10)
	cases := []struct {
		name string
		// quantity is the item's quantity at create.
		quantity int
		// waitsIn is text from the item write, where the change waits for lo.
		waitsIn string
		write   func(r *lagRig, itemID string) (string, string, any, int)
		// numbered checks that the change wrote what takes the number.
		numbered func(r *lagRig, fundingID string)
	}{
		{"quantity increase", 1, "SET quantity = $1, updated_at",
			func(r *lagRig, itemID string) (string, string, any, int) {
				return http.MethodPatch, "/v1/subscriptions/" + r.subID + "/items/" + itemID, map[string]any{"quantity": 3}, http.StatusOK
			},
			func(r *lagRig, _ string) { r.prorationInvoice() },
		},
		{"quantity decrease", 3, "SET quantity = $1, updated_at",
			func(r *lagRig, itemID string) (string, string, any, int) {
				return http.MethodPatch, "/v1/subscriptions/" + r.subID + "/items/" + itemID, map[string]any{"quantity": 1}, http.StatusOK
			},
			func(r *lagRig, fundingID string) {
				if n := r.creditNotesOn(fundingID); n != 1 {
					r.t.Errorf("clawback credit notes on the funding invoice = %d, want 1", n)
				}
			},
		},
		{"item removal", 1, "subscription_items SET deleted_at",
			func(r *lagRig, itemID string) (string, string, any, int) {
				return http.MethodDelete, "/v1/subscriptions/" + r.subID + "/items/" + itemID, nil, http.StatusNoContent
			},
			func(r *lagRig, fundingID string) {
				if n := r.creditNotesOn(fundingID); n != 1 {
					r.t.Errorf("clawback credit notes on the funding invoice = %d, want 1", n)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newLagRigBare(t, "in_advance", 0)
			admin := testutil.AdminPool(t)
			ctx := postgres.WithLivemode(context.Background(), false)

			// planA and planB both list the meter; lo is the lower id.
			lo, hi := r.planA, r.planB
			var first string
			if err := admin.QueryRowContext(ctx, `SELECT id FROM plans WHERE id IN ($1, $2) ORDER BY id LIMIT 1`, lo, hi).Scan(&first); err != nil {
				t.Fatalf("order plans: %v", err)
			}
			if first != lo {
				lo, hi = hi, lo
			}

			// The changed subscription: an item on lo and an add-on (an active
			// subscription keeps at least one item), its first invoice paid so
			// a decrease or a removal is credited.
			sub := r.must(http.MethodPost, "/v1/subscriptions", lagP0, map[string]any{
				"code": "lockord-sub", "display_name": "Lock order", "customer_id": r.custID,
				"items": []map[string]any{{"plan_id": lo, "quantity": c.quantity}, {"plan_id": r.planC, "quantity": 1}},
			}, http.StatusCreated)
			r.subID = sub["id"].(string)
			var itemID string
			for _, it := range r.sub().Items {
				if it.PlanID == lo {
					itemID = it.ID
				}
			}
			r.payFirstInvoice(lagP0.Add(time.Hour))
			fundingID := r.firstInvoiceID()

			// W's subscription: another customer's draft on hi.
			custB := r.must(http.MethodPost, "/v1/customers", lagP0, map[string]any{
				"external_id": "lockord_b", "display_name": "Lock order B",
			}, http.StatusCreated)
			subStore := subscription.NewPostgresStore(r.db)
			draft, err := subStore.Create(ctx, r.tenantID, domain.Subscription{
				Code: "lockord-draft", DisplayName: "Draft", CustomerID: custB["id"].(string),
				Items:  []domain.SubscriptionItem{{PlanID: hi, Quantity: 1}},
				Status: domain.SubscriptionDraft, BillingTime: domain.BillingTimeCalendar,
			})
			if err != nil {
				t.Fatalf("create draft: %v", err)
			}
			r.clk.Set(changeAt)

			// 1. W activates the draft: it holds hi and stops before its number.
			numbers := tenant.NewSettingsStore(r.db)
			opCtx := clock.WithEffectiveNow(ctx, changeAt)
			wHolds, wGo := make(chan struct{}), make(chan struct{})
			var release sync.Once
			releaseW := func() { release.Do(func() { close(wGo) }) }
			// A failed wait below must not leave W holding its transaction.
			defer releaseW()
			wDone := make(chan error, 1)
			go func() {
				_, err := subStore.ActivateDraftWithBill(opCtx, r.tenantID, draft.ID, changeAt, changeAt, lagB, 1, func(tx *sql.Tx, _ domain.Subscription) error {
					close(wHolds)
					<-wGo
					_, err := numbers.NextInvoiceNumberTx(opCtx, tx, r.tenantID)
					return err
				})
				wDone <- err
			}()
			select {
			case <-wHolds:
			case err := <-wDone:
				t.Fatalf("activation finished before its number: %v", err)
			case <-time.After(15 * time.Second):
				t.Fatal("the activation never reached its number")
			}

			// 2. The meter edit locks lo and waits for hi.
			type resp struct {
				code int
				body []byte
			}
			editDone := make(chan resp, 1)
			go func() {
				code, body := r.do(http.MethodPatch, "/v1/meters/"+r.meterID, changeAt, map[string]any{"aggregation": "max"})
				editDone <- resp{code, body}
			}()
			testutil.WaitForLockWaiter(t, ctx, admin, "meter_ids @>")

			// 3. The item change waits for lo in its item write.
			method, path, body, want := c.write(r, itemID)
			writeDone := make(chan resp, 1)
			go func() {
				code, raw := r.do(method, path, changeAt, body)
				writeDone <- resp{code, raw}
			}()
			testutil.WaitForLockWaiter(t, ctx, admin, c.waitsIn)

			// 4. W numbers its invoice.
			releaseW()

			var wErr error
			var edit, write resp
			for range 3 {
				select {
				case wErr = <-wDone:
				case edit = <-editDone:
				case write = <-writeDone:
				case <-time.After(20 * time.Second):
					t.Fatal("the activation, the meter edit or the item change never finished")
				}
			}
			// The server answers a deadlock with a 500 and logs the 40P01.
			if wErr != nil {
				t.Fatalf("activation (deadlock: %v): %v", postgres.IsDeadlock(wErr), wErr)
			}
			if write.code != want {
				t.Fatalf("item change %s %s: %d %s, want %d (a 500 is the deadlock in the server log)", method, path, write.code, write.body, want)
			}
			if edit.code >= 300 && !strings.Contains(string(edit.body), errs.CodeMeterInUse) {
				t.Fatalf("meter edit: want success or %s, got %d %s (a 500 is the deadlock in the server log)", errs.CodeMeterInUse, edit.code, edit.body)
			}
			c.numbered(r, fundingID)
		})
	}
}
