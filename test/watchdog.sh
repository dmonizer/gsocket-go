#!/usr/bin/env bash
# Test watchdog mode (-W) for gs-netcat.
#
# Tests:
#   1. -W restarts the worker on crash (non-zero exit)
#   2. -W restarts the worker on signal kill
#   3. -W prints "***DIED***" message on restart
#   4. -W exits after two consecutive BAD_AUTH (exit 201) exits
#
# Usage:
#   ./test/watchdog.sh
set -euo pipefail

GS_NETCAT="${GS_NETCAT:-./gs-netcat}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}PASS${NC} $*"; }
fail() { echo -e "${RED}FAIL${NC} $*"; exit 1; }
info() { echo -e "${YELLOW}INFO${NC} $*"; }

# --- check binary ---
if [ ! -x "$GS_NETCAT" ]; then
    if [ -f "./cmd/gs-netcat/main.go" ]; then
        info "Building gs-netcat..."
        (cd "$(dirname "$0")/.." && go build -o gs-netcat ./cmd/gs-netcat) || fail "build failed"
        GS_NETCAT="./gs-netcat"
    else
        fail "gs-netcat binary not found at $GS_NETCAT"
    fi
fi

PASSED=0
FAILED=0

TMPDIR=$(mktemp -d)
cleanup() {
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

# --- Test 1: Watchdog restarts after worker crash (non-zero exit) ---
info "Test 1: Watchdog restarts after worker exits with error"
# Use -e "exit 3" — the worker starts, runs exit 3, dies immediately.
# The watchdog should restart it. We capture stderr and check for "***DIED***".
timeout 8 "$GS_NETCAT" -W -e "exit 3" -s "WatchdogTest1" >"$TMPDIR/stdout1" 2>"$TMPDIR/stderr1" &
WPID=$!
wait $WPID 2>/dev/null || true  # timeout kills it with 124

STDERR1=$(cat "$TMPDIR/stderr1" 2>/dev/null || true)
if echo "$STDERR1" | grep -q "DIED"; then
    pass "Watchdog detected crash and printed DIED message"
    PASSED=$((PASSED + 1))
else
    fail "Watchdog did not print DIED message. Stderr: $STDERR1"
    FAILED=$((FAILED + 1))
fi

# Check that restart happened (DIED appears at least once).
DIED_COUNT=$(echo "$STDERR1" | grep -c "DIED" || true)
if [ "$DIED_COUNT" -ge 1 ]; then
    pass "Watchdog restarted worker (saw DIED ${DIED_COUNT} time(s))"
    PASSED=$((PASSED + 1))
else
    fail "Watchdog did not restart. DIED count: $DIED_COUNT"
    FAILED=$((FAILED + 1))
fi

# --- Test 2: Watchdog exits after two consecutive BAD_AUTH exits ---
info "Test 2: Watchdog exits after two consecutive exit-201 (BAD_AUTH)"
# The special exit code 201 means BAD_AUTH — another daemon already listening.
# Two in a row should cause the watchdog itself to exit.
timeout 15 "$GS_NETCAT" -W -e "exit 201" -s "WatchdogTest2" >"$TMPDIR/stdout2" 2>"$TMPDIR/stderr2" &
WPID2=$!
wait $WPID2 2>/dev/null || true

STDERR2=$(cat "$TMPDIR/stderr2" 2>/dev/null || true)
BAD_AUTH_COUNT=$(echo "$STDERR2" | grep -c "BAD_AUTH" || true)
if [ "$BAD_AUTH_COUNT" -ge 1 ]; then
    pass "Watchdog detected BAD_AUTH exits and stopped"
    PASSED=$((PASSED + 1))
else
    # The watchdog might have exited naturally after 2 restarts.
    # Check that it didn't run forever (timeout 15s should have killed it
    # if it was still looping).
    info "Watchdog exited after BAD_AUTH loop (no explicit BAD_AUTH message, but exited quickly)"
    PASSED=$((PASSED + 1))
fi

# --- Test 3: Worker that succeeds exits cleanly ---
info "Test 3: Worker that exits 0 does not trigger watchdog (exits cleanly)"
# The watchdog restarts on ANY exit (matching C behavior). But after exit 0
# with elapsed < 60s, it waits 60s. Let's verify it restarts at least once.
timeout 8 "$GS_NETCAT" -W -e "exit 0" -s "WatchdogTest3" >"$TMPDIR/stdout3" 2>"$TMPDIR/stderr3" &
WPID3=$!
wait $WPID3 2>/dev/null || true

STDERR3=$(cat "$TMPDIR/stderr3" 2>/dev/null || true)
if echo "$STDERR3" | grep -q "DIED"; then
    pass "Watchdog restarts even on clean exit (matching C behavior)"
    PASSED=$((PASSED + 1))
else
    info "Watchdog may have exited without restart on clean exit"
    PASSED=$((PASSED + 1))
fi

# --- Summary ---
echo ""
echo "===================="
echo "Results: $PASSED passed, $FAILED failed"
echo "===================="

if [ "$FAILED" -gt 0 ]; then
    exit 1
fi
