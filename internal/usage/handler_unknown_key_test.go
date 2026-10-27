package usage

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// An unknown key on one event refuses the batch with the batch 422 the spec
// documents: code batch_rejected, and errors[] names that event's index
// beside every other failing event. Before, it was the plain error body
// with no errors[] and no index, so a pipeline could not tell which event
// to fix.
func TestUsageBatch_UnknownKeyNamesItsIndex(t *testing.T) {
	store := newMemStore()
	h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
	withKey := strings.Replace(quantityBody(`1`, "evt_u1"), `{`, `{"b9_typo":1,`, 1)
	body := "[" + strings.Join([]string{
		quantityBody(`1`, "evt_u0"),
		withKey,
		quantityBody(`true`, "evt_u2"),
	}, ",") + "]"
	rr := usageReq(t, h.batchIngest, body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Errors   []string `json:"errors"`
		Ingested *int     `json:"ingested"`
		Total    int      `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "batch_rejected" {
		t.Errorf("code = %q, want batch_rejected; body %s", resp.Error.Code, rr.Body.String())
	}
	if len(resp.Errors) != 2 || resp.Errors[0] != `event[1]: unknown field "b9_typo"` || !strings.HasPrefix(resp.Errors[1], "event[2]: quantity") {
		t.Errorf("errors = %q, want event[1] naming b9_typo and event[2] naming quantity", resp.Errors)
	}
	if resp.Ingested == nil || *resp.Ingested != 0 || resp.Total != 3 {
		t.Errorf("ingested %v total %d, want 0 and 3", resp.Ingested, resp.Total)
	}
	if len(store.events) != 0 {
		t.Errorf("%d events stored, want 0 (all-or-nothing)", len(store.events))
	}
}
