// Package project exposes build-time metadata for the klaus-gateway binary.
package project

var (
	version        = "dev"
	gitSHA         = "unknown"
	buildTimestamp = "unknown"
)

// Version returns the semantic version, injected at build time via -ldflags.
func Version() string { return version }

// GitSHA returns the git commit SHA the binary was built from.
func GitSHA() string { return gitSHA }

// BuildTimestamp returns the RFC3339 build timestamp.
func BuildTimestamp() string { return buildTimestamp }
