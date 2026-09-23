// Package project exposes build-time metadata for the klaus-gateway binary.
package project

import "runtime/debug"

// dev is the default for unset build identifiers. Local `go build`
// invocations without ldflags keep this so `klaus-gateway --version` stays
// printable.
const dev = "dev"

// devel is the module version runtime/debug reports for a build that carries
// no resolvable VCS tag (no .git, or built outside a module checkout).
const devel = "(devel)"

// Build identifiers, overridden at link time via `-X` ldflags. The
// architect-orb `go-build` job links with the `.ldflags` file its `go-test`
// step writes, which stamps `gitSHA` (from `CIRCLE_SHA1`) and `buildTimestamp`
// (UTC build time) but not `version`; `make test`, which the job runs in
// between, appends `version` from gitsemver (Makefile.custom.mk): the release
// on a tag build, a dev version on a branch, the same string as the image tag.
// The devctl Makefile stamps all three locally. A build without a `version`
// ldflag, such as a plain `go build`, falls back to the Go build info (see
// Version). That is never a v2+ release: the module path carries no major
// version suffix, so the toolchain ignores the v2+ tags and reports a v0/v1
// pseudo-version or "(devel)".
var (
	version        = dev
	gitSHA         = dev
	buildTimestamp = "unknown"
)

// Version returns the best human-readable build identifier available, in
// order: an explicitly injected `version` ldflag, the VCS version stamped into
// the Go build info (a v0/v1 pseudo-version for this module, see above), the
// injected commit SHA, and finally the placeholder "dev".
func Version() string {
	if version != dev && version != "" {
		return version
	}
	if v := buildInfoVersion(); v != "" {
		return v
	}
	if gitSHA != dev {
		return gitSHA
	}
	return dev
}

// buildInfoVersion reads the main module version the Go toolchain embedded from
// version control. It returns "" when no usable version is present — either no
// build info, or the "(devel)" placeholder a tag-less build produces — so
// Version can fall through to the next source.
var buildInfoVersion = func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if v := info.Main.Version; v != "" && v != devel {
		return v
	}
	return ""
}

// GitSHA returns the commit SHA the binary was built from.
func GitSHA() string { return gitSHA }

// BuildTimestamp returns the UTC build time in RFC 3339 format, or
// "unknown" when no ldflag was injected.
func BuildTimestamp() string { return buildTimestamp }
