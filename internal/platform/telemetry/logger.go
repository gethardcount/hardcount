package telemetry

import (
	"context"
	"log/slog"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/auth"
)

// ContextHandler wraps a slog.Handler and adds request_id and tenant_id from
// the call's ctx to every record. Callers must use the slog methods that end
// in Context (InfoContext, ErrorContext and so on) for these attributes to
// appear. A bare slog.Error("msg") has no ctx to read from.
//
// This is the one place logs are enriched, so handlers and services do not
// have to stamp request_id at every call site. A design partner who hits a
// bug can report the request_id from the Hardcount-Request-Id response
// header. One grep over the structured JSON logs then shows the full trace.
type ContextHandler struct {
	slog.Handler
}

// NewContextHandler wraps h, so records from the slog methods that end in
// Context carry request_id and tenant_id attrs when ctx has them.
func NewContextHandler(h slog.Handler) *ContextHandler {
	return &ContextHandler{Handler: h}
}

// Handle implements slog.Handler. It reads request_id (chi middleware) and
// tenant_id (auth middleware) from ctx and adds them as attrs on the record
// before delegating to the wrapped handler.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if reqID := chimw.GetReqID(ctx); reqID != "" {
			r.AddAttrs(slog.String("request_id", reqID))
		}
		if tenantID := auth.TenantID(ctx); tenantID != "" {
			r.AddAttrs(slog.String("tenant_id", tenantID))
		}
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs returns a new ContextHandler whose wrapped handler has the given
// attrs attached. Required for slog.Logger.With to keep working.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup returns a new ContextHandler whose wrapped handler has the given
// group name attached. Required for slog.Logger.WithGroup to keep working.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithGroup(name)}
}
