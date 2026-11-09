// Package logging holds ErrorLevel, which picks the log level for an error
// from work done for a request. Handlers, middleware and services all use
// it, so it lives outside the HTTP packages.
//
// It is not in internal/platform/telemetry, which holds the rest of the
// logging code, because telemetry imports internal/auth, and auth's
// middleware calls ErrorLevel. Putting it there would be an import cycle.
package logging

import (
	"context"
	"errors"
	"log/slog"
)

// ErrorLevel returns the level for logging err, an error from work done for
// a request. ctx is the request's context, or one derived from it.
//
// It is WARN when the request's client went away, for example by closing a
// browser tab: ctx ended with context.Canceled and err wraps
// context.Canceled. This relies on net/http being the only code that cancels
// a request's context, which it does when the client disconnects. Every
// other failure is ERROR, the level operators alert on. That includes the
// 30s route timeout, which ends ctx with context.DeadlineExceeded, because a
// slow request is a server problem.
//
// Use it only where the harm ends with the request, such as a failed read
// for its answer. A line about work lost or left half done, such as an audit
// row or a session revocation, stays at ERROR even when the client went away.
func ErrorLevel(ctx context.Context, err error) slog.Level {
	if errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
		return slog.LevelWarn
	}
	return slog.LevelError
}
