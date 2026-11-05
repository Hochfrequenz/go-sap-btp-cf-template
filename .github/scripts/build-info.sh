#!/usr/bin/env bash
# Prints the build info served at /version as key=value lines (usable with
# `>> "$GITHUB_OUTPUT"` and a `while read` loop):
#   version, commit, branch, build_date
# Run inside the git checkout. The one rule for every build (deploy, mock image, make):
#   commit  short SHA of HEAD (at least 7 chars)
#   branch  pull request: $GITHUB_HEAD_REF; attached checkout: the branch name;
#           detached checkout (tag, release, sha): "main" if HEAD is an ancestor of
#           origin/main (the checkout needs that ref, e.g. fetch-depth: 0), else "detached".
#           Never a SHA, never "HEAD".
#   version first argument only (the environment is ignored), else `git describe --tags --always`
set -eu

default_branch=main
version="${1:-}"
[ -n "$version" ] || version="$(git describe --tags --always 2>/dev/null || echo dev)"
commit="$(git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)"

if [ -n "${GITHUB_HEAD_REF:-}" ]; then
  branch="$GITHUB_HEAD_REF"
elif b="$(git symbolic-ref --short -q HEAD 2>/dev/null)" && [ -n "$b" ]; then
  branch="$b"
elif git merge-base --is-ancestor HEAD "origin/$default_branch" 2>/dev/null; then
  branch="$default_branch"
else
  branch=detached
fi

printf 'version=%s\ncommit=%s\nbranch=%s\nbuild_date=%s\n' \
  "$version" "$commit" "$branch" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
