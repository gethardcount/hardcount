package auth

import (
	"log/slog"
	"net/http"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// Need is who may call a route: a caller holding Perm, only a dashboard
// session (Session is the task its 403 names), or any caller (Any). The zero
// Need lets nobody through.
type Need struct {
	Perm    Permission
	Session string
	Any     bool
}

// Needs gives a request's route's Need, and false for a request no route
// serves.
type Needs interface {
	Need(*http.Request) (Need, bool)
}

// RequirePermission returns middleware that answers 403 unless the caller has
// what the request's route needs. A request no route serves passes on to the
// router's 404 or 405. It runs right after authentication and answers before
// any change, so the idempotency layer does not store its 403.
func RequirePermission(needs Needs) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return PermissionRequired{needs: needs, next: next}
	}
}

// PermissionRequired checks the route's Need. RequirePermission makes it; its
// doc has the rule.
type PermissionRequired struct {
	needs Needs
	next  http.Handler
}

func (c PermissionRequired) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	need, routed := c.needs.Need(r)
	switch {
	case !routed, need.Any:
	case need.Session != "":
		if !CallerOf(r.Context()).IsSession() {
			respond.Forbidden(w, r, sessionRefusal(need.Session))
			return
		}
	case need.Perm != "":
		if !permitted(w, r, need.Perm) {
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

// PermissionCheck answers 403 unless the caller holds the permission its
// request's method needs. Require and RequireMethod make it; RequireMethod's
// doc has the rule.
type PermissionCheck struct {
	read, write Permission
	next        http.Handler
}

func (c PermissionCheck) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	perm := c.read
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		perm = c.write
	}
	if permitted(w, r, perm) {
		c.next.ServeHTTP(w, r)
	}
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

// Require returns middleware that answers 403 unless the caller holds perm
// (hasPermission), whatever the request's method. A 403 is answered only
// before any change, as here, in front of the handler: the idempotency layer
// does not store it.
func Require(perm Permission) func(http.Handler) http.Handler {
	return RequireMethod(perm, perm)
}

// RequireMethod returns middleware that answers 403 unless the caller holds
// the permission the request's method needs: write for POST, PUT, PATCH and
// DELETE, read for every other method. Put it in front of a chi.Mount whose
// subrouter has both reads and writes, where one Require would let a caller
// that may read also write. Like Require, it answers before any change.
func RequireMethod(read, write Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return PermissionCheck{read: read, write: write, next: next}
	}
}

// SessionRequired lets only dashboard sessions through. RequireSession makes
// it; its doc has the rule.
type SessionRequired struct {
	refusal string
	next    http.Handler
}

func (c SessionRequired) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !CallerOf(r.Context()).IsSession() {
		respond.Forbidden(w, r, c.refusal)
		return
	}
	c.next.ServeHTTP(w, r)
}

// RequireSession returns middleware that lets only dashboard sessions through
// (Caller.IsSession). Use it for a human act (team membership), where an API
// key has no user to attribute the action to, and for settings both modes
// share, which a key, made in one mode, must not change. Any other caller
// gets a 403 that ends "sign in to the dashboard to <task>", before any
// change, as Require does.
func RequireSession(task string) func(http.Handler) http.Handler {
	msg := sessionRefusal(task)
	return func(next http.Handler) http.Handler {
		return SessionRequired{refusal: msg, next: next}
	}
}
