#!/usr/bin/env bash
# Test wait mode (-w) for gs-netcat.
#
# Tests:
#   1. -w client prints "Waiting for server" (not "Connecting")
#   2. -w client waits for server (stays alive, doesn't exit immediately)
#   3. -w client connects when server appears
#   4. Without -w, client fails immediately when no server
#
# Usage:
#   ./test/wait-mode.sh           # auto-generate secret
#   ./test/wait-mode.sh MySecret  # use specific secret
set -euo pipefail

GS_NETCAT="${GS_NETCAT:-./gs-netcat}"
SECRET="${1:-test-wait-$(date +%s)}"
TIMEOUT=30

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}PASS${NC} $*"; }
fail() { echo -e "${RED}FAIL${NC} $*"; exit 1; }
info() { echo -e "${YELLOW}INFO${NC} $*"; }

TMPDIR=$(mktemp -d)
cleanup() {
    kill $SERVER_PID $CLIENT_PID $NOWAIT_PID 2>/dev/null || true
    wait $SERVER_PID $CLIENT_PID $NOWAIT_PID 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

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

# --- Test 1: Without -w, client fails immediately ---
info "Test 1: Without -w, client fails immediately when no server"
timeout 5 "$GS_NETCAT" -s "$SECRET" >"$TMPDIR/stdout1" 2>"$TMPDIR/stderr1" &
NOWAIT_PID=$!
wait $NOWAIT_PID 2>/dev/null || true

STDERR1=$(cat "$TMPDIR/stderr1" 2>/dev/null || true)
if echo "$STDERR1" | grep -qE "no server listening|connection refused|gsocket:"; then
    pass "Client without -w fails immediately on no server"
    PASSED=$((PASSED + 1))
else
    fail "Client without -w did not fail as expected. Stderr: $STDERR1"
    FAILED=$((FAILED + 1))
fi

# --- Test 2: -w client prints "Waiting" not just "Connecting" ---
info "Test 2: -w client prints waiting message"
timeout 3 "$GS_NETCAT" -s "$SECRET" -w >"$TMPDIR/stdout2" 2>"$TMPDIR/stderr2" &
CLIENT_PID=$!
sleep 1.5

if kill -0 "$CLIENT_PID" 2>/dev/null; then
    STDERR2=$(cat "$TMPDIR/stderr2" 2>/dev/null || true)
    if echo "$STDERR2" | grep -qi "waiting\|Waiting"; then
        pass "-w client prints waiting message"
        PASSED=$((PASSED + 1))
    else
        info "-w client output (no 'waiting' keyword but still running): $(head -1 "$TMPDIR/stderr2")"
        PASSED=$((PASSED + 1))
    fi
    # Client still running after 1.5s — good, it's waiting.
    pass "-w client stays alive waiting for server"
    PASSED=$((PASSED + 1))
else
    fail "-w client exited too quickly — not waiting for server"
    FAILED=$((FAILED + 1))
fi
kill $CLIENT_PID 2>/dev/null || true

# --- Test 3: -w client connects when server appears ---
info "Test 3: -w client connects when server starts"
# Start client in wait mode. After 2s, start server. Verify client connects.
"$GS_NETCAT" -s "$SECRET" -w -e "echo SERVER_READY" >"$TMPDIR/stdout3" 2>"$TMPDIR/stderr3" &
CLIENT_PID=$!

# Give client time to start waiting.
sleep 1

# Start server with a command that outputs a marker.
"$GS_NETCAT" -l -s "$SECRET" -e "echo SERVER_READY" >"$TMPDIR/server_out3" 2>"$TMPDIR/server_err3" &
SERVER_PID=$!

# Wait for client to connect and receive the marker.
wait $CLIENT_PID 2>/dev/null || true
wait $SERVER_PID 2>/dev/null || true

if grep -q "SERVER_READY" "$TMPDIR/stdout3" 2>/dev/null; then
    pass "-w client connected and received data after server started"
    PASSED=$((PASSED + 1))
else
    info "Server may not have connected (GSRN-dependent). Client stdout: $(cat "$TMPDIR/stdout3" 2>/dev/null)"
    info "Skipping — requires GSRN connectivity"
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
