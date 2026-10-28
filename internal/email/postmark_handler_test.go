package email

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type markCall struct{ tenantID, id, state string }

type fakeDeliveryStore struct {
	rows    map[string]OutboxRow
	marked  []markCall
	getErr  error
	markErr error
	gets    int
}

func (f *fakeDeliveryStore) GetByID(_ context.Context, id string) (OutboxRow, error) {
	f.gets++
	if f.getErr != nil {
		return OutboxRow{}, f.getErr
	}
	row, ok := f.rows[id]
	if !ok {
		return OutboxRow{}, sql.ErrNoRows
	}
	return row, nil
}

func (f *fakeDeliveryStore) MarkDeliveryState(_ context.Context, tenantID, id, state string) (bool, error) {
	if f.markErr != nil {
		return false, f.markErr
	}
	f.marked = append(f.marked, markCall{tenantID, id, state})
	return true, nil
}

type supCall struct{ tenantID, email, reason string }

type fakeSuppressor struct {
	bounced    []supCall
	complained []supCall
	err        error
}

func (f *fakeSuppressor) SuppressBounced(_ context.Context, tenantID, email, reason string) error {
	if f.err != nil {
		return f.err
	}
	f.bounced = append(f.bounced, supCall{tenantID, email, reason})
	return nil
}

func (f *fakeSuppressor) SuppressComplained(_ context.Context, tenantID, email, reason string) error {
	if f.err != nil {
		return f.err
	}
	f.complained = append(f.complained, supCall{tenantID, email, reason})
	return nil
}

const testOutboxID = "hc_emob_00112233445566778899aabb"

func testRow() OutboxRow {
	return OutboxRow{
		ID: testOutboxID, TenantID: "ten_1", Livemode: true,
		EmailType: TypeInvoice,
		Payload:   map[string]any{"to": "ap@acme.test", "invoice_number": "INV-9"},
		Status:    OutboxDispatched, DeliveryState: DeliveryUnknown,
	}
}

func postmarkPost(t *testing.T, h *PostmarkHandler, body string, auth func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/postmark", strings.NewReader(body))
	if auth != nil {
		auth(req)
	}
	rec := httptest.NewRecorder()
	h.HandleWebhook(rec, req)
	return rec
}

func goodAuth(r *http.Request) { r.SetBasicAuth("pmuser", "pmpass") }

func deliveryJSON(recipient string) string {
	return `{"RecordType":"Delivery","MessageID":"m-1","Recipient":"` + recipient + `",` +
		`"Metadata":{"hc-outbox-id":"` + testOutboxID + `"},"DeliveredAt":"2026-07-19T10:00:00Z"}`
}

func bounceJSON(email, bounceType string, typeCode int, inactive bool) string {
	inact := "false"
	if inactive {
		inact = "true"
	}
	return `{"RecordType":"Bounce","ID":42,"MessageID":"m-2","Email":"` + email + `",` +
		`"Type":"` + bounceType + `","TypeCode":` + strconv.Itoa(typeCode) + `,"Inactive":` + inact + `,` +
		`"Description":"The server was unable to deliver your message","Metadata":{"hc-outbox-id":"` + testOutboxID + `"}}`
}

// TestPostmarkWebhook_Auth locks the gate: bad or absent credentials are
// 401 (retryable — visible in Postmark's activity feed, never a silent
// drop), and an unconfigured handler rejects everything rather than
// standing open.
func TestPostmarkWebhook_Auth(t *testing.T) {
	store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}}
	h := NewPostmarkHandler(store, "pmuser", "pmpass")

	if rec := postmarkPost(t, h, deliveryJSON("ap@acme.test"), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth: got %d, want 401", rec.Code)
	}
	if rec := postmarkPost(t, h, deliveryJSON("ap@acme.test"), func(r *http.Request) {
		r.SetBasicAuth("pmuser", "wrong")
	}); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong pass: got %d, want 401", rec.Code)
	}
	if store.gets != 0 {
		t.Error("unauthenticated request must never touch the store")
	}

	unconfigured := NewPostmarkHandler(store, "", "")
	if rec := postmarkPost(t, unconfigured, deliveryJSON("ap@acme.test"), func(r *http.Request) {
		r.SetBasicAuth("", "")
	}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unconfigured creds: got %d, want 401 (never an open endpoint)", rec.Code)
	}
}

