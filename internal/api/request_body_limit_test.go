package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/integrations/litellm"
)

// TestLimitRequestBody_LiteLLMRouteHasItsOwnCap: the LiteLLM adapter reads
// bodies up to litellm.MaxBodyBytes — a full LiteLLM batch is about 4 MB —
// and every other route stays capped at 1 MB. Before, the one 1 MB cap for
// every route refused each full batch, and LiteLLM never resends a 4xx.
// Mutation-verify: drop the LiteLLM branch in limitRequestBody → the 4 MB
// spend body is cut at 1 MB.
func TestLimitRequestBody_LiteLLMRouteHasItsOwnCap(t *testing.T) {
	// The stub reads the whole body and answers how much it read, or the cap
	// it hit.
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = fmt.Fprint(w, tooBig.Limit)
			return
		}
		_, _ = fmt.Fprint(w, n)
	}))
	post := func(path string, size int) (int, string) {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(make([]byte, size)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	const batch = 4 << 20
	for _, c := range []struct {
		name, path string
		size       int
		wantCode   int
		wantBody   string
	}{
		{"LiteLLM spend reads a full batch", "/v1/integrations/litellm/spend", batch, http.StatusOK, fmt.Sprint(batch)},
		{"LiteLLM spend reads up to its cap", "/v1/integrations/litellm/spend", litellm.MaxBodyBytes, http.StatusOK, fmt.Sprint(litellm.MaxBodyBytes)},
		{"LiteLLM spend stops past its cap", "/v1/integrations/litellm/spend", litellm.MaxBodyBytes + 1, http.StatusRequestEntityTooLarge, fmt.Sprint(litellm.MaxBodyBytes)},
		{"usage batch keeps 1 MB", "/v1/usage-events/batch", batch, http.StatusRequestEntityTooLarge, fmt.Sprint(maxRequestBodyBytes)},
		{"other routes keep 1 MB", "/v1/customers", maxRequestBodyBytes + 1, http.StatusRequestEntityTooLarge, fmt.Sprint(maxRequestBodyBytes)},
		{"1 MB still reads", "/v1/customers", maxRequestBodyBytes, http.StatusOK, fmt.Sprint(maxRequestBodyBytes)},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := post(c.path, c.size)
			if code != c.wantCode || body != c.wantBody {
				t.Fatalf("POST %s with %d bytes = %d %q, want %d %q", c.path, c.size, code, body, c.wantCode, c.wantBody)
			}
		})
	}
}
