package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A subscription starts now unless the caller asks for a draft (simplification
// audit D2, 2026-10-23). Before, POST /v1/subscriptions without start_now and
// without a trial made a draft that billed nothing until someone activated it,
// so a forgotten flag silently billed nothing.
//
// The golden files under testdata/starts_now were written on 210646e8 (before
// this change) by a create that sent "start_now": true through the same rig
// and the same normalization (TestCreateSubscription_StartNowTrueMatchesGolden,
// removed with start_now). A create with no flag must now produce those bytes
// exactly: the same subscription and the same day-1 in_advance invoice.
//
// The rig is the close-lag rig: the real server over HTTP, every collaborator
// wired as in production, each request served at a chosen instant. Plans are
// in_advance: plan_a (3000c base, a 1c/call meter) and plan_c (1000c base, no
// meter). The tenant zone is UTC.

var startsNowAt = time.Date(2026, 4, 10, 14, 30, 0, 0, time.UTC)

var updateStartsNowGolden = flag.Bool("update-starts-now-golden", false, "rewrite testdata/starts_now from the create under test")

// startsNowBody is a create body for the rig's customer on plan_a and plan_c.
// extra adds or overrides fields.
func (r *lagRig) startsNowBody(code, billingTime string, extra map[string]any) map[string]any {
	body := map[string]any{
		"code": code, "display_name": code, "customer_id": r.custID, "billing_time": billingTime,
		"items": []map[string]any{{"plan_id": r.planA, "quantity": 1}, {"plan_id": r.planC, "quantity": 2}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// doKey is r.do with an Idempotency-Key header.
func (r *lagRig) doKey(method, path, key string, at time.Time, body any) *httptest.ResponseRecorder {
	r.t.Helper()
	r.clk.Set(at)
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(clock.WithEffectiveNow(context.Background(), at))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", r.auth)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	r.srv.ServeHTTP(rec, req)
	return rec
}

// customerCounts returns how many subscriptions and how many invoices the
// rig's customer has, read from the database.
func (r *lagRig) customerCounts() (subs, invoices int) {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM subscriptions WHERE customer_id = $1`, r.custID).Scan(&subs); err != nil {
		r.t.Fatalf("count subscriptions: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invoices WHERE customer_id = $1`, r.custID).Scan(&invoices); err != nil {
		r.t.Fatalf("count invoices: %v", err)
	}
	return subs, invoices
}

var hcIDPattern = regexp.MustCompile(`^hc_[a-z]+_[0-9A-Za-z]+$`)

// normalizeIDs replaces every Hardcount object id in v with a placeholder
// numbered by first appearance, so two runs (or two tenants) compare equal
// when only their ids differ. Map keys are walked in sorted order.
func normalizeIDs(v any, seen map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			x[k] = normalizeIDs(x[k], seen)
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalizeIDs(x[i], seen)
		}
		return x
	case string:
		if !hcIDPattern.MatchString(x) {
			return x
		}
		if p, ok := seen[x]; ok {
			return p
		}
		p := fmt.Sprintf("<id %d>", len(seen)+1)
		seen[x] = p
		return p
	default:
		return v
	}
}

// startsNowSnapshot is the subscription and its invoices as the API returns
// them, ids normalized, as indented JSON.
func (r *lagRig) startsNowSnapshot(subID string) []byte {
	r.t.Helper()
	at := r.clk.Now(context.Background())
	code, raw := r.do(http.MethodGet, "/v1/subscriptions/"+subID, at, nil)
	if code != http.StatusOK {
		r.t.Fatalf("get subscription: %d %s", code, raw)
	}
	var sub any
	if err := json.Unmarshal(raw, &sub); err != nil {
		r.t.Fatalf("decode subscription: %v", err)
	}
	invs := []any{}
	for _, inv := range r.invoicesOf(subID) {
		code, raw := r.do(http.MethodGet, "/v1/invoices/"+inv.id, at, nil)
		if code != http.StatusOK {
			r.t.Fatalf("get invoice: %d %s", code, raw)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			r.t.Fatalf("decode invoice: %v", err)
		}
		invs = append(invs, doc)
	}
	// Items and lines written in one transaction share a created_at, so the
	// API may list them in either order. Sort them by what tells them apart
	// before ids are numbered.
	if m, ok := sub.(map[string]any); ok {
		sortByKey(m["items"], "quantity")
	}
	for _, doc := range invs {
		if m, ok := doc.(map[string]any); ok {
			sortByKey(m["line_items"], "description")
		}
	}
	seen := map[string]string{}
	snap := map[string]any{
		"subscription": normalizeIDs(sub, seen),
		"invoices":     normalizeIDs(invs, seen),
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		r.t.Fatalf("encode snapshot: %v", err)
	}
	return out.Bytes()
}

// sortByKey sorts a JSON array of objects by the printed value of key.
func sortByKey(v any, key string) {
	arr, ok := v.([]any)
	if !ok {
		return
	}
	sort.SliceStable(arr, func(i, j int) bool {
		a, _ := arr[i].(map[string]any)
		b, _ := arr[j].(map[string]any)
		return fmt.Sprint(a[key]) < fmt.Sprint(b[key])
	})
}

func startsNowGoldenPath(name string) string {
	return filepath.Join("testdata", "starts_now", name+".json")
}

// checkStartsNowGolden compares got with the golden file, or rewrites the file
// when -update-starts-now-golden is set.
func checkStartsNowGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := startsNowGoldenPath(name)
	if *updateStartsNowGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: the create does not match what start_now=true produced.\n--- got\n%s\n--- want (%s)\n%s", name, got, path, want)
	}
}

