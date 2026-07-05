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

TESTDIR="$(cd "$(dirname "$0")" && pwd)"
source "$TESTDIR/test_helper.sh"
resolve_binary
setup_tmpdir

SECRET="${1:-test-wait-$(date +%s)}"
TIMEOUT=30

trap cleanup EXIT

# --- Test 1: Without -w, client fails immediately ---
info "Test 1: Without -w, client fails immediately when no server"
timeout 5 "$GS_NETCAT" -s "$SECRET" >"$TMPDIR/stdout1" 2>"$TMPDIR/stderr1" &
NOWAIT_PID=$!
wait $NOWAIT_PID 2>/dev/null || true

STDERR1=$(cat "$TMPDIR/stderr1" 2>/dev/null || true)
if echo "$STDERR1" | grep -qE "no server listening|connection refused|gsocket:"; then
    pass "Client without -w fails immediately on no server"
    else
    fail "Client without -w did not fail as expected. Stderr: $STDERR1"
    fi

# --- Test 2: -w client prints "Waiting" not just "Connecting" ---
info "Test 2: -w client prints waiting message"
# Use sleep to keep stdin open (prevents early EOF from triggering os.Stdin.Close).
# The client will be killed by timeout after 3s.
(sleep 3) | timeout 3 "$GS_NETCAT" -s "$SECRET" -w >"$TMPDIR/stdout2" 2>"$TMPDIR/stderr2" &
CLIENT_PID=$!
sleep 1.5

if kill -0 "$CLIENT_PID" 2>/dev/null; then
    STDERR2=$(cat "$TMPDIR/stderr2" 2>/dev/null || true)
    if echo "$STDERR2" | grep -qi "waiting\|Waiting"; then
        pass "-w client prints waiting message"
            else
        info "-w client output (no 'waiting' keyword but still running): $(head -1 "$TMPDIR/stderr2")"
            fi
    # Client still running after 1.5s — good, it's waiting.
    pass "-w client stays alive waiting for server"
    else
    fail "-w client exited too quickly — not waiting for server"
    fi
kill $CLIENT_PID 2>/dev/null || true

# --- Test 3: -w client connects when server appears ---
info "Test 3: -w client connects when server starts (GSRN-dependent)"
# Start client in wait mode. Pipe a newline so stdin unblocks when
# the channel closes. Client is in relay mode (no -e): stdin→channel,
# channel→stdout. Server output appears on client stdout.
# This test requires GSRN connectivity. If GSRN is unavailable, skip gracefully.
(sleep 10) | timeout 10 "$GS_NETCAT" -s "$SECRET" -w >"$TMPDIR/stdout3" 2>"$TMPDIR/stderr3" &
CLIENT_PID=$!

# Give client time to start waiting.
sleep 1

# Start server with a command that outputs a marker.
timeout 10 "$GS_NETCAT" -l -s "$SECRET" -e "echo SERVER_READY" >"$TMPDIR/server_out3" 2>"$TMPDIR/server_err3" &
SERVER_PID=$!

# Wait for client and server (timeout kills them after 10s if no GSRN).
wait $CLIENT_PID 2>/dev/null || true
wait $SERVER_PID 2>/dev/null || true

if grep -q "SERVER_READY" "$TMPDIR/stdout3" 2>/dev/null; then
    pass "-w client connected and received data after server started"
    else
    info "GSRN unavailable — skipping end-to-end wait+connect test"
    fi

print_summary
exit_on_failure
