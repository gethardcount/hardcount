// Package version carries the build-time version stamp. Version is
// injected at build via
//
//	-ldflags "-X github.com/gethardcount/hardcount/internal/version.Version=v0.x.y"
//
// (Dockerfile ARG VERSION → the CI release job passes the git tag). The
// "dev" default means an unstamped local build says so honestly —
// pre-2026-07-06 `hardcount version` printed a hardcoded April date on every
// build and the Hardcount-Version response header pinned the same stale
// constant, so "what version are you running?" was unanswerable.
package version

var Version = "dev"
