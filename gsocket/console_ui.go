package gsocket

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

// consoleFocus indicates which tier (shell or console) has keyboard focus.
type consoleFocus int

const (
	focusShell   consoleFocus = iota
	focusConsole
)

// Console manages a split-screen terminal UI with a fixed 2-line footer.
//
// The terminal is divided by setting a scroll region on the upper N-2 rows.
// Shell output scrolls normally within that region. The bottom 2 rows
// display a status bar and an optional command line.
//
//	   ┌──────────────────────────────────────┐
//	   │  shell output (scrolls normally)      │  ← rows 0..N-3
//	   │  $ ls -la                             │
//	   ├──────────────────────────────────────┤
//	   │  [↑1.2KB/s ↓0.8KB/s] [rtt 45ms] ...  │  ← row N-2: status bar
//	   │  console> ping█                       │  ← row N-1: command line
//	   └──────────────────────────────────────┘
type Console struct {
	reader *ConsoleReader

	// Terminal dimensions.
	rows int
	cols int

	// Focus state.
	focus consoleFocus

	// Serialises stdout writes with the line editor (set by EnableLineEditing).
	stdoutMu *sync.Mutex

	// statusDirty is set when the status bar needs redrawing but
	// line editing is active (deferred until the user presses Enter).
	statusDirty bool

	// Status bar values.
	mu        sync.Mutex
	pingRTT   time.Duration
	bpsUp     int64
	bpsDown   int64
	comment   string
	startTime time.Time

	// Running state — set to false by Ctrl-E + c.
	running bool

	// Command dispatch callback (set by Peer).
	OnCommand func(string)
}

// NewConsole creates a Console wrapping the given stdin reader (normally
// os.Stdin). It measures the terminal, creates the underlying
// ConsoleReader, and wires all callbacks.
func NewConsole(stdin io.Reader) *Console {
	c := &Console{
		reader:    NewConsoleReader(stdin),
		focus:     focusShell,
		startTime: time.Now(),
		running:   true,
	}

	// Wire ConsoleReader callbacks to Console methods.
	c.reader.onFocusDown = c.enterCommandMode
	c.reader.onFocusUp = c.exitCommandMode
	c.reader.onCloseConsole = c.toggleCommandMode
	c.reader.onCommand = c.dispatchCommand
	c.reader.onCmdChanged = c.redrawCommandLine

	// Measure terminal now (may be updated later via HandleWinch).
	c.measureTerminal()

	return c
}

// Init sets up the terminal for split-screen operation.
// Must be called after term.MakeRaw (raw mode must be active).
func (c *Console) Init() {
	// Clear screen and home cursor so the session starts clean.
	fmt.Fprint(os.Stdout, "\033[2J\033[H")
	// Set scroll region: rows 0 to rows-2 (0-indexed), leaving
	// rows rows-1 and rows for the footer. ANSI scroll regions
	// are 1-indexed, so: \033[1;rows-2r.
	if c.rows > 2 {
		fmt.Fprintf(os.Stdout, "\033[1;%dr", c.rows-2)
	}
	c.drawStatusBar()
}

// EnableLineEditing enables local line editing on the underlying
// ConsoleReader. Called when the server reports no PTY.
// stdoutMu serialises display writes with channel→stdout output.
func (c *Console) EnableLineEditing(stdoutMu *sync.Mutex) {
	c.stdoutMu = stdoutMu
	ed := NewLineEditor(os.Stdout, stdoutMu)
	c.reader.SetLineMode(true, ed)
	// Defer status bar updates while line editing to avoid
	// \033[s/\033[u interference with the line editor.
	c.reader.onLineEntered = c.flushDeferredStatusBar
}

// Read reads from stdin, applying console escape filtering and command
// accumulation. Returns only bytes to forward to the encrypted channel.
// In shell mode, returns filtered input. In console mode, returns 0 bytes
// (keystrokes go to the command buffer).
func (c *Console) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// Write writes shell output to the terminal. The scroll region
// (set by Init) confines scrolling to the upper area automatically.
func (c *Console) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if c.stdoutMu != nil {
		c.stdoutMu.Lock()
		defer c.stdoutMu.Unlock()
	}
	return os.Stdout.Write(data)
}

// Close resets the terminal to normal operation. Idempotent.
func (c *Console) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return
	}
	c.running = false
	fmt.Fprint(os.Stdout, "\033[r")
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows-1)
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows)
}

// Running returns false after the console has been closed via Ctrl-E + c.
func (c *Console) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// SetPingRTT updates the ping RTT shown in the status bar.
func (c *Console) SetPingRTT(rtt time.Duration) {
	c.mu.Lock()
	c.pingRTT = rtt
	c.mu.Unlock()
	c.drawStatusBar()
}

// SetBPS updates the bytes-per-second values shown in the status bar.
func (c *Console) SetBPS(up, down int64) {
	c.mu.Lock()
	c.bpsUp = up
	c.bpsDown = down
	c.mu.Unlock()
	c.drawStatusBar()
}

// SetComment sets a transient comment in the status bar (e.g. "pwd: /home/user").
// The comment is cleared on the next command or explicit clear.
func (c *Console) SetComment(s string) {
	c.mu.Lock()
	c.comment = s
	c.mu.Unlock()
	c.drawStatusBar()
}

