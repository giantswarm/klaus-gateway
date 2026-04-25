# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Fixed

- Add `.abs/main.yaml` with `replace-chart-version-with-git` /
  `replace-app-version-with-git` enabled. Without this config app-build-suite
  packaged the chart with the literal `0.1.0` placeholder from `Chart.yaml`,
  which left the published chart's `appVersion` (and thus the default
  `image.tag`) pointing at the non-existent `:0.1.0` image. The same flag is
  used by `klaus` and `mcp-prometheus`.

### Changed

- Switch the chart catalog jobs to the `app-build-suite` executor (mirrors the
  klaus and mcp-prometheus pattern). `app-build-suite` rewrites `Chart.yaml`'s
  `version` and `appVersion` from the git tag at build time, which finally
  lets tag releases publish a chart -- previously every tag build failed
  architect's strict `helm-chart-template` validator because
  `pkg/project/project.go` keeps the literal value `dev`.
- Hardcode `version`/`appVersion` placeholders in `helm/klaus-gateway/Chart.yaml`
  back to `0.1.0`. The CI's `app-build-suite` step overwrites them; templating
  via `[[ .Version ]]` (introduced in #19) is incompatible with that flow.
- Split the tag-build registry push into two parallel jobs: a gsoci-only push
  that gates the chart catalog release, and a separate "all registries" push
  that also covers the slow China mirror. The chart push no longer waits for
  the China mirror, so a slow mirror only delays itself.

[Unreleased]: https://github.com/giantswarm/REPOSITORY_NAME/tree/main
