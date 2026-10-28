package recipe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
)

// TestRecipePreview_OptsOutOfAudit pins that POST /v1/recipes/{key}/preview —
// a read-only dry-run render — calls audit.MarkSkip. The deleted catch-all used
// to record a spurious "Created recipe" row for it; today the declaration is what
// stops the coverage detector reporting a mutating-method 2xx that mutates
// nothing. An empty registry makes the service return ErrNotFound, but MarkSkip
// is called first regardless, which is what we assert.
func TestRecipePreview_OptsOutOfAudit(t *testing.T) {
	h := &Handler{svc: &Service{registry: &Registry{}}}

	req := httptest.NewRequest(http.MethodPost, "/v1/recipes/anything/preview", strings.NewReader(`{}`))
	req = req.WithContext(audit.WithRequestState(req.Context()))
	rec := httptest.NewRecorder()

	h.preview(rec, req)

	if !audit.WasHandled(req.Context()) {
		t.Error("recipe preview must call audit.MarkSkip — it writes nothing, and an undeclared mutating 2xx reads as an uncovered mutation")
	}
}
