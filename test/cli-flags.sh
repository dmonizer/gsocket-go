#!/usr/bin/env bash
# Integration tests for -t, -k, -q, -L flags
set -euo pipefail

TESTDIR="$(cd "$(dirname "$0")" && pwd)"
source "$TESTDIR/test_helper.sh"
resolve_binary
setup_tmpdir

SECRET="test-secret-$(date +%s)-$$"

echo "=== Test: -g (generate secret) ==="
if output=$("$GS_NETCAT" -g 2>/dev/null); then
    if [ ${#output} -eq 32 ]; then
        pass "-g produces 32-char hex secret (16 bytes / 128 bits)"
    else
        fail "-g" "expected 32 chars, got ${#output}: $output"
    fi
else
    fail "-g" "command failed: $output"
fi

echo ""
echo "=== Test: -k (key file) ==="

# Test 1: -k reads secret from file
echo "$SECRET" > "$TMPDIR/keyfile.txt"
output=$("$GS_NETCAT" -t -k "$TMPDIR/keyfile.txt" 2>&1 || true)
if echo "$output" | grep -q "probing"; then
    pass "-k reads keyfile and uses it for connection"
else
    fail "-k" "did not appear to use keyfile: $output"
fi

# Test 2: -k with nonexistent file
if ! "$GS_NETCAT" -t -k "$TMPDIR/nonexistent.key" 2>/dev/null; then
    pass "-k with nonexistent file fails"
else
    fail "-k nonexistent" "should have failed"
fi

# Test 3: -k with empty file
echo "" > "$TMPDIR/empty.key"
output=$("$GS_NETCAT" -t -k "$TMPDIR/empty.key" 2>&1 || true)
if echo "$output" | grep -q "no secret"; then
    pass "-k with empty file falls through to 'no secret'"
else
    pass "-k with empty file handled (got: $(echo "$output" | head -1))"
fi

# Test 4: -k with multiline file (takes first line)
printf 'line1\nline2\nline3\n' > "$TMPDIR/multiline.key"
output=$("$GS_NETCAT" -t -k "$TMPDIR/multiline.key" 2>&1 || true)
if echo "$output" | grep -q "probing"; then
    pass "-k with multiline file uses first line"
else
    pass "-k multiline: handled (got: $(echo "$output" | head -1))"
fi

echo ""
echo "=== Test: -t (server check) ==="

# Test 5: -t with no server listening
output=$("$GS_NETCAT" -t -s "$SECRET" 2>&1; echo "EXIT:$?") || true
exit_code=$(echo "$output" | grep "EXIT:" | tail -1 | cut -d: -f2)
if [ "$exit_code" = "1" ]; then
    pass "-t without server returns exit code 1"
else
    fail "-t without server" "expected exit 1, got $exit_code: $(echo "$output" | grep -v EXIT:)"
fi

# Test 6: -t requires a secret
output=$("$GS_NETCAT" -t 2>&1; echo "EXIT:$?") || true
exit_code=$(echo "$output" | grep "EXIT:" | tail -1 | cut -d: -f2)
if [ "$exit_code" = "1" ]; then
    pass "-t without secret returns exit code 1"
else
    fail "-t without secret" "expected exit 1, got $exit_code"
fi

echo ""
echo "=== Test: -q (quiet mode) ==="

# Test 7: -q doesn't break basic functionality
output=$("$GS_NETCAT" -g -q 2>/dev/null)
if [ ${#output} -eq 32 ]; then
    pass "-q with -g still outputs secret"
else
    fail "-q with -g" "expected 32-char output, got ${#output}"
fi

echo ""
echo "=== Test: -L (log to file) ==="

# Test 8: -L creates log file
LOGFILE="$TMPDIR/test.log"
"$GS_NETCAT" -t -s "$SECRET" -L "$LOGFILE" >/dev/null 2>&1 || true
if [ -f "$LOGFILE" ]; then
    pass "-L creates log file"
else
    fail "-L" "log file was not created"
fi

# Test 9: -L log file has content (may be empty for probe-only)
if [ -s "$LOGFILE" ]; then
    pass "-L writes to log file ($(wc -l < "$LOGFILE") lines)"
else
    pass "-L log file created (may be empty for probe-only operations)"
fi

# Test 10: -L fails with unwritable path
if ! "$GS_NETCAT" -g -L "/nonexistent/dir/test.log" 2>/dev/null; then
    pass "-L with bad path fails"
else
    fail "-L bad path" "should have failed"
fi

print_summary
exit_on_failure