// TestCreateSubscription_StartsNowByDefault: a create with no start flag and no
// trial is active at once and bills its day-1 in_advance invoice, byte-equal
// to what start_now=true produced before the change. Calendar billing makes a
// stub to May 1 (prorated base); anniversary billing makes a full month.
func TestCreateSubscription_StartsNowByDefault(t *testing.T) {
	for _, billingTime := range []string{"calendar", "anniversary"} {
		t.Run(billingTime, func(t *testing.T) {
			r := newLagRigBare(t, "in_advance", 0)
			out := r.must(http.MethodPost, "/v1/subscriptions", startsNowAt, r.startsNowBody("starts-now", billingTime, nil), http.StatusCreated)
			if got := flowStr(out, "status"); got != string(domain.SubscriptionActive) {
				t.Fatalf("status = %q, want active", got)
			}
			subID := flowStr(out, "id")
			if subs, invs := r.customerCounts(); subs != 1 || invs != 1 {
				t.Fatalf("after one create: %d subscriptions and %d invoices, want 1 and 1", subs, invs)
			}
			checkStartsNowGolden(t, billingTime, r.startsNowSnapshot(subID))
		})
	}
}

// TestCreateSubscription_TrialStartsNow: a create with trial_days starts the
// trial at once, as before: trialing from now, the first paid period anchored
// on the trial end (ADR-037), and no invoice until the trial ends.
func TestCreateSubscription_TrialStartsNow(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	out := r.must(http.MethodPost, "/v1/subscriptions", startsNowAt, r.startsNowBody("trial-now", "calendar", map[string]any{"trial_days": 14}), http.StatusCreated)
	r.subID = flowStr(out, "id")
	s := r.sub()
	trialEnd := startsNowAt.AddDate(0, 0, 14)
	if s.Status != domain.SubscriptionTrialing {
		t.Fatalf("status = %s, want trialing", s.Status)
	}
	if s.TrialStartAt == nil || !s.TrialStartAt.Equal(startsNowAt) || s.TrialEndAt == nil || !s.TrialEndAt.Equal(trialEnd) {
		t.Fatalf("trial = [%v, %v), want [%v, %v)", s.TrialStartAt, s.TrialEndAt, startsNowAt, trialEnd)
	}
	if s.StartedAt == nil || !s.StartedAt.Equal(startsNowAt) || s.ActivatedAt != nil {
		t.Fatalf("started_at %v activated_at %v, want started now and not yet activated", s.StartedAt, s.ActivatedAt)
	}
	ps := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	pe := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if s.CurrentBillingPeriodStart == nil || !s.CurrentBillingPeriodStart.Equal(ps) || s.CurrentBillingPeriodEnd == nil || !s.CurrentBillingPeriodEnd.Equal(pe) {
		t.Fatalf("first paid period = [%v, %v), want [%v, %v)", s.CurrentBillingPeriodStart, s.CurrentBillingPeriodEnd, ps, pe)
	}
	if _, invs := r.customerCounts(); invs != 0 {
		t.Fatalf("a trial create wrote %d invoices, want 0", invs)
	}
}

