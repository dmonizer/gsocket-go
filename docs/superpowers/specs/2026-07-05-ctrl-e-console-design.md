# Ctrl-E Console Design

> **Date:** 2026-07-05
> **Branch:** `feature/dating-app`
> **Status:** Design approved

## Overview

Implement the Ctrl-E console (split-screen terminal with status bar and command input) matching the C `gs-netcat` implementation. Gated behind the `-C` flag. File transfer commands deferred to a future change.

## Architecture

### Approach: Scroll-region + cursor save/restore

Use ANSI escape sequences to carve a fixed 2-line footer out of the terminal, leaving the remaining rows for shell I/O. This matches the C implementation's approach — no external TUI library, no alternate screen buffer.

```
┌──────────────────────────────────────┐
│  shell output (scrolls normally)      │  ← rows 0..N-3, scroll region
│  ...                                  │
│  $ ls -la                             │
│  total 42                             │
├──────────────────────────────────────┤
│  [load 0.15] [↑1.2KB/s ↓0.8KB/s]    │  ← row N-2: status bar
│  console> ping█                       │  ← row N-1: command line (focused)
└──────────────────────────────────────┘
```

**Scroll region:** `\033[0;N-3r` — all scrolling (shell output) confined to rows 0..N-3. Rows N-2 and N-1 are outside the scroll region and never disturbed by shell output.

**Focus states:**
- `focusShell` (default): keystrokes go to shell. Ctrl-E enters escape mode.
- `focusConsole`: keystrokes go to command buffer. Enter executes. Ctrl-E + ↑ or Ctrl-E + c exits back to shell.

## Components

### 1. `gsocket/console.go` — Enhanced ConsoleReader

Adds command-mode awareness to the existing escape-sequence filter:

**New fields:**
- `consoleMode bool` — true when focus is in the console command line
- `cmdBuf []byte` — accumulated command text
- `onCommand func(string)` — called when Enter pressed in console mode
- `onFocusUp func()` — Ctrl-E + ↑
- `onFocusDown func()` — Ctrl-E + ↓
- `onCloseConsole func()` — Ctrl-E + c

**State machine (per byte read from stdin):**

```
if consoleMode:
    Ctrl-E + ↑        → onFocusUp(), exit console mode, no byte forwarded
    Ctrl-E + c        → onCloseConsole(), exit console mode
    Ctrl-E + Ctrl-E   → append literal Ctrl-E (0x05) to cmdBuf
    Ctrl-E + [        → read full arrow sequence, discard
    Enter (0x0D)      → onCommand(string(cmdBuf)), reset cmdBuf
    BS (0x7F)         → delete last char from cmdBuf
    printable         → append to cmdBuf

if not consoleMode:
    Ctrl-E + ↓        → onFocusDown(), enter console mode
    Ctrl-E + E/e/Ctrl-E → forward literal 0x05
    Ctrl-E + ↑        → onFocusUp() (NOP without console, but keep hook)
    Ctrl-E + [        → read arrow sequence, discard
    Ctrl-E + other    → forward character directly (existing behavior)
    plain bytes       → forwarded to shell (existing behavior)
```

### 2. `gsocket/console_ui.go` — Terminal Rendering (NEW)

**`Console` struct:**

```go
type Console struct {
    rows, cols    int          // terminal dimensions
    focus         consoleFocus // focusShell or focusConsole
    reader        *ConsoleReader
    startTime     time.Time    // when console was initialized

    // Status bar values (updated async).
    mu            sync.Mutex
    pingRTT       time.Duration
    bpsUp, bpsDown int64
    comment       string       // transient message ("pwd: /home/user")
    
    // BPS averaging (exponential moving average matching C).
    byteReadTotal, byteWrittenTotal int64
    lastBPSUpdate time.Time
    
    running       bool
}
```

**Key methods:**

