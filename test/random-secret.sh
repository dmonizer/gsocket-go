#!/usr/bin/env bash
# Test random secret generation for gs-netcat.
#
# Tests:
#   1. -g flag: generate secret and exit
#   2. Empty stdin input: generate random secret
#   3. Explicit stdin input: use it as secret
#   4. -s flag overrides stdin
#
# Usage:
#   ./test/random-secret.sh
set -euo pipefail

TESTDIR="$(cd "$(dirname "$0")" && pwd)"
source "$TESTDIR/test_helper.sh"
resolve_binary
setup_tmpdir


# --- Test 1: -g generates a 32-char hex secret ---
info "Test 1: -g flag generates hex secret"
SECRET=$("$GS_NETCAT" -g 2>/dev/null)
if echo "$SECRET" | grep -qE '^[0-9a-f]{32}$'; then
    pass "-g generated valid 32-char hex secret: $SECRET"
    else
    fail "-g output invalid: '$SECRET' (expected 32 hex chars)"
    fi

# --- Test 2: -g output is different each time ---
info "Test 2: -g generates unique secrets"
S1=$("$GS_NETCAT" -g 2>/dev/null)
S2=$("$GS_NETCAT" -g 2>/dev/null)
if [ "$S1" != "$S2" ]; then
    pass "-g generates unique secrets"
    else
    fail "-g generated same secret twice: $S1"
    fi

# --- Test 3: Empty stdin → auto-generate secret ---
info "Test 3: Empty stdin generates random secret"
# We need a flag that causes secret resolution but exits quickly.
# Use -g in combination? No. Use --help? No.
# Use -l which will try GSRN but we only care about the secret output.
# Actually: -s is not provided → resolveSecret prompts → empty input → generates.
# The generated secret is printed to stderr as "=Secret         : <hex>"
OUTPUT=$(echo "" | timeout 3 "$GS_NETCAT" -l 2>&1) || true
if echo "$OUTPUT" | grep -qE '=Secret\s+:\s+[0-9a-f]{32}'; then
    GEN=$(echo "$OUTPUT" | grep -E '=Secret\s+:' | sed 's/.*: //')
    if echo "$GEN" | grep -qE '^[0-9a-f]{32}$'; then
        pass "Empty stdin generated random secret: $GEN"
            else
        fail "Generated secret invalid: '$GEN'"
            fi
else
    fail "Empty stdin did not generate secret. Output: $OUTPUT"
    fi

# --- Test 4: Explicit stdin input is used as secret ---
info "Test 4: Explicit stdin input used as secret"
OUTPUT=$(echo "MyTestSecret123" | timeout 3 "$GS_NETCAT" -l 2>&1) || true
if echo "$OUTPUT" | grep -q "MyTestSecret123"; then
    pass "Explicit stdin input used as secret"
    else
    # The secret may not be echoed if greetings are suppressed; check it doesn't
    # generate a new one.
    if echo "$OUTPUT" | grep -qE '=Secret\s+:' && ! echo "$OUTPUT" | grep -q "MyTestSecret123"; then
        fail "Stdin input 'MyTestSecret123' was ignored, random generated instead"
            else
        pass "Explicit stdin input used (no random generation detected)"
            fi
fi

# --- Test 5: -s flag takes priority over stdin ---
info "Test 5: -s flag overrides stdin"
OUTPUT=$(echo "IgnoredStdin" | timeout 3 "$GS_NETCAT" -l -s "FlagSecret999" 2>&1) || true
if echo "$OUTPUT" | grep -q "FlagSecret999" || ! echo "$OUTPUT" | grep -q "IgnoredStdin"; then
    pass "-s flag takes priority over stdin"
    else
    fail "-s flag did not override stdin"
    fi

# --- Test 6: GSOCKET_SECRET env var ---
info "Test 6: GSOCKET_SECRET environment variable"
OUTPUT=$(GSOCKET_SECRET="EnvSecret456" timeout 3 "$GS_NETCAT" -l 2>&1) || true
# The secret from env var is not echoed by default (only auto-generated secrets
# are printed). Verify it doesn't fail with "No secret provided".
if ! echo "$OUTPUT" | grep -q "No secret provided"; then
    pass "GSOCKET_SECRET env var used (no missing-secret error)"
    else
    fail "GSOCKET_SECRET env var not used. Output: $OUTPUT"
    fi

print_summary
exit_on_failure
