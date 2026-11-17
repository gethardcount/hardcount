package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/go-chi/chi/v5/middleware"
)

// accessLog writes one line per request, so after an incident the log can
// say what a caller did and which tenant it touched: the method, the path
// (loggablePath), the status, the duration, the request_id that joins the
// line to the request's audit rows, the client address and the caller. The
// caller is the key or session the request was made with, in the audit log's
// words; any other request is "anonymous". Only the caller's id is logged: a
// user's name is an email address, and logs go to sinks that cannot erase it.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)

		// Authentication runs inside this middleware and hands its handler a
		// new request, so it names the caller on the record, not on r.
		ctx := r.Context()
		rec := request.Of(ctx)
		attrs := []any{
			"method", r.Method,
			"path", loggablePath(r),
			"status", rec.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(ctx),
			"ip", rec.ClientIP(),
		}
		if actor := rec.Actor(); actor != (request.Actor{}) {
			attrs = append(attrs, "actor_type", actor.Type, "actor_id", actor.ID, "tenant_id", actor.TenantID)
		} else {
			// No actor_id or tenant_id: a blank field reads as "we lost it",
			// not "there was none".
			attrs = append(attrs, "actor_type", "anonymous")
		}
		slog.Info("request", attrs...)
	})
}
