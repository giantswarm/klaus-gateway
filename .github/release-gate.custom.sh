#!/usr/bin/env bash
# The stable gate: the auto-release workflow runs it on the promoted commit with
# the version about to be tagged, before the tag and the GitHub release exist.
# A stable release whose UPGRADE.md still has a `## Next` section with content
# fails the tag's own build (`make check-upgrade-notes` in `make test`), after
# the tag is out; refusing it here cuts no tag at all.
set -euo pipefail

make --no-print-directory check-upgrade-notes VERSION="${1:?usage: $0 <tag>}"
