#!/usr/bin/env bash
# Computes the release metadata used by .github/workflows/release.yml and writes
# it to $GITHUB_OUTPUT as: version, prerelease, publish, tags.
#
# Environment:
#   GITHUB_EVENT_NAME   "push" (a v* tag) or "workflow_dispatch" (dry run, never publishes)
#   GITHUB_REF_NAME     the tag name for a push, e.g. v1.2.3
#   INPUT_VERSION       the version for a workflow_dispatch dry run
#   GITHUB_SHA          the commit being released
#   CHANGELOG           changelog path (default CHANGELOG.md)
#   SKIP_GIT_CHECKS=1   skip the "tag is on main" check (used by the tests)
set -euo pipefail

out="${GITHUB_OUTPUT:-/dev/stdout}"
changelog="${CHANGELOG:-CHANGELOG.md}"

if [ "${GITHUB_EVENT_NAME:-}" = "workflow_dispatch" ]; then
  version="${INPUT_VERSION:-}"
  version="${version#v}"
  publish=false
else
  version="${GITHUB_REF_NAME:-}"
  version="${version#v}"
  publish=true
fi

# Semantic version: X.Y.Z, optionally followed by a pre-release such as -rc.1.
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
if ! [[ "$version" =~ $semver ]]; then
  echo "::error::'${version}' is not a valid semantic version (expected X.Y.Z or X.Y.Z-rc.1)"
  exit 1
fi

if [[ "$version" == *-* ]]; then prerelease=true; else prerelease=false; fi

core="${version%%-*}"
major="${core%%.*}"
rest="${core#*.}"
minor="${rest%%.*}"

# A stable release moves the floating tags; a pre-release only gets its exact tag.
if [ "$prerelease" = true ]; then
  tags="${version}"
else
  tags="${version} ${major}.${minor} ${major} latest"
fi

if [ "$publish" = true ]; then
  if [ "$prerelease" = false ]; then
    # exact-prefix match (no regex), so the dots in the version are literal
    if ! awk -v h="## [${version}]" 'index($0,h)==1{f=1} END{exit !f}' "$changelog"; then
      echo "::error::${changelog} has no '## [${version}]' heading. Move the [Unreleased] entries under it before tagging."
      exit 1
    fi
  fi
  if [ "${SKIP_GIT_CHECKS:-0}" != "1" ]; then
    if ! git merge-base --is-ancestor "${GITHUB_SHA}" origin/main; then
      echo "::error::tag ${GITHUB_REF_NAME} (${GITHUB_SHA}) is not on main. Release from a commit that is on main."
      exit 1
    fi
  fi
fi

{
  echo "version=${version}"
  echo "prerelease=${prerelease}"
  echo "publish=${publish}"
  echo "tags=${tags}"
} >> "$out"
