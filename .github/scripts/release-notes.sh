#!/usr/bin/env bash
# Prints the CHANGELOG.md section of a version (without its heading), e.g.
#   release-notes.sh 1.2.3   ->   the text under "## [1.2.3]" up to the next "## [" heading.
# Prints nothing if the version has no section (the caller supplies a fallback).
set -euo pipefail
version="${1:?usage: release-notes.sh VERSION}"
changelog="${CHANGELOG:-CHANGELOG.md}"
awk -v h="## [${version}]" 'index($0,h)==1{p=1;next} p&&/^## \[/{exit} p' "$changelog"
