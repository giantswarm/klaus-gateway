# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Fixed

- Set chart `appVersion` to the chart `version` so the default `image.tag`
  resolves to the per-tag image pushed by the auto-release pipeline instead of
  the now-missing floating `:dev` tag.

[Unreleased]: https://github.com/giantswarm/REPOSITORY_NAME/tree/main
