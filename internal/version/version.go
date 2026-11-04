// Package version carries the build-time version stamp. The build sets
// Version with
//
//	-ldflags "-X github.com/gethardcount/hardcount/internal/version.Version=v0.x.y"
//
// The Dockerfile takes it as ARG VERSION, and the CI release job passes the
// git tag. The "dev" default means an unstamped local build says so.
// `hardcount version` and the Hardcount-Version response header both report
// Version, so they answer "what version are you running?".
package version

// Version is the build's version stamp, or "dev" when the build was not
// stamped.
var Version = "dev"
