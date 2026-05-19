# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Changed



- Disable cosign keyless chart signing on the `push-to-app-catalog*` jobs (`sign: false`). The architect orb's `push-to-app-catalog` defaults `sign` to `true` since v8.2.0 and shells out to `cosign`, but this repo uses `executor: app-build-suite` (so the `app_build_suite` Python CLI is available to package the chart with metadata) and the `app-build-suite` image doesn't ship `cosign`. Without this opt-out, every chart push fails on the `Mint Sigstore OIDC token` step with `cosign: command not found`. To be removed once architect-orb makes `cosign-prepare` resilient to a missing binary (or ships cosign in the `app-build-suite` executor).
- Bump `giantswarm/architect` orb to `8.2.1` to pick up [architect-orb#767](https://github.com/giantswarm/architect-orb/pull/767): `image-login-to-registries` is now POSIX-portable, unblocking `architect/sync-china-registry` (the gsoci -> Aliyun mirror via the in-China `giantswarm/galaxy-runner`). The v8.1.0 refactor accidentally introduced bash-only `${!var}` indirect expansion in the shared login command, which BusyBox `/bin/sh` (used by the regctl executor) rejected with `bad substitution` -- so no Aliyun mirror has been happening since the migration to `split-china-push: true`. v8.2.x also enables cosign keyless signing, SLSA provenance, and SBOM attestations by default for public images and charts.
- Replace the `push-to-gsoci-release` + `push-to-all-registries-release` workaround pair (gsoci-only push gating the chart, plus a parallel best-effort all-registries push to dodge Aliyun timeouts) with a single `push-to-registries-release` job using `split-china-push: true` and a companion `sync-china-registry` job. The cross-Pacific `docker buildx` push to the Aliyun mirror is gone; the in-China `giantswarm/galaxy-runner` runs `regctl image copy` from gsoci to Aliyun via the Singapore geo-replica. The chart catalog publish still does not gate on Aliyun.
- Bump `giantswarm/architect` orb to `8.1.0` and migrate image pushes from the deprecated `push-to-registries-multiarch` job to `push-to-registries` with `multiarch: true`. Picks up the v8.1.0 QEMU/binfmt auto-registration, hardened buildx bootstrap, and standard OCI image labels.

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
