# Ctrl-E Console — Current State & Bug Tracking

> **Branch:** `feature/code-review-and-fixes`
> **Date:** 2026-07-05

## What's Implemented

### Files changed/created:
- `gsocket/console.go` — ConsoleReader with console-mode awareness, command buffer, Ctrl-E escape filtering
- `gsocket/console_ui.go` — Console struct: split-screen terminal, status bar, command line, ANSI scroll region
- `gsocket/console_ui_test.go` — Tests for formatSize, Console command dispatch, lifecycle
- `gsocket/console_reader_test.go` — Tests for ConsoleReader passthrough and Ctrl-E escape sequences
- `gsocket/peer.go` — WithConsole option, console-aware I/O loop, BPS ticker, PWDReq/chnPWD handlers, PONG→RTT
- `gsocket/peer_pty_linux.go` — SIGWINCH→Console.HandleWinch notification
- `cmd/gs-netcat/main.go` — `-C` flag

### What works:
- `-C` flag creates Console, sets scroll region, renders status bar with duration timer
- BPS ticker computes bytes-per-second via exponential moving average (matching C)
- PONG callback updates RTT in status bar
- PWDReq handler on server replies with `os.Getwd()`; client shows path
- Ctrl-E + E/e/Ctrl-E → literal 0x05 (verified by unit tests)
- Ctrl-E + other characters → forwarded directly (verified by unit tests)
- Plain byte passthrough works (verified by unit tests)
- `-i` without `-C` works normally (ConsoleReader filters Ctrl-E, forwards everything else)

## Known Bugs

### Bug 1: Ctrl-E + ↓/↑ (arrow keys) don't enter/exit command mode

**Root cause:** Terminals emit `\033[A` for Up arrow, `\033[B` for Down arrow in raw mode. After `Ctrl-E` (0x05), the ConsoleReader's `pending` flag is set. The next byte from the arrow key is `\033` (0x1B, ESC). The `forwardEscape` function checks for `'['` but gets `0x1B`, falls through to `default:`, and emits `\033` as a plain byte. The remaining `[B` goes through as regular input. The `onFocusDown`/`onFocusUp` callbacks never fire.

**Attempted fix (reverted):** Added `0x1B` case to `forwardEscape`/`handleConsoleEscape` that consumed `[` and direction byte via new `consumeArrowBracket()` helper. **Reverted because user reported regression** — `-i` without `-C` stopped working (couldn't type, Ctrl-C broken). The exact cause of the regression is still unclear since the `0x1B` handling only activates after Ctrl-E sets `pending=true`.

**Planned fix:** After the user confirms the revert fixed the regression:
1. Re-add the `0x1B` handling but more carefully — ensure it ONLY fires when `cr.pending` is true
2. Add comprehensive ConsoleReader unit tests for arrow key sequences
3. The current `readArrowSequence()` expects `[` has already been consumed — we need `consumeArrowSequence()` that reads the full `\033[A`/`\033[B` sequence:
   ```go
   func (cr *ConsoleReader) consumeArrowSequence() {
       // Already received 0x1B (ESC). Read '[' then direction.
       bracket := make([]byte, 1)
       _, _ = cr.src.Read(bracket)
       if bracket[0] == '[' {
           dir := make([]byte, 1)
           _, _ = cr.src.Read(dir)
           switch dir[0] {
           case 'A': // Up
               if cr.onFocusUp != nil { cr.onFocusUp() }
           case 'B': // Down
               if cr.onFocusDown != nil { cr.onFocusDown() }
           }
       }
   }
   ```
4. Apply to both `forwardEscape` (shell mode) and `handleConsoleEscape` (console mode)
5. Wire with `stty` awareness — verify raw mode is active before expecting these sequences

### Bug 2: Ctrl-E + c doesn't visibly do anything

**Status:** The `shutdown()` callback IS called (sets `running=false`, clears command line). The BPS ticker now checks `Running()` and stops. But the session continues because the stdin→channel goroutine and channel→stdout loop are unaffected.

**Expected behavior from C:** Ctrl-E + c closes the console UI (status bar, command line) but keeps the shell session alive. The user can continue using the shell normally without the footer.

**Current behavior:** `shutdown()` runs, BPS ticker stops, command line clears. But the scroll region is NOT reset at shutdown time — only in `Close()` (deferred, runs on session exit). So the footer area remains reserved even though it's empty.

**Planned fix:** In `shutdown()`:
1. Reset the scroll region to full screen (`\033[r`)
2. Clear the footer lines
3. The deferred `Close()` already does this, so make `shutdown()` idempotent by setting `running=false` and having `Close()` check `running` to avoid double-cleanup

### Bug 3: `-C` without `-i` has no effect

**Root cause:** `runRelay()` and `runExecCmd()` don't check `p.consoleUI`. The Console is created (by `WithConsole()`) but never initialized or used. Without raw mode, Ctrl-E sequences aren't captured by the terminal driver anyway.

**Planned fix:** Document that `-C` requires `-i`. Optionally print a warning: `"-C requires -i to enable the console"`. Or make `-C` imply `-i`.

## Test Plan

### Unit tests (add to `console_reader_test.go`):

1. **ConsoleReader arrow key sequences:**
   - `TestConsoleReaderArrowUp`: bytes `{0x05, 0x1B, 0x5B, 0x41}` → onFocusUp called, 0 bytes forwarded
   - `TestConsoleReaderArrowDown`: bytes `{0x05, 0x1B, 0x5B, 0x42}` → onFocusDown called, 0 bytes forwarded
   - `TestConsoleReaderArrowInConsoleMode`: same but in console mode
   - `TestConsoleReaderCtrlE_c_ShellMode`: bytes `{0x05, 'c'}` → onCloseConsole called

2. **ConsoleReader command mode:**
   - `TestConsoleReaderCommandModeTyping`: set consoleMode, type "ping\n" → onCommand("ping") called
   - `TestConsoleReaderCommandModeBackspace`: type "pin\177g\n" → onCommand("pig") called
   - `TestConsoleReaderCommandModeCtrlE_Up`: in console mode, Ctrl-E+↑ → onFocusUp, exits console mode

3. **Console lifecycle:**
   - `TestConsoleInit`: creates Console, Init sets scroll region, Close resets
   - `TestConsoleShutdown`: Ctrl-E+c calls shutdown, running becomes false

### Integration test (manual):

```bash
# Terminal 1: server (Linux with PTY)
./gs-netcat -l -i -s test123

# Terminal 2: client with console
./gs-netcat -i -C -s test123

# Verify:
# 1. Status bar shows duration timer
# 2. Shell I/O works (type ls, see output)
# 3. Ctrl-E + ↓ enters command mode → "console> " prompt appears
# 4. Type "ping" + Enter → "ping sent..." then RTT appears
# 5. Type "pwd" + Enter → working directory appears
# 6. Ctrl-E + ↑ returns to shell focus
# 7. Ctrl-E + c closes console → scroll region resets, session continues
# 8. Ctrl-C exits cleanly
```

## Next Steps After Reboot

1. Ask user to confirm revert fixed the regression
2. Implement Bug 1 fix (arrow keys) with proper tests
3. Implement Bug 2 fix (shutdown reset scroll region)
4. Handle Bug 3 (-C without -i)
5. Full integration test
6. Final review + merge/push
