#!/usr/bin/env bash
# UDP integration test for gs-netcat.
#
# Architecture:
#   [UDP echo :EPORT] ← [gs-netcat server -l -u -d 127.0.0.1 -p EPORT]
#                              ←GSRN→
#                       [gs-netcat client -u -p CPORT] ← [UDP client → CPORT]
#
# The UDP client sends a datagram to CPORT, which is length-framed over
# the GS tunnel, forwarded by the server to the UDP echo, echoed back,
# framed back, and received by the UDP client.
#
# Usage:
#   ./test/udp-integration.sh              # auto-generate secret
#   ./test/udp-integration.sh MySecret     # use specific secret
set -euo pipefail

GS_NETCAT="${GS_NETCAT:-./gs-netcat}"
SECRET="${1:-test-udp-$(date +%s)}"
TIMEOUT=20

# --- helpers ---
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}PASS${NC} $*"; }
fail() { echo -e "${RED}FAIL${NC} $*"; exit 1; }
info() { echo -e "${YELLOW}INFO${NC} $*"; }

cleanup() {
    info "Cleaning up..."
    kill $SERVER_PID $CLIENT_PID $ECHO_PID 2>/dev/null || true
    wait $SERVER_PID $CLIENT_PID $ECHO_PID 2>/dev/null || true
    rm -rf "$TMPDIR"
}
TMPDIR=$(mktemp -d)
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

# --- 1. Start a UDP echo server ---
info "Starting UDP echo server..."
mkfifo "$TMPDIR/echo_port"

python3 -c "
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(('127.0.0.1', 0))
port = s.getsockname()[1]
with open('$TMPDIR/echo_port', 'w') as f:
    f.write(str(port))
# Echo one datagram then exit.
data, addr = s.recvfrom(4096)
s.sendto(data, addr)
s.close()
" &
ECHO_PID=$!

EPORT=$(cat "$TMPDIR/echo_port")
info "UDP echo server on 127.0.0.1:$EPORT (pid=$ECHO_PID)"

# --- 2. Start gs-netcat UDP forwarding server ---
info "Starting gs-netcat UDP forwarding server..."
"$GS_NETCAT" -l -u -d "127.0.0.1" -p "$EPORT" -s "$SECRET" -v &
SERVER_PID=$!
sleep 1

if ! kill -0 $SERVER_PID 2>/dev/null; then
    fail "gs-netcat UDP server failed to start"
fi
info "Server PID: $SERVER_PID"

# --- 3. Start gs-netcat UDP client listener ---
CPORT=$(python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
')
info "Starting gs-netcat UDP client on 127.0.0.1:$CPORT..."
"$GS_NETCAT" -u -p "$CPORT" -s "$SECRET" -v &
CLIENT_PID=$!
sleep 2

if ! kill -0 $CLIENT_PID 2>/dev/null; then
    fail "gs-netcat UDP client failed to start"
fi
info "Client PID: $CLIENT_PID"

# --- 4. Send UDP datagram and expect echo ---
info "Sending UDP test datagram..."

RESULT=$(python3 -c "
import socket, sys, time

s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(10)

msg = b'HELLO_UDP_ECHO'
try:
    s.sendto(msg, ('127.0.0.1', $CPORT))
    data, addr = s.recvfrom(4096)
    if data == msg:
        print('PASS: echo match: ' + data.decode())
    else:
        print('FAIL: got ' + repr(data) + ' want ' + repr(msg))
except socket.timeout:
    print('FAIL: timed out waiting for UDP echo')
except Exception as e:
    print('FAIL: ' + str(e))
finally:
    s.close()
" 2>&1)

info "$RESULT"

if echo "$RESULT" | grep -q "^PASS"; then
    pass "UDP integration test passed"
else
    fail "UDP integration test failed: $RESULT"
fi
