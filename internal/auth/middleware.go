package auth

import (
	"net/http"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// PermissionCheck answers 403 unless the caller may make the request, by one
// of three rules (Kind). Require, RequireMethod and RequireSession make it.
type PermissionCheck struct {
	// Kind is the rule.
	Kind CheckKind
	// Read is the permission a read needs (GET, HEAD, OPTIONS), and Write
	// the one a write needs (POST, PUT, PATCH, DELETE). Require sets both to
	// its one permission. RequireSession checks neither.
	Read, Write Permission

	refusal string // RequireSession's 403 message
	next    http.Handler
}

// CheckKind is the rule a PermissionCheck applies.
type CheckKind int

const (
	// CheckPermission needs one permission for every method (Require).
	CheckPermission CheckKind = iota
	// CheckByMethod needs Read for a read and Write for a write
	// (RequireMethod).
	CheckByMethod
	// CheckSession lets only dashboard sessions through (RequireSession).
	CheckSession
)

func (c PermissionCheck) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch c.Kind {
	case CheckSession:
		if !CallerOf(r.Context()).IsSession() {
			respond.Forbidden(w, r, c.refusal)
			return
		}
	default:
		perm := c.Read
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			perm = c.Write
		}
		if !permitted(w, r, perm) {
			return
		}
	}
	c.next.ServeHTTP(w, r)
}

// Require returns middleware that answers 403 unless the caller holds perm
// (HasPermission).
func Require(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return PermissionCheck{Kind: CheckPermission, Read: perm, Write: perm, next: next}
	}
}

// permitted reports whether the caller holds perm, and answers 403 when it
// does not.
func permitted(w http.ResponseWriter, r *http.Request, perm Permission) bool {
	if HasPermission(CallerOf(r.Context()), perm) {
		return true
	}
	respond.Forbidden(w, r, "insufficient permissions: this key type does not have "+string(perm)+" access")
	return false
}

// RequireSession lets only dashboard sessions through. Use it for a human act
// (team membership), where an API key has no user to attribute the action
// to, and for settings both modes share, which a key, made in one mode, must
// not change. Any other caller gets a 403 that ends "sign in to the dashboard
// to <task>".
func RequireSession(task string) func(http.Handler) http.Handler {
	msg := "this endpoint requires a dashboard session — sign in to the dashboard to " + task
	return func(next http.Handler) http.Handler {
		return PermissionCheck{Kind: CheckSession, refusal: msg, next: next}
	}
}

// RequireMethod returns middleware that picks the permission from the HTTP
// method: GET, HEAD and OPTIONS check read, and POST, PUT, PATCH and DELETE
// check write. Put it in front of a chi.Mount whose subrouter has both reads
// and writes, where one Require would let a caller that may read also write.
func RequireMethod(read, write Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return PermissionCheck{Kind: CheckByMethod, Read: read, Write: write, next: next}
	}
}
