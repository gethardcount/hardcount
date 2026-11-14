package auth

import (
	"net/http"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// Require returns middleware that answers 403 unless the caller's kind of
// credential holds perm.
func Require(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if permitted(w, r, perm) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// permitted reports whether the caller's kind of credential holds perm, and
// answers 403 when it does not.
func permitted(w http.ResponseWriter, r *http.Request, perm Permission) bool {
	if HasPermission(GetKeyType(r.Context()), perm) {
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
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if GetKeyType(r.Context()) != KeyTypeSession {
				respond.Forbidden(w, r, msg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireMethod returns middleware that picks the permission from the HTTP
// method: GET, HEAD and OPTIONS check read, and POST, PUT, PATCH and DELETE
// check write. Put it in front of a chi.Mount whose subrouter has both reads
// and writes, where one Require would let a caller that may read also write.
//
// Usage:
//
//	r.With(auth.RequireMethod(auth.PermCustomerRead, auth.PermCustomerWrite)).
//	    Mount("/customers", customerH.Routes())
func RequireMethod(read, write Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			perm := read
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				perm = write
			}
			if permitted(w, r, perm) {
				next.ServeHTTP(w, r)
			}
		})
	}
}
