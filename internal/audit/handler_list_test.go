package audit

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A bad limit or offset is a 422 naming it, never clamped or ignored, and it
// is refused BEFORE any store access — the nil logger proves it.
func TestList_BadPageIs422(t *testing.T) {
	h := NewHandler(nil)
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "offset=-1"} {
		req := httptest.NewRequest(http.MethodGet, "/?"+q, nil)
		rec := httptest.NewRecorder()
		h.list(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("?%s: status %d, want 422. body=%s", q, rec.Code, rec.Body.String())
		}
	}
}

// An unknown ?order is rejected rather than silently ignored: a client that
// asked for one ordering and got another, then paged through it, reads rows in
// an order it is not expecting.
func TestList_InvalidOrderIs400(t *testing.T) {
	h := NewHandler(nil)
	req := httptest.NewRequest(http.MethodGet, "/?order=bogus", nil)
	rec := httptest.NewRecorder()
	h.list(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400. body=%s", rec.Code, rec.Body.String())
	}
}
