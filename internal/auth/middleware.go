package auth

import (
	"log/slog"
	"net/http"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// Access is who may call a route: a caller holding Perm, only a dashboard
// session (Session is the task its 403 names), or any caller (Any). The zero
// Access lets nobody through.
type Access struct {
	Perm    Permission
	Session string
	Any     bool
}

// AccessTable gives the Access of a request's route, and false for a request
// no route serves.
type AccessTable interface {
	Lookup(*http.Request) (Access, bool)
}

// RequirePermission returns middleware that answers 403 unless the caller has
// the Access the request's route needs. unrouted answers a request no route
// serves (its 404 or 405), so the next handler runs only for a route the
// table lists and the caller may call.
func RequirePermission(table AccessTable, unrouted http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return PermissionRequired{table: table, unrouted: unrouted, next: next}
	}
}

// PermissionRequired checks the route's Access. RequirePermission makes it;
// its doc has the rule.
type PermissionRequired struct {
	table    AccessTable
	unrouted http.Handler
	next     http.Handler
}

func (c PermissionRequired) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	access, routed := c.table.Lookup(r)
	switch {
	case !routed:
		c.unrouted.ServeHTTP(w, r)
		return
	case access.Any: // any caller passes
	case access.Session != "":
		if !CallerOf(r.Context()).IsSession() {
			respond.Forbidden(w, r, "this endpoint requires a dashboard session — sign in to the dashboard to "+access.Session)
			return
		}
	case access.Perm != "":
		if !hasPermission(CallerOf(r.Context()), access.Perm) {
			respond.Forbidden(w, r, "insufficient permissions: this key type does not have "+string(access.Perm)+" access")
			return
		}
	default:
		// The route's entry names no caller: a fault in the table, refused.
		slog.ErrorContext(r.Context(), "route entry names no caller", "method", r.Method)
		respond.InternalError(w, r)
		return
	}
	c.next.ServeHTTP(w, r)
}
