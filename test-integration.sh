#!/usr/bin/env bash
set -euo pipefail

BINARY="${1:-./gs-netcat-linux}"
SECRET="test-secret-$$-$(date +%s)"
TEXT="Hello gs-netcat integration test $(date +%s)"
OUTFILE=$(mktemp)
ERRFILE=$(mktemp)
CLIENT_ERR=$(mktemp)
TIMEOUT=30

cleanup() {
    kill %1 2>/dev/null || true
    wait 2>/dev/null || true
    rm -f "$OUTFILE" "$ERRFILE" "$CLIENT_ERR"
}
trap cleanup EXIT

echo "=== gs-netcat Integration Test ==="
echo "Binary: $BINARY"
echo "Secret: $SECRET"
echo "Text:   $TEXT"
echo ""

# Check binary exists and is executable
if [[ ! -x "$BINARY" ]]; then
    echo "ERROR: Binary '$BINARY' not found or not executable"
    exit 1
fi

# Start listener in background with stdin from /dev/null.
# The listener's stdin goroutine gets EOF, exits, and RunShell returns.
# But we also need the listener to STAY ALIVE long enough for the client.
# So we use a trick: start a sleep in a subshell to keep stdout open.
#
# Actually, simpler: redirect stdin but don't worry — the channel→stdout
# goroutine reads data and writes to stdout BEFORE the stdin goroutine's
# defer p.Close() tears down the connection. The TCP data is already
# buffered in the kernel.
"$BINARY" -s "$SECRET" -l < /dev/null > "$OUTFILE" 2> "$ERRFILE" &
LISTENER_PID=$!
echo "Listener started (PID=$LISTENER_PID)"

# Wait for listener to register with GSRN.
REGISTERED=0
for i in $(seq 1 "$TIMEOUT"); do
    if grep -q "Waiting" "$ERRFILE" 2>/dev/null; then
        REGISTERED=1
        echo "Listener registered after ${i}s"
        break
    fi
    if ! kill -0 "$LISTENER_PID" 2>/dev/null; then
        echo ""
        echo "ERROR: Listener died before registering. Stderr:"
        cat "$ERRFILE"
        exit 1
    fi
    sleep 1
done

if [[ $REGISTERED -eq 0 ]]; then
    echo ""
    echo "ERROR: Listener did not register within ${TIMEOUT}s. Stderr:"
    cat "$ERRFILE"
    exit 1
fi

# Start client: pipe text through stdin.
echo "Starting client..."
echo "$TEXT" | "$BINARY" -s "$SECRET" 2> "$CLIENT_ERR" &
CLIENT_PID=$!

# Wait for client to finish, with timeout.
CLIENT_DONE=0
for i in $(seq 1 "$TIMEOUT"); do
    if ! kill -0 "$CLIENT_PID" 2>/dev/null; then
        CLIENT_DONE=1
        echo "Client finished after ${i}s"
        break
    fi
    sleep 1
done

if [[ $CLIENT_DONE -eq 0 ]]; then
    echo "WARNING: Client did not exit within ${TIMEOUT}s, killing..."
    kill "$CLIENT_PID" 2>/dev/null || true
    wait "$CLIENT_PID" 2>/dev/null || true
fi

# Wait for listener to exit (stdin from /dev/null should cause it to exit
# after the client disconnects), with timeout.
LISTENER_DONE=0
for i in $(seq 1 5); do
    if ! kill -0 "$LISTENER_PID" 2>/dev/null; then
        LISTENER_DONE=1
        echo "Listener exited after client disconnect"
        break
    fi
    sleep 1
done

if [[ $LISTENER_DONE -eq 0 ]]; then
    echo "Listener still running, sending SIGINT..."
    kill -INT "$LISTENER_PID" 2>/dev/null || true
    sleep 1
    if kill -0 "$LISTENER_PID" 2>/dev/null; then
        echo "Listener didn't respond to SIGINT, sending SIGTERM..."
        kill -TERM "$LISTENER_PID" 2>/dev/null || true
        wait "$LISTENER_PID" 2>/dev/null || true
    fi
fi

# Verify the text arrived.
echo ""
echo "=== Verification ==="
if grep -qF "$TEXT" "$OUTFILE"; then
    echo "SUCCESS: Test text found in output file"
    exit 0
else
    echo "ERROR: Test text NOT found in output file"
    echo ""
    echo "--- Listener stderr ---"
    cat "$ERRFILE"
    echo ""
    echo "--- Client stderr ---"
    cat "$CLIENT_ERR"
    echo ""
    echo "--- Listener stdout (hexdump) ---"
    if [[ -s "$OUTFILE" ]]; then
        xxd "$OUTFILE" || cat -v "$OUTFILE"
    else
        echo "(empty)"
    fi
    exit 1
fi