// TestPostmarkWebhook_Delivery: a Delivery for the PRIMARY recipient
// promotes delivery_state under the ROW's tenant; a Delivery naming a CC
// address must not flip the row (per-recipient attribution, ADR-082).
func TestPostmarkWebhook_Delivery(t *testing.T) {
	store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}}
	sup := &fakeSuppressor{}
	h := NewPostmarkHandler(store, "pmuser", "pmpass")
	h.SetSuppressor(sup)

	if rec := postmarkPost(t, h, deliveryJSON("AP@ACME.TEST"), goodAuth); rec.Code != http.StatusOK {
		t.Fatalf("delivery: got %d, want 200", rec.Code)
	}
	if len(store.marked) != 1 || store.marked[0] != (markCall{"ten_1", testOutboxID, DeliveryDelivered}) {
		t.Errorf("delivery mark: got %+v (recipient match must be case-insensitive, tenant from ROW)", store.marked)
	}
	if len(sup.bounced)+len(sup.complained) != 0 {
		t.Error("delivery must never touch suppression")
	}

	store.marked = nil
	if rec := postmarkPost(t, h, deliveryJSON("finance@acme.test"), goodAuth); rec.Code != http.StatusOK {
		t.Fatalf("cc delivery: got %d, want 200", rec.Code)
	}
	if len(store.marked) != 0 {
		t.Errorf("a CC-address delivery must not flip the row's delivery_state: %+v", store.marked)
	}
}

// TestPostmarkWebhook_Bounce: a permanent bounce (provider verdict —
// HardBounce or Inactive) writes delivery_state AND suppresses the
// EVENT's address; a soft/transient bounce writes NOTHING (every major
// ESP retries-then-surfaces, never suppresses); a CC-address bounce
// suppresses the CC without flipping the row.
func TestPostmarkWebhook_Bounce(t *testing.T) {
	store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}}
	sup := &fakeSuppressor{}
	h := NewPostmarkHandler(store, "pmuser", "pmpass")
	h.SetSuppressor(sup)

	if rec := postmarkPost(t, h, bounceJSON("ap@acme.test", "HardBounce", 1, true), goodAuth); rec.Code != http.StatusOK {
		t.Fatalf("hard bounce: got %d, want 200", rec.Code)
	}
	if len(store.marked) != 1 || store.marked[0].state != DeliveryBounced {
		t.Errorf("hard bounce mark: got %+v", store.marked)
	}
	if len(sup.bounced) != 1 || sup.bounced[0].tenantID != "ten_1" || sup.bounced[0].email != "ap@acme.test" {
		t.Fatalf("hard bounce suppression: got %+v (tenant must come from the ROW)", sup.bounced)
	}
	if !strings.HasPrefix(sup.bounced[0].reason, "postmark HardBounce") {
		t.Errorf("bounce reason should name the provider verdict: %q", sup.bounced[0].reason)
	}

	store.marked, sup.bounced = nil, nil
	if rec := postmarkPost(t, h, bounceJSON("ap@acme.test", "SoftBounce", 4096, false), goodAuth); rec.Code != http.StatusOK {
		t.Fatalf("soft bounce: got %d, want 200", rec.Code)
	}
	if len(store.marked)+len(sup.bounced) != 0 {
		t.Errorf("soft bounce must write NOTHING: marked=%+v suppressed=%+v", store.marked, sup.bounced)
	}

	if rec := postmarkPost(t, h, bounceJSON("finance@acme.test", "HardBounce", 1, true), goodAuth); rec.Code != http.StatusOK {
		t.Fatalf("cc bounce: got %d, want 200", rec.Code)
	}
	if len(store.marked) != 0 {
		t.Errorf("a CC-address bounce must not flip the row's delivery_state: %+v", store.marked)
	}
	if len(sup.bounced) != 1 || sup.bounced[0].email != "finance@acme.test" {
		t.Errorf("cc bounce must suppress the CC address itself: %+v", sup.bounced)
	}
}

