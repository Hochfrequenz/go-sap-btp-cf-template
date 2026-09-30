#!/usr/bin/env bash
# scan_gate <message> <cmd...>
#
# Wraps a scan command (rg, or grep -r) whose exit code carries three
# distinct meanings that a plain `if cmd; then ...; fi` cannot tell apart:
#
#   0 — the command found a match. For these gates a match IS the
#       violation being scanned for, so the gate must FAIL.
#   1 — no match. The tree is clean, the gate PASSES.
#   anything else (2, 127, ...) — the command itself errored: an
#       unreadable file, a bad glob, or (127) the tool missing from
#       PATH entirely. rg and GNU `grep -r` both exit 2 for this, even
#       when they already printed a real match before hitting the
#       error further down the tree — so a single unreadable file can
#       silently swallow a genuine violation elsewhere if the caller
#       only branches on zero-vs-nonzero. Treat this the same as a
#       violation: fail loudly, never pass silently.
#
# Do not rely on `set -e` inside this function to catch a failing scan
# command: `set -e` is suspended for any command that is itself part of
# a conditional (`if`, `&&`, `||`, ...), which is exactly the context
# this function's caller may run it in. The exit code is captured
# explicitly instead, with `rc=0; "$@" || rc=$?`.
#
# Call this as a plain statement, not as `if scan_gate ...; then`:
#   source "$GITHUB_WORKSPACE/.github/scripts/scan-gate.sh"
#   scan_gate "<message shown when the gate fails>" rg -n ... 'pattern' .
#
# A nonzero return under the caller's own `set -e` ends the step there,
# which is correct when the gate check is the last thing in the step.
# If the step needs to run further checks after the gate, capture the
# return with `scan_gate ... || fail=1` instead and exit at the end.
scan_gate() {
  local message="$1"
  shift
  local rc=0
  "$@" || rc=$?
  case "$rc" in
    0)
      echo "::error::${message}" >&2
      return 1
      ;;
    1)
      return 0
      ;;
    *)
      echo "::error::scan tool failed (exit $rc): $1 — treating as a gate failure, not a clean tree" >&2
      return 1
      ;;
  esac
}