// TestCreateSubscription_ZeroTrialDaysIsNoTrial: trial_days 0 is accepted and
// means no trial. A form whose trial field defaults to 0 sends it, so the spec
// says "trial_days above 0" makes a trialing subscription: with 0 the create
// is active and bills day 1, and with status "draft" it is a plain draft.
func TestCreateSubscription_ZeroTrialDaysIsNoTrial(t *testing.T) {
	t.Run("start now", func(t *testing.T) {
		r := newLagRigBare(t, "in_advance", 0)
		out := r.must(http.MethodPost, "/v1/subscriptions", startsNowAt, r.startsNowBody("zero-trial", "calendar", map[string]any{"trial_days": 0}), http.StatusCreated)
		if got := flowStr(out, "status"); got != string(domain.SubscriptionActive) {
			t.Fatalf("status = %q, want active", got)
		}
		r.subID = flowStr(out, "id")
		if s := r.sub(); s.TrialStartAt != nil || s.TrialEndAt != nil {
			t.Fatalf("trial = [%v, %v), want none", s.TrialStartAt, s.TrialEndAt)
		}
		if subs, invs := r.customerCounts(); subs != 1 || invs != 1 {
			t.Fatalf("%d subscriptions and %d invoices, want 1 and 1 (day-1 bill)", subs, invs)
		}
	})
	t.Run("draft", func(t *testing.T) {
		r := newLagRigBare(t, "in_advance", 0)
		out := r.must(http.MethodPost, "/v1/subscriptions", startsNowAt, r.startsNowBody("zero-trial-draft", "calendar", map[string]any{"status": "draft", "trial_days": 0}), http.StatusCreated)
		if got := flowStr(out, "status"); got != string(domain.SubscriptionDraft) {
			t.Fatalf("status = %q, want draft", got)
		}
		if subs, invs := r.customerCounts(); subs != 1 || invs != 0 {
			t.Fatalf("%d subscriptions and %d invoices, want 1 and 0", subs, invs)
		}
	})
}

// TestCreateSubscription_ExplicitDraftBillsNothingUntilActivated: status
// "draft" is the one way to create a subscription that does not start. It has
// no period, no billing run bills it, and activating it bills day 1 from the
// activation instant (Activate's ActivateDraftWithBill, unchanged).
func TestCreateSubscription_ExplicitDraftBillsNothingUntilActivated(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	out := r.must(http.MethodPost, "/v1/subscriptions", startsNowAt, r.startsNowBody("draft-1", "calendar", map[string]any{"status": "draft"}), http.StatusCreated)
	r.subID = flowStr(out, "id")
	s := r.sub()
	if s.Status != domain.SubscriptionDraft {
		t.Fatalf("status = %s, want draft", s.Status)
	}
	if s.CurrentBillingPeriodStart != nil || s.NextBillingAt != nil || s.StartedAt != nil || s.ActivatedAt != nil {
		t.Fatalf("a draft carries period %v next %v started %v activated %v, want none", s.CurrentBillingPeriodStart, s.NextBillingAt, s.StartedAt, s.ActivatedAt)
	}

	// A billing run a month later bills nothing for the draft.
	later := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	r.run(later)
	if _, invs := r.customerCounts(); invs != 0 {
		t.Fatalf("a draft was billed: %d invoices, want 0", invs)
	}

	// Activation starts it and bills day 1 from the activation instant.
	act := r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/activate", later, nil, http.StatusOK)
	if got := flowStr(act, "status"); got != string(domain.SubscriptionActive) {
		t.Fatalf("status after activate = %q, want active", got)
	}
	invs := r.invoices()
	if len(invs) != 1 {
		r.logInvoices(invs)
		t.Fatalf("after activate: %d invoices, want 1", len(invs))
	}
	if want := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC); !invs[0].start.Equal(want) {
		t.Fatalf("day-1 invoice starts %v, want the activation day %v", invs[0].start, want)
	}
}

