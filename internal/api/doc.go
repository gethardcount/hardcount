// Package api provides the HTTP server and route wiring for Hardcount.
// It creates domain stores, services, and handlers, then mounts them
// on a chi router with auth, rate limiting, and metrics middleware.
package api
