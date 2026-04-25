# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Changed

- Split tag-build registry push into two parallel jobs: a gsoci-only push that
  gates the chart catalog release, and a separate "all registries" push that
  also covers the slow China mirror. The chart push no longer waits for the
  China mirror to come back online, mirroring the pattern in `mcp-prometheus`.

### Fixed

- Set chart `appVersion` to the chart `version` so the default `image.tag`
  resolves to the per-tag image pushed by the auto-release pipeline instead of
  the now-missing floating `:dev` tag.

[Unreleased]: https://github.com/giantswarm/REPOSITORY_NAME/tree/main
