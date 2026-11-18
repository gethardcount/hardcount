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
// the Access the request's route needs. A request no route serves passes on
// to the router's 404 or 405. It runs right after authentication and answers
// before any change, so the idempotency layer does not store its 403.
func RequirePermission(table AccessTable) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return PermissionRequired{table: table, next: next}
	}
}

// PermissionRequired checks the route's Access. RequirePermission makes it;
// its doc has the rule.
type PermissionRequired struct {
	table AccessTable
	next  http.Handler
}

func (c PermissionRequired) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	access, routed := c.table.Lookup(r)
	switch {
	case !routed, access.Any:
	case access.Session != "":
		if !CallerOf(r.Context()).IsSession() {
			respond.Forbidden(w, r, sessionRefusal(access.Session))
			return
		}
	case access.Perm != "":
		if !permitted(w, r, access.Perm) {
			return
		}
	default:
		// The route's entry names nobody: a fault in the table, refused.
		slog.ErrorContext(r.Context(), "route declares no permission", "method", r.Method)
		respond.InternalError(w, r)
		return
	}
	c.next.ServeHTTP(w, r)
}

// sessionRefusal is the 403's text for a caller that is not a dashboard
// session, on a route for task.
func sessionRefusal(task string) string {
	return "this endpoint requires a dashboard session — sign in to the dashboard to " + task
}

// permitted reports whether the caller holds perm, and answers 403 when it
// does not.
func permitted(w http.ResponseWriter, r *http.Request, perm Permission) bool {
	if hasPermission(CallerOf(r.Context()), perm) {
		return true
	}
	respond.Forbidden(w, r, "insufficient permissions: this key type does not have "+string(perm)+" access")
	return false
}
