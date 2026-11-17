package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/auth"
)

// A line written with a *Context method carries the request's id and tenant
// from ctx, once each; the request-path lines rely on it instead of passing
// either by hand.
func TestContextHandler_AddsRequestIDAndTenantOnce(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := auth.WithTenantID(context.WithValue(context.Background(), chimw.RequestIDKey, "req_1"), "hc_ten_1")
	log.WarnContext(ctx, "a line")

	if n := strings.Count(buf.String(), `"request_id"`) + strings.Count(buf.String(), `"tenant_id"`); n != 2 {
		t.Fatalf("line %s: want request_id and tenant_id once each", buf.String())
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil || line["request_id"] != "req_1" || line["tenant_id"] != "hc_ten_1" {
		t.Errorf("line %s (%v): want request_id req_1 and tenant_id hc_ten_1", buf.String(), err)
	}
}
