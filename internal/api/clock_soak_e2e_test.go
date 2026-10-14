package api

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/doctor"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_ClockSoak_MonthEndAnchorThroughYearBoundary is the stability plan's
// B2 soak (docs/dev/product-stability-plan.md): one subscription anchored on
// the WORST day of the month (the 31st), driven through 13 real cycle closes
// across a year boundary and two Februaries, through the REAL server — the
// test-clock advance endpoint, the catchup orchestrator, the billing engine —
// with the money-invariant doctor sweeping the whole database after every
// advance.
//
// The pure date math is already pinned by anniversary_month_end_test.go
// (ADR-055: Jan 31 → Feb 28 → Mar 31, anchor preserved). What that cannot
// prove is the PIPELINE: that the engine actually consults the anchored
// advance at cycle close, that every close produces exactly one invoice whose
// period ends where the math says, and that thirteen consecutive closes leave
// ZERO invariant violations behind. A drift here is a permanently shifted
// billing day for every month-end customer; a violation here is the class the
// doctor exists to catch at the moment it is created, not months later.
func TestE2E_ClockSoak_MonthEndAnchorThroughYearBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", "soak-bootstrap-token-1234")

	srv := NewServer(db, clock.NewFake(time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// The doctor needs a role that sees every row (RLS-scoped roles make it
	// silently blind — see internal/doctor).
	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// --- fixture: tenant, clock frozen Jan 31, pinned customer, $50/mo sub ---
	resp := doPost(t, ts, "/v1/bootstrap", "", map[string]any{
		"token": "soak-bootstrap-token-1234", "tenant_name": "Soak Co",
		"owner_email": "owner@soak.test", "owner_password": "a-real-12char-pw",
	})
	assertStatus(t, resp, 201)
	key, _ := readJSON(t, resp)["secret_key_test"].(string)
	if key == "" {
		t.Fatal("bootstrap returned no test key")
	}
	auth := "Bearer " + key

	start := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	resp = doPost(t, ts, "/v1/test-clocks", auth, map[string]any{
		"name": "soak clock", "frozen_time": start.Format(time.RFC3339),
	})
	assertStatus(t, resp, 201)
	clockID := readJSON(t, resp)["id"].(string)

	resp = doPost(t, ts, "/v1/customers", auth, map[string]any{
		"external_id": "soak_cust", "display_name": "Soak Cust",
		"email": "", "test_clock_id": clockID,
	})
	assertStatus(t, resp, 201)
	custID := readJSON(t, resp)["id"].(string)

	resp = doPost(t, ts, "/v1/plans", auth, map[string]any{
		"code": "soak-mo", "name": "Soak Monthly", "currency": "USD",
		"billing_interval": "monthly", "base_amount_cents": 5000, "status": "active",
	})
	assertStatus(t, resp, 201)
	planID := readJSON(t, resp)["id"].(string)

	resp = doPost(t, ts, "/v1/subscriptions", auth, map[string]any{
		"code": "soak-sub", "display_name": "Soak Sub", "customer_id": custID,
		"items": []map[string]any{{"plan_id": planID}}, "started_at": start.Format(time.RFC3339),
		"billing_time": "anniversary",
	})
	assertStatus(t, resp, 201)
	subID := readJSON(t, resp)["id"].(string)
	resp = doPost(t, ts, "/v1/subscriptions/"+subID+"/activate", auth, nil)
	assertStatus(t, resp, 200)

	// The expected period-end DATES for a day-31 anchor (ADR-055 clamp):
	// Feb 28 '26, Mar 31, Apr 30, May 31, Jun 30, Jul 31, Aug 31, Sep 30,
	// Oct 31, Nov 30, Dec 31, Jan 31 '27, Feb 28 '27, Mar 31 '27.
	// Both Februaries are non-leap; the anchor must clamp to 28 twice and
	// RESTORE to 31 after each — the ratchet this sequence exists to refute.
	ends := []time.Time{
		time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC),
	}

	assertPeriodEndDay := func(step int, wantDate time.Time) {
		t.Helper()
		resp := doGet(t, ts, "/v1/subscriptions/"+subID, auth)
		assertStatus(t, resp, 200)
		peRaw, _ := readJSON(t, resp)["current_billing_period_end"].(string)
		pe, err := time.Parse(time.RFC3339, peRaw)
		if err != nil {
			t.Fatalf("step %d: parse period end %q: %v", step, peRaw, err)
		}
		gy, gm, gd := pe.UTC().Date()
		wy, wm, wd := wantDate.Date()
		if gy != wy || gm != wm || gd != wd {
			t.Fatalf("step %d: period end = %04d-%02d-%02d, want %04d-%02d-%02d — the month-end anchor drifted (the exact ratchet ADR-055 exists to prevent)",
				step, gy, gm, gd, wy, wm, wd)
		}
	}

	// Activation must open the first period ending Feb 28.
	assertPeriodEndDay(0, ends[0])

	countInvoices := func() int {
		var n int
		if err := admin.QueryRow(`SELECT count(*) FROM invoices WHERE customer_id = $1`, custID).Scan(&n); err != nil {
			t.Fatalf("count invoices: %v", err)
		}
		return n
	}

	// advanceTo moves the customer's test clock and waits for the catchup
	// phases (which run asynchronously) to settle.
	advanceTo := func(step int, target time.Time) {
		t.Helper()
		resp := doPost(t, ts, "/v1/test-clocks/"+clockID+"/advance", auth, map[string]any{
			"frozen_time": target.Format(time.RFC3339),
		})
		assertStatus(t, resp, 200)
		deadline := time.Now().Add(60 * time.Second)
		for {
			resp := doGet(t, ts, "/v1/test-clocks/"+clockID, auth)
			status, _ := readJSON(t, resp)["status"].(string)
			if status == "ready" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("step %d: clock advance did not settle (status %q)", step, status)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	assertDoctorClean := func(step int) {
		t.Helper()
		res := doctor.Run(t.Context(), admin, doctor.Checks)
		for _, e := range res.Errors {
			t.Errorf("step %d: doctor check failed to run: %v", step, e)
		}
		for _, v := range res.Violations {
			t.Errorf("step %d: invariant violated: [%s/%s] %s %s", step, v.Check.Domain, v.Check.Name, v.RowID, v.Detail)
		}
		if t.Failed() {
			t.FailNow()
		}
	}

	// --- expected amounts, derived from this test's inputs (issue #988) ---
	//
	// Inputs: one item, quantity 1 (the default), on a plan with
	// base_amount_cents 5000, monthly, base_bill_timing omitted (pricing
	// defaults it to in_arrears), no meters, no coupon, no credit grant, and
	// no tax setup on a freshly bootstrapped tenant.
	//
	// Engine rules that decide the number:
	//   - in_arrears: each cycle close bills the period that just ENDED,
	//     so close i bills [periodStart_i, ends[i]] (periodStart_0 is the
	//     Jan 31 00:00 snap of the 10:00 start, ADR-012).
	//   - A base fee is prorated only when the billed days are FEWER than a
	//     full cycle measured from the period start (emitBaseSegmentLine:
	//     segDays < fullCycleDays, or periodDays < fullCycleDays). Every
	//     period here runs anchor boundary → anchor boundary, so it IS a full
	//     cycle by definition (ADR-055 clamps the 31st to Feb 28 / Apr 30 and
	//     restores it after). The days differ (28, 30, 31) but the fee is a
	//     flat monthly price, not a per-day one, so each period bills the
	//     whole 5000.
	//     Day counts checked by hand: Jan31→Feb28 = 28 of a 28-day cycle;
	//     Feb28→Mar31 = 31 days against a cycle of 28 if the anchor is
	//     re-derived from the clamped Feb 28 start (31 ≥ 28, so still not
	//     prorated); Mar31→Apr30 = 30/30; Apr30→May31 = 31 vs 30; and so on.
	//     No period is SHORTER than its cycle, so no proration fires.
	//   - So per invoice: one base_fee line, quantity 1, unit 5000, amount
	//     5000, no "prorated" in its description; subtotal = 5000 (sum of
	//     the one line); discount 0; tax 0 (no tax provider configured);
	//     total = 5000 + 0 - 0 = 5000.
	// The doctor only checks total = subtotal + tax - discount, which a
	// self-consistent wrong amount (e.g. a 31/28-prorated 5536) would pass.
	const (
		wantBaseCents     = int64(5000) // plan base_amount_cents × quantity 1
		wantLines         = 1           // base fee only: no meters, no tax lines
		wantDiscountCents = int64(0)
		wantTaxCents      = int64(0)
		wantSubtotalCents = wantBaseCents                                        // sum of the single line
		wantTotalCents    = wantSubtotalCents + wantTaxCents - wantDiscountCents // 5000
	)

	num := func(m map[string]any, k string) int64 {
		t.Helper()
		f, ok := m[k].(float64)
		if !ok {
			t.Fatalf("field %q missing or not a number: %v", k, m[k])
		}
		return int64(f)
	}
	sameDay := func(raw any, want time.Time) bool {
		s, _ := raw.(string)
		got, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return false
		}
		gy, gm, gd := got.UTC().Date()
		wy, wm, wd := want.Date()
		return gy == wy && gm == wm && gd == wd
	}

	seen := map[string]bool{}
	// newInvoiceID returns the one invoice that did not exist before this
	// step, failing if there is not exactly one.
	newInvoiceID := func(step int) string {
		t.Helper()
		rows, err := admin.Query(`SELECT id FROM invoices WHERE customer_id = $1`, custID)
		if err != nil {
			t.Fatalf("list invoices: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var fresh []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan invoice id: %v", err)
			}
			if !seen[id] {
				fresh = append(fresh, id)
				seen[id] = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate invoices: %v", err)
		}
		if len(fresh) != 1 {
			t.Fatalf("step %d: %d new invoices, want 1 (%v)", step, len(fresh), fresh)
		}
		return fresh[0]
	}

	// assertInvoiceAmounts reads the invoice through the API (what a caller
	// sees) and pins its money fields and lines to the derived numbers.
	assertInvoiceAmounts := func(step int, id string, periodStart, periodEnd time.Time) {
		t.Helper()
		resp := doGet(t, ts, "/v1/invoices/"+id, auth)
		assertStatus(t, resp, 200)
		body := readJSON(t, resp)
		inv, ok := body["invoice"].(map[string]any)
		if !ok {
			t.Fatalf("step %d: response has no invoice object: %v", step, body)
		}
		if !sameDay(inv["billing_period_start"], periodStart) || !sameDay(inv["billing_period_end"], periodEnd) {
			t.Errorf("step %d: invoice period = %v → %v, want %s → %s (in_arrears bills the period that just closed)",
				step, inv["billing_period_start"], inv["billing_period_end"], periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
		}
		if got := num(inv, "subtotal_cents"); got != wantSubtotalCents {
			t.Errorf("step %d: subtotal_cents = %d, want %d", step, got, wantSubtotalCents)
		}
		if got := num(inv, "discount_cents"); got != wantDiscountCents {
			t.Errorf("step %d: discount_cents = %d, want %d", step, got, wantDiscountCents)
		}
		if got := num(inv, "tax_amount_cents"); got != wantTaxCents {
			t.Errorf("step %d: tax_amount_cents = %d, want %d", step, got, wantTaxCents)
		}
		if got := num(inv, "total_amount_cents"); got != wantTotalCents {
			t.Errorf("step %d: total_amount_cents = %d, want %d", step, got, wantTotalCents)
		}
		items, ok := body["line_items"].([]any)
		if !ok {
			t.Fatalf("step %d: line_items missing or not an array: %v", step, body["line_items"])
		}
		if len(items) != wantLines {
			t.Fatalf("step %d: %d line items, want %d: %v", step, len(items), wantLines, items)
		}
		li := items[0].(map[string]any)
		if lt, _ := li["line_type"].(string); lt != "base_fee" {
			t.Errorf("step %d: line_type = %q, want base_fee", step, lt)
		}
		if got := num(li, "quantity"); got != 1 {
			t.Errorf("step %d: line quantity = %d, want 1", step, got)
		}
		if got := num(li, "unit_amount_cents"); got != wantBaseCents {
			t.Errorf("step %d: line unit_amount_cents = %d, want %d", step, got, wantBaseCents)
		}
		if got := num(li, "amount_cents"); got != wantBaseCents {
			t.Errorf("step %d: line amount_cents = %d, want %d", step, got, wantBaseCents)
		}
		if d, _ := li["description"].(string); strings.Contains(d, "prorated") {
			t.Errorf("step %d: line description %q says prorated, but every period is a full anchored cycle", step, d)
		}
	}

	periodStart := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC) // ADR-012 snap of the 10:00 start
	for i := 0; i < len(ends)-1; i++ {
		step := i + 1
		advanceTo(step, ends[i].Add(26*time.Hour)) // past boundary i, well short of boundary i+1

		// One close per advance: the new open period must end at ends[i+1]…
		assertPeriodEndDay(step, ends[i+1])
		// …exactly one more invoice must exist (the closed period's)…
		if got, want := countInvoices(), step; got != want {
			t.Fatalf("step %d: invoice count = %d, want %d — a close either skipped or double-billed", step, got, want)
		}
		// …billing exactly the derived amount for the period that closed…
		assertInvoiceAmounts(step, newInvoiceID(step), periodStart, ends[i])
		periodStart = ends[i]
		// …and the WHOLE database must still satisfy every money invariant.
		assertDoctorClean(step)
	}

	// Negative control (issue #988 item 3): advance again WITHOUT crossing
	// the open period's end (Mar 31 '27). The clock sits at Mar 1 '27 02:00;
	// Mar 30 '27 12:00 is past it and still 12h short of the boundary. No
	// close is due, so no invoice may appear and the period must not move.
	final := len(ends)
	advanceTo(final, ends[len(ends)-1].Add(-12*time.Hour))
	assertPeriodEndDay(final, ends[len(ends)-1])
	if got, want := countInvoices(), len(ends)-1; got != want {
		t.Fatalf("step %d: invoice count = %d after an advance that crossed no period end, want %d", final, got, want)
	}
	assertDoctorClean(final)

	t.Logf("soak passed: 13 closes at 5000 cents each, Jan 31 anchor held through two Februaries and a year boundary, a no-boundary advance billed nothing, doctor clean after every advance")
}
