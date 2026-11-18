package auth

import (
	"net/http"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

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
// that may read also write.
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
// gets a 403 that ends "sign in to the dashboard to <task>".
func RequireSession(task string) func(http.Handler) http.Handler {
	msg := "this endpoint requires a dashboard session — sign in to the dashboard to " + task
	return func(next http.Handler) http.Handler {
		return SessionRequired{refusal: msg, next: next}
	}
}
