#!/usr/bin/env bash
# SOCKS5 integration test for gs-netcat.
#
# Architecture:
#   [echo server :EPORT] ← [gs-netcat server -l -S] ←GSRN→ [gs-netcat client -p CPORT]
#                                                           ↑
#                                              [SOCKS5 client connects to CPORT]
#
# The SOCKS5 client connects to CPORT, sends a CONNECT request for
# 127.0.0.1:EPORT, and expects the GS tunnel + SOCKS5 server to relay
# data to/from the echo server.
#
# Usage:
#   ./test/socks-integration.sh              # auto-generate secret
#   ./test/socks-integration.sh MySecret     # use specific secret
set -euo pipefail

GS_NETCAT="${GS_NETCAT:-./gs-netcat}"
SECRET="${1:-test-socks-$(date +%s)}"
TIMEOUT=30

# --- helpers ---
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}PASS${NC} $*"; }
fail() { echo -e "${RED}FAIL${NC} $*"; exit 1; }
info() { echo -e "${YELLOW}INFO${NC} $*"; }

cleanup() {
    info "Cleaning up..."
    kill $SERVER_PID $CLIENT_PID $ECHO_PID 2>/dev/null || true
    wait $SERVER_PID $CLIENT_PID $ECHO_PID 2>/dev/null || true
}
trap cleanup EXIT

# --- check binary ---
if [ ! -x "$GS_NETCAT" ]; then
    # Try building it.
    if [ -f "./cmd/gs-netcat/main.go" ]; then
        info "Building gs-netcat..."
        (cd "$(dirname "$0")/.." && go build -o gs-netcat ./cmd/gs-netcat) || fail "build failed"
        GS_NETCAT="./gs-netcat"
    else
        fail "gs-netcat binary not found at $GS_NETCAT"
    fi
fi

# --- 1. Start a TCP echo server ---
info "Starting echo server..."
EPORT=$(python3 -c '
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
s.listen(1)
print(s.getsockname()[1])
sys.stdout.flush()
# Echo: read line, write it back, close.
conn, _ = s.accept()
data = conn.recv(1024)
conn.sendall(data)
conn.close()
s.close()
' 2>&1 &)
ECHO_PID=$!
# The python script forks itself; the port is printed by the child before
# the parent does echo work. Wait a bit for the port.
sleep 0.3
# EPORT is tricky to get from the python output. Let me use a temp file.
# Actually let me rewrite this more carefully.
kill $ECHO_PID 2>/dev/null || true

# Use a temp-file approach.
TMPDIR=$(mktemp -d)
mkfifo "$TMPDIR/echo_port"

python3 -c "
import socket, sys, os
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(('127.0.0.1', 0))
s.listen(1)
port = s.getsockname()[1]
with open('$TMPDIR/echo_port', 'w') as f:
    f.write(str(port))
conn, _ = s.accept()
data = conn.recv(4096)
conn.sendall(data)
conn.close()
s.close()
" &
ECHO_PID=$!

EPORT=$(cat "$TMPDIR/echo_port")
info "Echo server on 127.0.0.1:$EPORT (pid=$ECHO_PID)"

# --- 2. Start gs-netcat SOCKS5 server ---
info "Starting gs-netcat SOCKS5 server..."
"$GS_NETCAT" -l -S -s "$SECRET" -v &
SERVER_PID=$!
sleep 1

if ! kill -0 $SERVER_PID 2>/dev/null; then
    fail "gs-netcat server failed to start"
fi
info "Server PID: $SERVER_PID"

# --- 3. Start gs-netcat client listener ---
# Pick a free port for the client SOCKS5 listener.
CPORT=$(python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
')
info "Starting gs-netcat client on 127.0.0.1:$CPORT..."
"$GS_NETCAT" -p "$CPORT" -s "$SECRET" -v &
CLIENT_PID=$!
sleep 2

if ! kill -0 $CLIENT_PID 2>/dev/null; then
    fail "gs-netcat client failed to start"
fi
info "Client PID: $CLIENT_PID"

# --- 4. Run SOCKS5 client ---
info "Running SOCKS5 test client..."

RESULT=$(python3 -c "
import socket, struct, sys, time

# Connect to the GS-netcat SOCKS5 proxy.
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.settimeout(10)
try:
    s.connect(('127.0.0.1', $CPORT))
except Exception as e:
    print('FAIL: connect to proxy: ' + str(e))
    sys.exit(0)

# SOCKS5 greeting: ver=5, 1 method, NO AUTH.
s.sendall(b'\x05\x01\x00')
resp = s.recv(2)
if resp != b'\x05\x00':
    print('FAIL: auth choice: ' + resp.hex())
    sys.exit(0)

# SOCKS5 CONNECT to echo server.
host = b'127.0.0.1'
port = $EPORT
req = b'\x05\x01\x00\x01' + socket.inet_aton('127.0.0.1') + struct.pack('!H', port)
s.sendall(req)
hdr = s.recv(4)
if len(hdr) < 4 or hdr[1] != 0:
    print('FAIL: connect reply code=' + str(hdr[1]) if len(hdr) >= 2 else 'FAIL: short reply')
    sys.exit(0)

# Read remaining bind address.
addr_type = hdr[3]
if addr_type == 1:
    s.recv(4 + 2)  # IPv4 + port
elif addr_type == 3:
    n = ord(s.recv(1))
    s.recv(n + 2)
elif addr_type == 4:
    s.recv(16 + 2)

# Send test message, expect echo.
msg = b'HELLO_SOCKS5'
s.sendall(msg)
reply = b''
while len(reply) < len(msg):
    chunk = s.recv(len(msg) - len(reply))
    if not chunk:
        break
    reply += chunk

if reply == msg:
    print('PASS: echo match: ' + reply.decode())
else:
    print('FAIL: got ' + repr(reply) + ' want ' + repr(msg))

s.close()
" 2>&1)

info "$RESULT"

if echo "$RESULT" | grep -q "^PASS"; then
    pass "SOCKS5 integration test passed"
else
    fail "SOCKS5 integration test failed: $RESULT"
fi