// HandleWinch updates the terminal dimensions and adjusts the scroll region.
// Called from the SIGWINCH handler.
func (c *Console) HandleWinch(rows, cols int) {
	c.mu.Lock()
	c.rows = rows
	c.cols = cols
	focusConsoleMode := c.focus == focusConsole
	c.mu.Unlock()
	if c.rows > 2 {
		fmt.Fprintf(os.Stdout, "\033[1;%dr", c.rows-2)
	}
	c.drawStatusBar()
	if focusConsoleMode {
		c.redrawCommandLine()
	}
}

// --- private methods ---

// toggleCommandMode toggles between shell focus and console
// command-line focus (Ctrl-E + c). Does nothing if shut down.
func (c *Console) toggleCommandMode() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	isShell := c.focus == focusShell
	c.mu.Unlock()
	if isShell {
		c.enterCommandMode()
	} else {
		c.exitCommandMode()
	}
}

// enterCommandMode switches focus to the console command line.
func (c *Console) enterCommandMode() {
	c.mu.Lock()
	c.focus = focusConsole
	c.mu.Unlock()
	c.reader.SetConsoleMode(true)
	if c.stdoutMu != nil {
		c.stdoutMu.Lock()
		defer c.stdoutMu.Unlock()
	}
	c.redrawCommandLine()
}

// exitCommandMode switches focus back to the shell.
func (c *Console) exitCommandMode() {
	c.mu.Lock()
	c.focus = focusShell
	c.mu.Unlock()
	c.reader.SetConsoleMode(false)
	if c.stdoutMu != nil {
		c.stdoutMu.Lock()
		defer c.stdoutMu.Unlock()
	}
	// Clear command line, then move cursor to the bottom of
	// the shell area (just above the status bar).
	if c.rows > 0 {
		fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows)
	}
	if c.rows > 2 {
		// Position at end of shell output — new output scrolls
		// naturally from here.
		fmt.Fprintf(os.Stdout, "\033[%d;1H", c.rows-2)
	}
}

// dispatchCommand is called when Enter is pressed in console mode.
func (c *Console) dispatchCommand(cmd string) {
	if c.OnCommand != nil {
		c.OnCommand(cmd)
	}
}

// measureTerminal reads the current terminal dimensions.
// Uses golang.org/x/term.GetSize which works cross-platform.
func (c *Console) measureTerminal() {
	fd := int(os.Stdin.Fd())
	w, h, err := term.GetSize(fd)
	if err != nil {
		c.rows = 24
		c.cols = 80
		return
	}
	c.rows = h
	c.cols = w
}

// drawStatusBar renders the status bar at row rows-1.
// If line editing is active, the redraw is deferred until the
// user presses Enter, avoiding \033[s/\033[u interference.
func (c *Console) drawStatusBar() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Defer status bar updates while line-editing in shell mode
	// (avoids \033[s/\033[u interference). In console mode the
	// command feedback must appear immediately.
	if c.reader.lineMode && c.focus == focusShell {
		c.statusDirty = true
		return
	}
	c.renderStatusBar()
}

// flushDeferredStatusBar draws the status bar if it was deferred.
func (c *Console) flushDeferredStatusBar() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusDirty {
		c.statusDirty = false
		c.renderStatusBar()
	}
}

// renderStatusBar builds and writes the status line and command line.
// Must be called with c.mu held.
func (c *Console) renderStatusBar() {
	if c.rows < 2 {
		return
	}

	// Build status line parts.
	var parts string
	if c.bpsUp > 0 || c.bpsDown > 0 {
		parts += fmt.Sprintf("[↑%s/s ↓%s/s] ", formatSize(c.bpsUp), formatSize(c.bpsDown))
	}
	if c.pingRTT > 0 {
		parts += fmt.Sprintf("[rtt %v] ", c.pingRTT.Round(time.Millisecond))
	}
	dur := time.Since(c.startTime).Truncate(time.Second)
	parts += fmt.Sprintf("[%v]", dur)

	if c.comment != "" {
		parts += " " + c.comment
	}

	// Truncate to terminal width.
	if len(parts) > c.cols {
		parts = parts[:c.cols]
	}

	// Status bar at row rows-1 (1-indexed).
	fmt.Fprintf(os.Stdout, "\033[s\033[%d;1H\033[K%s\033[u", c.rows-1, parts)

	// Command line at row rows (if in console mode).
	if c.focus == focusConsole {
		c.renderCommandLine()
	}
}

// redrawCommandLine is called by ConsoleReader when cmdBuf changes.
// It acquires the lock to safely read terminal dimensions.
func (c *Console) redrawCommandLine() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.focus == focusConsole {
		c.renderCommandLine()
	}
}

// renderCommandLine draws the command prompt at the bottom row.
// Must be called with c.mu held. Leaves cursor at the prompt.
func (c *Console) renderCommandLine() {
	if c.rows < 1 {
		return
	}
	prompt := "console> " + string(c.reader.CmdBuf())
	if len(prompt) > c.cols {
		prompt = prompt[:c.cols]
	}
	// Position at bottom row, clear, write prompt, leave cursor there.
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K%s", c.rows, prompt)
}

// formatSize formats a byte count for human display.
// Matches C's display logic in console.c.
func formatSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
