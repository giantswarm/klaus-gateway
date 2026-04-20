// Package version exposes build-time metadata.
//
// It is a thin alias over pkg/project because the architect Makefile injects
// ldflags into pkg/project. The architecture doc refers to internal/version,
// so this package is the import the rest of the code uses; pkg/project stays
// as the ldflags target.
package version

import "github.com/giantswarm/klaus-gateway/pkg/project"

func Version() string        { return project.Version() }
func GitSHA() string         { return project.GitSHA() }
func BuildTimestamp() string { return project.BuildTimestamp() }
