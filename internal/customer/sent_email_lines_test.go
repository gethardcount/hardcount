package customer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// The activity line standard (ADR-128) on the customer's Sent emails list:
// every row carries a `line` with the same email words the invoice's
// Activity card uses, so the card can stop keeping its own label and badge
// maps (B4: its "not sent — settled first" is false for a voided or
// written-off invoice).

type sentEmailsFake struct{ rows []SentEmailOutboxRow }

func (f sentEmailsFake) ListByCustomer(context.Context, string, string) ([]SentEmailOutboxRow, error) {
	return f.rows, nil
}

// fetchSentEmails serves GET /v1/customers/{id}/sent-emails over rows and
// returns each row as JSON.
func fetchSentEmails(t *testing.T, rows []SentEmailOutboxRow) []map[string]any {
	t.Helper()
	store := newMemoryStore()
	store.customers["vlx_cus_1"] = domain.Customer{ID: "vlx_cus_1", TenantID: "t1"}
	h := NewHandler(NewService(store))
	h.SetSentEmailsLister(sentEmailsFake{rows: rows})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "vlx_cus_1")
	ctx := auth.WithTenantID(req.Context(), "t1")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.listSentEmails(rr, req.WithContext(ctx))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		SentEmails []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.SentEmails
}

// metaOf renders a line's meta as the card would print it: each segment's
// parts (a link prints its label), joined by " · ", then the actor.
func metaOf(l map[string]any) string {
	var segs []string
	detail, _ := l["detail"].([]any)
	for _, s := range detail {
		parts, _ := s.([]any)
		var b strings.Builder
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			switch {
			case pm["link"] != nil:
				link, _ := pm["link"].(map[string]any)
				fmt.Fprint(&b, link["label"])
			default:
				fmt.Fprint(&b, pm["text"])
			}
		}
		segs = append(segs, b.String())
	}
	if a, ok := l["actor"].(map[string]any); ok && a["kind"] == "automatic" {
		segs = append(segs, "Automatic")
	}
	return strings.Join(segs, " · ")
}

func TestSentEmails_EveryRowHasALine(t *testing.T) {
	created := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	dispatched := created.Add(2 * time.Minute)
	rows := []SentEmailOutboxRow{
		{ID: "emob_1", EmailType: "invoice", Recipient: "billing@acme.test", Status: "dispatched", DeliveryState: "delivered",
			CreatedAt: created, DispatchedAt: &dispatched, InvoiceNumber: "VLX-000123"},
		{ID: "emob_2", EmailType: "payment_failed", Recipient: "billing@acme.test", Status: "dispatched", DeliveryState: "bounced",
			CreatedAt: created, DispatchedAt: &dispatched, InvoiceNumber: "VLX-000123"},
		{ID: "emob_3", EmailType: "dunning_warning", Recipient: "billing@acme.test", Status: "skipped", DeliveryState: "unknown",
			CreatedAt: created, InvoiceNumber: "VLX-000123",
			LastError: "email outbox: obsolete — invoice settled before delivery: invoice VLX-000123 reached voided before delivery"},
		{ID: "emob_4", EmailType: "dunning_escalation", Recipient: "billing@acme.test", Status: "pending", DeliveryState: "unknown",
			CreatedAt: created, InvoiceNumber: "VLX-000123"},
		{ID: "emob_5", EmailType: "payment_receipt", Recipient: "billing@acme.test", Status: "dispatched", DeliveryState: "complained",
			CreatedAt: created, DispatchedAt: &dispatched, InvoiceNumber: "VLX-000123"},
	}
	got := fetchSentEmails(t, rows)
	if len(got) != len(rows) {
		t.Fatalf("%d rows, want %d", len(got), len(rows))
	}

	want := []struct{ title, tone, problem, meta string }{
		{"Invoice email delivered", "neutral", "", "VLX-000123 · To billing@acme.test"},
		{"Failed payment notice bounced", "failure", "The customer's mail server rejected it.", "VLX-000123 · To billing@acme.test · Automatic"},
		// A row skipped before the reason was a field: the line says what
		// it can know, never "settled first".
		{"Payment reminder email not sent", "neutral", "", "The invoice was already closed · VLX-000123 · To billing@acme.test · Automatic"},
		{"Final notice queued", "neutral", "", "VLX-000123 · To billing@acme.test · Automatic"},
		{"Receipt email marked as spam", "failure", "", "VLX-000123 · To billing@acme.test · Automatic"},
	}
	for i, row := range got {
		l, ok := row["line"].(map[string]any)
		if !ok {
			t.Fatalf("row %v has no line", row["id"])
		}
		w := want[i]
		problem, _ := l["problem"].(string)
		if l["title"] != w.title || l["tone"] != w.tone || problem != w.problem {
			t.Errorf("row %v: title %q tone %q problem %q; want %q %q %q", row["id"], l["title"], l["tone"], problem, w.title, w.tone, w.problem)
		}
		if m := metaOf(l); m != w.meta {
			t.Errorf("row %v meta: %q, want %q", row["id"], m, w.meta)
		}
		if l["id"] != "email:"+fmt.Sprint(row["id"]) {
			t.Errorf("row %v: line id %v", row["id"], l["id"])
		}
		// A real-clock email is dated when it went out (else when it was
		// queued) and has no Recorded time.
		at := created
		if rows[i].DispatchedAt != nil {
			at = dispatched
		}
		if occ, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(l["occurred_at"])); !occ.Equal(at) {
			t.Errorf("row %v occurred_at %v, want %v", row["id"], l["occurred_at"], at)
		}
		if l["recorded_at"] != nil {
			t.Errorf("row %v on the real clock has recorded_at %v", row["id"], l["recorded_at"])
		}
	}
}