// TestCreateSubscription_RefusesAmbiguousStart: a field that used to choose
// the start, or a start the API cannot honor, is a 422 naming it, and nothing
// is written.
func TestCreateSubscription_RefusesAmbiguousStart(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	cases := []struct {
		name, param string
		extra       map[string]any
	}{
		// start_now is gone: true is now the default and false is spelled
		// status "draft". A caller still sending it is told, not ignored.
		{"start_now true", "start_now", map[string]any{"start_now": true}},
		{"start_now false", "start_now", map[string]any{"start_now": false}},
		// A draft does not start, so it cannot start a trial; activating a
		// draft starts billing at once.
		{"draft with a trial", "trial_days", map[string]any{"status": "draft", "trial_days": 14}},
		// status takes only "draft" on create.
		{"status active", "status", map[string]any{"status": "active"}},
		{"status trialing", "status", map[string]any{"status": "trialing"}},
		// An empty or null status is not a third way to say "start now": a
		// template that left the value unset must not start and bill.
		{"status empty", "status", map[string]any{"status": ""}},
		{"status null", "status", map[string]any{"status": nil}},
	}
	for i, tc := range cases {
		code, raw := r.do(http.MethodPost, "/v1/subscriptions", startsNowAt, r.startsNowBody(fmt.Sprintf("refused-%d", i), "calendar", tc.extra))
		var body struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		if code != http.StatusUnprocessableEntity || body.Error.Param != tc.param {
			t.Errorf("%s: %d %s, want 422 naming %s", tc.name, code, raw, tc.param)
		}
	}
	if subs, invs := r.customerCounts(); subs != 0 || invs != 0 {
		t.Fatalf("refused creates wrote %d subscriptions and %d invoices, want none", subs, invs)
	}
}

// TestCreateSubscription_RetryCreatesOneAndBillsOnce: the create now bills on
// day 1, so a retried create must not make a second subscription or a second
// invoice. With the same Idempotency-Key the retry (serial or concurrent) gets
// the first answer; without a key the same code is refused with 409. Either
// way the customer has one subscription and one invoice.
func TestCreateSubscription_RetryCreatesOneAndBillsOnce(t *testing.T) {

	t.Run("serial, same key", func(t *testing.T) {
		r := newLagRigBare(t, "in_advance", 0)
		body := r.startsNowBody("retry-serial", "calendar", nil)
		first := r.doKey(http.MethodPost, "/v1/subscriptions", "idem-starts-now-1", startsNowAt, body)
		if first.Code != http.StatusCreated {
			t.Fatalf("first: %d %s", first.Code, first.Body.String())
		}
		again := r.doKey(http.MethodPost, "/v1/subscriptions", "idem-starts-now-1", startsNowAt.Add(time.Second), body)
		if again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("retry: %d replayed=%q %s, want the replayed 201", again.Code, again.Header().Get("Idempotent-Replayed"), again.Body.String())
		}
		if !bytes.Equal(first.Body.Bytes(), again.Body.Bytes()) {
			t.Fatalf("replay differs from the first answer:\n%s\n%s", first.Body.String(), again.Body.String())
		}
		if subs, invs := r.customerCounts(); subs != 1 || invs != 1 {
			t.Fatalf("after a keyed retry: %d subscriptions and %d invoices, want 1 and 1", subs, invs)
		}
		// No key, same code: refused, nothing more written.
		nokey := r.doKey(http.MethodPost, "/v1/subscriptions", "", startsNowAt.Add(2*time.Second), body)
		if nokey.Code != http.StatusConflict {
			t.Fatalf("key-less retry: %d %s, want 409", nokey.Code, nokey.Body.String())
		}
		if subs, invs := r.customerCounts(); subs != 1 || invs != 1 {
			t.Fatalf("after a key-less retry: %d subscriptions and %d invoices, want 1 and 1", subs, invs)
		}
	})

	t.Run("concurrent, same key", func(t *testing.T) {
		r := newLagRigBare(t, "in_advance", 0)
		body := r.startsNowBody("retry-concurrent", "calendar", nil)
		const n = 4
		codes := make([]int, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				codes[i] = r.doKey(http.MethodPost, "/v1/subscriptions", "idem-starts-now-2", startsNowAt, body).Code
			}(i)
		}
		close(start)
		wg.Wait()
		created := 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
			default:
				t.Fatalf("concurrent create answers %v: want only 201 (first or replay) and 409 (key in use)", codes)
			}
		}
		if created == 0 {
			t.Fatalf("concurrent create answers %v: no 201", codes)
		}
		if subs, invs := r.customerCounts(); subs != 1 || invs != 1 {
			t.Fatalf("after %d concurrent creates with one key: %d subscriptions and %d invoices, want 1 and 1", n, subs, invs)
		}
	})
}
