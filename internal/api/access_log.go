package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/go-chi/chi/v5/middleware"
)

// requestLogger writes the access log. Each line says who made the request,
// which tenant it touched and from which address, not only the method, path,
// status and duration. So after an incident the log can answer "what did this
// caller do" and "which tenant was affected". The request_id on each line
// joins it to the audit row that the same request wrote.
//
// The caller is the API key or dashboard session the request was made with,
// under the audit log's names for it. session.RequireCaller names it;
// sign-out and the mode switch read the session cookie themselves and name
// it too (session.NameActor). Any other request is "anonymous": a public
// page, a webhook, a sign-in, or one refused before a caller was accepted. A
// public token page and a webhook learn their account inside the handler, so
// their lines have no tenant_id.
//
// Only the actor id is logged, not the name. A user actor's name is an
// email, and logs go to sinks that cannot erase data. For the same reason,
// the append-only audit_log stores no addresses either.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		// Authentication runs inside this middleware and hands its handler a
		// new request, so r's context never learns the caller. Authentication
		// writes the caller on this record.
		ctx := request.WithRecord(r.Context())
		r = r.WithContext(ctx)
		next.ServeHTTP(ww, r)

		attrs := []any{
			"method", r.Method,
			"path", loggablePath(r),
			"status", ww.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			// Joins this line to the audit row the same request wrote.
			"request_id", middleware.GetReqID(ctx),
			"ip", audit.ExtractClientIP(r),
		}
		if actor := request.ActorOf(ctx); actor != (request.Actor{}) {
			attrs = append(attrs, "actor_type", actor.Type, "actor_id", actor.ID, "tenant_id", actor.TenantID)
		} else {
			// No actor_id or tenant_id: a blank field reads as "we lost it",
			// not "there was none".
			attrs = append(attrs, "actor_type", "anonymous")
		}
		slog.Info("request", attrs...)
	})
}
