package middleware

import (
	"context"
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/xid"
)

// requestIDPrefix marks an id the server minted. audit_log is append-only,
// and rows older than RequestID may hold an id a client chose.
const requestIDPrefix = "req_"

// RequestID gives every request a new id, "req_" and an xid, and never takes
// one from the client: the id is stamped on the request's audit rows and log
// lines, so a client that chose it could forge or collide that evidence. A
// client matches its request to them by the Hardcount-Request-Id header, and
// cross-service tracing uses W3C Trace Context (Tracing).
//
// The id is stored under chi's RequestIDKey, so chimw.GetReqID returns it,
// and it is the Hardcount-Request-Id response header, set before the handler
// runs, so every answer carries it, even one with no body.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDPrefix + xid.New().String()
		ctx := context.WithValue(r.Context(), chimw.RequestIDKey, id)
		w.Header().Set("Hardcount-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
