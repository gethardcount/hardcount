package creditnote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
)

// TestCreateCommitRelief_TopLevelSplitRefused: the relief's cash split lives
// inside commit_relief. Sent at the top level (where an ordinary credit note
// takes it) the split was ignored and the relief defaulted to a full card
// refund: after a lost dispute, the bank already returned the money, so the
// customer would be paid back twice. The handler refuses each top-level split
// field with a 422 naming it, before the service runs.
func TestCreateCommitRelief_TopLevelSplitRefused(t *testing.T) {
	srv := NewHandler(nil).Routes()
	for _, c := range []struct{ field, value string }{
		{"out_of_band_amount_cents", `5000`},
		{"refund_amount_cents", `5000`},
		{"refund_type", `"refund"`},
	} {
		t.Run(c.field, func(t *testing.T) {
			body := `{"invoice_id":"inv_1","reason":"Lost card dispute","commit_relief":{"retire_all":true},"` + c.field + `":` + c.value + `}`
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1"))
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
			}
			var env struct {
				Error struct {
					Param   string `json:"param"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(rr.Body.Bytes(), &env)
			if env.Error.Param != c.field {
				t.Errorf("param: got %q, want %q", env.Error.Param, c.field)
			}
		})
	}
}
