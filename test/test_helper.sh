#!/usr/bin/env bash
# Shared test helpers for gs-netcat integration tests.
# Source this file at the beginning of each test script:
#   source "$(dirname "$0")/test_helper.sh"
#
# Provides:
#   - Color output (pass/fail/info)
#   - Binary auto-detection and build
#   - Temporary directory with automatic cleanup
#   - PASSED/FAILED counters and summary

set -euo pipefail

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# --- Test reporting ---
PASSED=${PASSED:-0}
FAILED=${FAILED:-0}

pass() { echo -e "${GREEN}PASS${NC} $*"; PASSED=$((PASSED + 1)); }
fail() { echo -e "${RED}FAIL${NC} $*"; FAILED=$((FAILED + 1)); }
info() { echo -e "${YELLOW}INFO${NC} $*"; }

# --- Binary detection ---
# GS_NETCAT can be set before sourcing to override.
# Otherwise auto-detect: look for built binary, then build from source.
resolve_binary() {
    if [ -n "${GS_NETCAT:-}" ] && [ -x "$GS_NETCAT" ]; then
        return 0
    fi

    # Try common locations relative to test/ directory.
    local testdir
    testdir="$(cd "$(dirname "${BASH_SOURCE[1]}")" && pwd)"

    for candidate in \
        "$testdir/../gs-netcat" \
        "$testdir/../build/linux/gs-netcat-amd64" \
        "$testdir/../build/$(go env GOOS 2>/dev/null || echo linux)/gs-netcat-$(go env GOARCH 2>/dev/null || echo amd64)"; do
        if [ -x "$candidate" ]; then
            GS_NETCAT="$candidate"
            return 0
        fi
    done

    # Build from source if Go is available.
    if command -v go >/dev/null 2>&1 && [ -f "$testdir/../cmd/gs-netcat/main.go" ]; then
        info "Building gs-netcat..."
        (cd "$testdir/.." && go build -o gs-netcat ./cmd/gs-netcat) || {
            fail "build failed"
            return 1
        }
        GS_NETCAT="$testdir/../gs-netcat"
        return 0
    fi

    fail "gs-netcat binary not found. Set GS_NETCAT= or install Go."
    return 1
}

# --- Temporary directory ---
# Creates a tmpdir and sets up EXIT trap for cleanup.
# Export TMPDIR for use in tests; add custom cleanup via TMPDIR_CLEANUP hook.
TMPDIR=${TMPDIR:-$(mktemp -d)}
TMPDIR_CLEANUP=${TMPDIR_CLEANUP:-}

setup_tmpdir() {
    mkdir -p "$TMPDIR"
    # shellcheck disable=SC2064
    trap 'rm -rf "$TMPDIR"; ${TMPDIR_CLEANUP:-true}' EXIT
}

# --- Summary ---
print_summary() {
    echo ""
    echo "===================="
    echo "Results: $PASSED passed, $FAILED failed"
    echo "===================="
}

exit_on_failure() {
    if [ "$FAILED" -gt 0 ]; then
        exit 1
    fi
    exit 0
}
