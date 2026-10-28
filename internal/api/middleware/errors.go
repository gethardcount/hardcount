package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// Recoverer turns a handler panic into the JSON 500 every other internal
// error answers (respond.InternalError), and logs the panic with its stack.
// It replaces chi's Recoverer, which answered an empty 500.
//
// http.ErrAbortHandler is re-panicked, as net/http expects: it aborts the
// response on purpose. A panic after the handler has started its answer
// cannot change it; the log still records the panic.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		defer func() {
			rvr := recover()
			if rvr == nil {
				return
			}
			if rvr == http.ErrAbortHandler { //nolint:errorlint // net/http compares the sentinel itself
				panic(rvr)
			}
			slog.ErrorContext(r.Context(), "handler panic", "panic", rvr, "method", r.Method, "path", r.URL.Path, "stack", string(debug.Stack()))
			if ww.Status() == 0 && r.Header.Get("Connection") != "Upgrade" {
				respond.InternalError(w, r)
			}
		}()
		next.ServeHTTP(ww, r)
	})
}

// Timeout ends the request's context after d. A handler that has written
// nothing when the time runs out gets the JSON 504 (respond.Timeout). One
// that already started its answer keeps it. It replaces chi's Timeout, which
// answered an empty 504.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			defer func() {
				cancel()
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && ww.Status() == 0 {
					respond.Timeout(w, r)
				}
			}()
			next.ServeHTTP(ww, r.WithContext(ctx))
		})
	}
}