// TestPostmarkWebhook_SpamComplaint: complained is the top of the
// lattice — delivery_state AND the per-cause complaint suppression path
// (never folded into bounce).
func TestPostmarkWebhook_SpamComplaint(t *testing.T) {
	store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}}
	sup := &fakeSuppressor{}
	h := NewPostmarkHandler(store, "pmuser", "pmpass")
	h.SetSuppressor(sup)

	body := `{"RecordType":"SpamComplaint","ID":7,"MessageID":"m-3","Email":"ap@acme.test",` +
		`"Type":"SpamComplaint","TypeCode":512,"Inactive":true,` +
		`"Metadata":{"hc-outbox-id":"` + testOutboxID + `"}}`
	if rec := postmarkPost(t, h, body, goodAuth); rec.Code != http.StatusOK {
		t.Fatalf("complaint: got %d, want 200", rec.Code)
	}
	if len(store.marked) != 1 || store.marked[0].state != DeliveryComplained {
		t.Errorf("complaint mark: got %+v", store.marked)
	}
	if len(sup.complained) != 1 || len(sup.bounced) != 0 {
		t.Errorf("complaint must use the complaint path, not bounce: complained=%+v bounced=%+v",
			sup.complained, sup.bounced)
	}
}

// TestPostmarkWebhook_DeterministicDrops: authenticated but
// deterministically-unprocessable payloads are 200-acked (a retry cannot
// fix them; Delivery retries only ~3x/21min) with nothing written —
// and NEVER a guessed tenant.
func TestPostmarkWebhook_DeterministicDrops(t *testing.T) {
	store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}}
	h := NewPostmarkHandler(store, "pmuser", "pmpass")

	cases := map[string]string{
		"unknown record type": `{"RecordType":"Open","MessageID":"m"}`,
		"bad json":            `{"RecordType":`,
		"no metadata":         `{"RecordType":"Delivery","Recipient":"ap@acme.test"}`,
		"unknown outbox id":   `{"RecordType":"Delivery","Recipient":"ap@acme.test","Metadata":{"hc-outbox-id":"hc_emob_ffffffffffffffffffffffff"}}`,
		"oversize":            `{"RecordType":"Delivery","Recipient":"` + strings.Repeat("a", maxPostmarkBodySize) + `"}`,
	}
	for name, body := range cases {
		if rec := postmarkPost(t, h, body, goodAuth); rec.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200 ack", name, rec.Code)
		}
	}
	if len(store.marked) != 0 {
		t.Errorf("deterministic drops must write nothing: %+v", store.marked)
	}
}

// TestPostmarkWebhook_TransientFailures5xx: only genuine transient
// failures 5xx, so Postmark redelivers into the idempotent writes.
func TestPostmarkWebhook_TransientFailures5xx(t *testing.T) {
	t.Run("lookup error", func(t *testing.T) {
		store := &fakeDeliveryStore{getErr: errors.New("db down")}
		h := NewPostmarkHandler(store, "pmuser", "pmpass")
		if rec := postmarkPost(t, h, deliveryJSON("ap@acme.test"), goodAuth); rec.Code != http.StatusInternalServerError {
			t.Errorf("lookup error: got %d, want 500", rec.Code)
		}
	})
	t.Run("mark error", func(t *testing.T) {
		store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}, markErr: errors.New("db down")}
		h := NewPostmarkHandler(store, "pmuser", "pmpass")
		if rec := postmarkPost(t, h, deliveryJSON("ap@acme.test"), goodAuth); rec.Code != http.StatusInternalServerError {
			t.Errorf("mark error: got %d, want 500", rec.Code)
		}
	})
	t.Run("suppressor error", func(t *testing.T) {
		store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}}
		h := NewPostmarkHandler(store, "pmuser", "pmpass")
		h.SetSuppressor(&fakeSuppressor{err: errors.New("db down")})
		if rec := postmarkPost(t, h, bounceJSON("ap@acme.test", "HardBounce", 1, true), goodAuth); rec.Code != http.StatusInternalServerError {
			t.Errorf("suppressor error: got %d, want 500", rec.Code)
		}
	})
}

// TestPostmarkWebhook_NilSuppressorDegrades: without a blind-index key
// there is no suppressor — delivery_state ingestion still works and a
// bounce degrades to WARN-only instead of failing the webhook.
func TestPostmarkWebhook_NilSuppressorDegrades(t *testing.T) {
	store := &fakeDeliveryStore{rows: map[string]OutboxRow{testOutboxID: testRow()}}
	h := NewPostmarkHandler(store, "pmuser", "pmpass")

	if rec := postmarkPost(t, h, bounceJSON("ap@acme.test", "HardBounce", 1, true), goodAuth); rec.Code != http.StatusOK {
		t.Fatalf("bounce without suppressor: got %d, want 200", rec.Code)
	}
	if len(store.marked) != 1 || store.marked[0].state != DeliveryBounced {
		t.Errorf("delivery_state must still record without a suppressor: %+v", store.marked)
	}
}
