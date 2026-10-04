#!/usr/bin/env bash
# Tests for release-meta.sh and release-notes.sh. Run from anywhere:
#   .github/scripts/test-release-scripts.sh
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

ok()   { echo "  ok    $1"; }
bad()  { echo "  FAIL  $1"; fail=1; }

cat > "$tmp/CHANGELOG.md" <<'MD'
# Changelog

## [Unreleased]

### Added
- something unreleased

## [1.2.3] - 2026-01-02

### Fixed
- the 1.2.3 fix

## [1.2.2] - 2025-12-01

- older
MD

# meta <expected-exit> <event> <ref|version> -> runs release-meta.sh, leaves outputs in $tmp/out
meta() {
  local event="$1" ref="$2"
  : > "$tmp/out"
  GITHUB_EVENT_NAME="$event" GITHUB_REF_NAME="$ref" INPUT_VERSION="$ref" GITHUB_SHA=deadbeef \
    GITHUB_OUTPUT="$tmp/out" CHANGELOG="$tmp/CHANGELOG.md" SKIP_GIT_CHECKS=1 \
    "$here/release-meta.sh" >/dev/null 2>&1
}
get() { grep "^$1=" "$tmp/out" | cut -d= -f2-; }

expect_ok() { # name event ref key=value...
  local name="$1" event="$2" ref="$3"; shift 3
  if ! meta "$event" "$ref"; then bad "$name (script failed)"; return; fi
  for kv in "$@"; do
    local k="${kv%%=*}" v="${kv#*=}"
    if [ "$(get "$k")" != "$v" ]; then bad "$name: $k='$(get "$k")', want '$v'"; return; fi
  done
  ok "$name"
}
expect_fail() { # name event ref
  if meta "$2" "$3"; then bad "$1 (expected a failure)"; else ok "$1"; fi
}

echo "release-meta.sh"
expect_ok "stable tag moves the floating tags" push v1.2.3 version=1.2.3 prerelease=false publish=true "tags=1.2.3 1.2 1 latest"
expect_ok "pre-release gets only its exact tag" push v1.2.3-rc.1 version=1.2.3-rc.1 prerelease=true publish=true tags=1.2.3-rc.1
expect_ok "pre-release does not need a changelog section" push v9.9.9-beta.2 prerelease=true
expect_ok "0.x versions" push v0.4.0-rc.1 version=0.4.0-rc.1 tags=0.4.0-rc.1
expect_ok "dry run never publishes" workflow_dispatch 0.0.0-dryrun version=0.0.0-dryrun publish=false
expect_ok "dry run accepts a leading v" workflow_dispatch v3.0.0 version=3.0.0 publish=false
expect_fail "stable release without a changelog heading" push v4.5.6
expect_fail "missing patch number" push v1.2
expect_fail "not a version" push vfoo
expect_fail "leading zero" push v01.2.3
expect_fail "empty pre-release" push v1.2.3-
expect_fail "dry run with an invalid version" workflow_dispatch not-a-version

echo "release-meta.sh: the tag must be on main"
repo="$tmp/repo"; mkdir "$repo"
(
  cd "$repo" && git init -q && git config user.email t@t && git config user.name t
  git commit -q --allow-empty -m main1 && git update-ref refs/remotes/origin/main HEAD
  git checkout -q -b side && git commit -q --allow-empty -m side
) >/dev/null 2>&1
main_sha="$(git -C "$repo" rev-parse origin/main)"; side_sha="$(git -C "$repo" rev-parse side)"
run_git() { (cd "$repo" && GITHUB_EVENT_NAME=push GITHUB_REF_NAME=v1.2.3-rc.1 GITHUB_SHA="$1" GITHUB_OUTPUT=/dev/null CHANGELOG="$tmp/CHANGELOG.md" "$here/release-meta.sh" >/dev/null 2>&1); }
if run_git "$main_sha"; then ok "a tag on main is accepted"; else bad "a tag on main was rejected"; fi
if run_git "$side_sha"; then bad "a tag NOT on main was accepted"; else ok "a tag not on main is rejected"; fi

echo "release-notes.sh"
notes="$(CHANGELOG="$tmp/CHANGELOG.md" "$here/release-notes.sh" 1.2.3)"
case "$notes" in *"the 1.2.3 fix"*) ok "returns the section";; *) bad "section missing";; esac
case "$notes" in *"older"*|*"something unreleased"*) bad "leaked into a neighbouring section";; *) ok "stops at the next heading";; esac
[ -z "$(CHANGELOG="$tmp/CHANGELOG.md" "$here/release-notes.sh" 7.7.7)" ] && ok "unknown version prints nothing" || bad "unknown version printed text"
[ -n "$(CHANGELOG="$tmp/CHANGELOG.md" "$here/release-notes.sh" 1.2.2)" ] && ok "last section is found" || bad "last section missing"

echo
if [ "$fail" -eq 0 ]; then echo "all release script tests passed"; else echo "SOME TESTS FAILED"; fi
exit "$fail"