| Method | Purpose |
|--------|---------|
| `NewConsole() *Console` | Create, measure terminal, draw initial status bar |
| `Init()` | Set scroll region, draw status bar |
| `HandleStdinByte(byte) (forward byte, ok bool)` | Route byte through ConsoleReader; returns bytes to forward to channel |
| `Write(data []byte)` | Write shell output to upper region using save/restore cursor |
| `computeBPS(int64, int64)` | Exponential moving average (~0.125 weight to new sample) |
| `drawStatusBar()` | Save cursor → goto row N-2 → clear line → draw bar → restore cursor |
| `drawCommandLine()` | Goto row N-1 → clear line → draw "console> " + cmdBuf + cursor |
| `executeCommand(string)` | Dispatch `ping`/`pwd`/`log`/`ids` via registered handlers |
| `Close()` | Reset scroll region `\033[0;r`, clear footer lines |
| `handleWinch(int, int)` | Recalculate scroll region on terminal resize |

**Status bar format (single line, matches C):**

```
[↑1.2KB/s ↓0.8KB/s] [rtt 45ms] [load 0.15] [2 peers] [00:03:42]
```

Components present only when data available. BPS hidden when zero. Load comes from PONG messages or `/proc/loadavg`.

**ANSI escape sequences used:**

| Sequence | Meaning |
|----------|---------|
| `\033[s` | Save cursor position |
| `\033[u` | Restore cursor position |
| `\033[N;Mr` | Set scroll region rows N to M |
| `\033[K` | Clear to end of line |
| `\033[?25l` / `\033[?25h` | Hide / show cursor |
| `\033[%d;%dH` | Move cursor to row, col |

### 3. Changes to `peer.go`

**New option:**
```go
func WithConsole() PeerOption  // enables console UI
```

**Changes to `runClientInteractive()`:**
- When console enabled: create `Console`, use its reader (not bare `ConsoleReader`) for stdin
- Wire PONG callback → `console.pingRTT = rtt` + `console.computeBPS()`
- Track BPS: update byte counters in read/write paths, compute rate every second
- Stdin reads go through `Console.HandleStdinByte()` which returns either forwarded bytes or dispatches commands
- Stdout writes go through `Console.Write()` which handles scroll region

**New field on Peer:**
```go
console *Console  // nil when -C not used
```

**New method on Peer:**
```go
func (p *Peer) BPSStats() (up, down int64)
```

### 4. Changes to `cmd/gs-netcat/main.go`

- Add `-C` flag: `consoleUI = flag.Bool("C", false, "Enable console status bar and commands")`
- Pass `gsocket.WithConsole()` when true

## Commands

| Command | Implementation |
|---------|---------------|
| `ping` | Send `msgPing` via `app.SendMessage`. RTT updated in PONG callback → status bar refreshes. |
| `pwd` | Send `msgPWDReq`. Server-side handler (new, `wireAppCallbacks`) replies with `chnPWD` containing the path string. Client receives → status bar comment: `"pwd: /home/user"`. |
| `log` | Toggle: server log messages appear in status area instead of stdout. Second `log` command disables. |
| `ids` | Stub — sets status bar comment: `"IDS: not implemented"`. |
| `clear` | Clears the comment area on the status bar. |

## Files

| File | Action |
|------|--------|
| `gsocket/console.go` | **Modify** — add command mode to ConsoleReader |
| `gsocket/console_ui.go` | **New** — Console struct, terminal rendering |
| `gsocket/peer.go` | **Modify** — WithConsole option, BPS tracking, PWD handler |
| `gsocket/appproto.go` | **No changes** — msgPWDReq, chnPWD already defined |
| `cmd/gs-netcat/main.go` | **Modify** — add -C flag |

## Testing

- `console.go` already has no dedicated tests (it's tested indirectly via integration)
- `console_ui.go` will be tested via manual integration testing (requires real terminal)
- ANSI escape sequence correctness verified by visual inspection
- BPS calculation unit test via `gsocket/console_ui_test.go`
