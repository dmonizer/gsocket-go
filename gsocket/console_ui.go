package gsocket

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// ANSI escape sequence fragments. Prepend CSI ("\x1b[") to build a
// complete control sequence, e.g. csi("2J") → ESC[2J (clear screen).
const (
	CSI = "\x1b["

	// Cursor movement.
	CursorUp    = "A"
	CursorDown  = "B"
	CursorRight = "C"
	CursorLeft  = "D"
	CursorNext  = "E" // Beginning of Nth next line
	CursorPrev  = "F" // Beginning of Nth previous line
	CursorCol   = "G" // Horizontal absolute
	CursorPos   = "H" // row;col (default 1;1 = home)

	// Erase.
	EraseDisplayBelow  = "J"  // ESC[J or ESC[0J — erase from cursor to end
	EraseDisplayAll    = "2J" // ESC[2J — erase entire display
	EraseLineToEnd     = "K"  // ESC[K or ESC[0K
	EraseLineFromStart = "1K" // ESC[1K
	EraseEntireLine    = "2K" // ESC[2K

	// Scrolling.
	ScrollUp        = "S"
	ScrollDown      = "T"
	SetScrollRegion = "r" // ESC[r resets; ESC[top;bot r sets region

	// Save/restore cursor (ANSI — used by drawStatusBar for transient saves).
	SaveCursor    = "s"
	RestoreCursor = "u"

	// Complete DEC sequences; these must not be prefixed with CSI.
	// ANSI and DEC cursor saves may share a single terminal save slot.
	DECSaveCursor    = "\x1b7"
	DECRestoreCursor = "\x1b8"

	// Text attributes (SGR).
	SGR = "m"

	// Device status.
	DeviceStatus = "n"

	// Modes.
	SetMode   = "h"
	ResetMode = "l"
)

// consoleFocus indicates which tier (shell or console) has keyboard focus.
type consoleFocus int

const (
	focusShell consoleFocus = iota
	focusConsole
)

// Console manages a split-screen terminal UI. The bottom portion of the
// screen is a console area (~15% of terminal height): a status bar showing
// transfer speeds / RTT / duration, and a "console>" command prompt.
//
// Initially hidden; Ctrl-E + c toggles visibility. Ctrl-E + ↓ enters
// command mode; Ctrl-E + ↑ returns focus to the shell.
//
// When visible (24-row terminal, 4-row console area):
//
//	┌──────────────────────────────────────┐
//	│  shell output (scrolls normally)      │  ← rows 1..20
//	│  $ ls -la                             │
//	├──────────────────────────────────────┤
//	│  [↑1.2KB/s ↓0.8KB/s] [rtt 45ms] ...  │  ← row 21: status bar
//	│                                      │  ← row 22-23: console output
//	│  console> ping█                       │  ← row 24: command line
//	└──────────────────────────────────────┘
type Console struct {
	reader  *ConsoleReader
	display io.Writer

	// Terminal dimensions.
	rows int
	cols int

	// Focus state.
	focus   consoleFocus
	visible bool // true when the console footer is shown

	// All display transactions lock mu, then stdoutMu. The line editor
	// only locks stdoutMu and never calls into Console while holding it.
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

	// Running state.
	running bool

	// Command dispatch callback (set by Peer).
	OnCommand func(string)
}

// csi builds a complete ANSI escape sequence from fragments.
// When called without format arguments, %-verbs pass through
// unchanged so the caller can use them in an outer fmt.Fprintf.
//
//	csi("2J")         → "\x1b[2J"       (completed immediately)
//	csi("%d;1H", 5)   → "\x1b[5;1H"     (completed immediately)
//	csi("1;%dr")      → "\x1b[1;%dr"    (pass-through for outer Fprintf)
func csi(format string, args ...any) string {
	if len(args) == 0 {
		return CSI + format
	}
	return fmt.Sprintf(CSI+format, args...)
}

// --- construction ---

// NewConsole creates a Console wrapping the given stdin reader (normally
// os.Stdin). The console is initially hidden — status bar and command
// line are not shown until the user activates them with Ctrl-E + c.
func NewConsole(stdin io.Reader) *Console {
	c := &Console{
		reader:    NewConsoleReader(stdin),
		display:   os.Stdout,
		stdoutMu:  &sync.Mutex{},
		focus:     focusShell,
		startTime: time.Now(),
		running:   true,
	}

	c.reader.onFocusDown = c.enterCommandMode
	c.reader.onFocusUp = c.exitCommandMode
	c.reader.onCloseConsole = c.toggleVisibility
	c.reader.onCommand = c.dispatchCommand
	c.reader.onCmdChanged = c.redrawCommandLine

	c.measureTerminal()
	return c
}

// Init clears the screen for a clean session start. Does NOT show the
// console — that is activated later by the user via Ctrl-E + c.
func (c *Console) Init() {
	c.lockDisplay()
	defer c.unlockDisplay()
	fmt.Fprint(c.display, csi(EraseDisplayAll)+csi(CursorPos))
}

// lockDisplay serializes a complete cursor movement / paint / restore
// transaction with shell output and local line editing.
func (c *Console) lockDisplay() {
	c.mu.Lock()
	c.stdoutMu.Lock()
}

func (c *Console) unlockDisplay() {
	c.stdoutMu.Unlock()
	c.mu.Unlock()
}

// --- I/O ---

// EnableLineEditing enables local line editing on the underlying
// ConsoleReader. Called when the server reports no PTY.
func (c *Console) EnableLineEditing(stdoutMu *sync.Mutex) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stdoutMu = stdoutMu
	ed := NewLineEditor(c.display, stdoutMu)
	c.reader.SetLineMode(true, ed)
	c.reader.onLineEntered = c.flushDeferredStatusBar
}

// Read delegates to the underlying ConsoleReader.
func (c *Console) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// Write restores the shell cursor before writing while the console owns
// focus, then saves the updated shell cursor and repaints the command line.
func (c *Console) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	c.lockDisplay()
	defer c.unlockDisplay()
	if c.visible && c.focus == focusConsole {
		fmt.Fprint(c.display, DECRestoreCursor)
	}
	n, err := c.display.Write(data)
	if c.visible && c.focus == focusConsole {
		fmt.Fprint(c.display, DECSaveCursor)
		c.renderStatusBar()
		c.renderCommandLine()
	}
	return n, err
}

// Close resets the terminal to normal operation. Idempotent.
func (c *Console) Close() {
	c.lockDisplay()
	defer c.unlockDisplay()
	if !c.running {
		return
	}
	c.running = false
	if c.visible {
		c.hide()
	}
}

// Running returns false after the console has been closed.
func (c *Console) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// --- status bar updates ---

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

// SetComment sets a transient comment in the status bar.
func (c *Console) SetComment(s string) {
	c.mu.Lock()
	c.comment = s
	c.mu.Unlock()
	c.drawStatusBar()
}

// HandleWinch updates the terminal dimensions and re-applies the
// scroll region if the console is visible.
func (c *Console) HandleWinch(rows, cols int) {
	if rows < 3 || cols < 1 {
		return
	}
	c.lockDisplay()
	defer c.unlockDisplay()
	if c.visible {
		if c.focus == focusConsole {
			fmt.Fprint(c.display, DECRestoreCursor)
		}
		fmt.Fprint(c.display, DECSaveCursor)
		// Clear the previous footer too, including rows that become shell
		// space when the terminal grows.
		c.clearFooter(min(c.rows, rows))
	}
	c.rows, c.cols = rows, cols
	if !c.running || !c.visible {
		return
	}
	// Margin changes home the cursor. Restore it before making room
	// within the new physical screen dimensions.
	fmt.Fprint(c.display, csi(SetScrollRegion), DECRestoreCursor)
	c.reserveFooter()
	fmt.Fprint(c.display, DECSaveCursor)
	c.splitScreen()
	c.renderStatusBar()
	if c.focus == focusConsole {
		c.renderCommandLine()
	} else {
		fmt.Fprint(c.display, DECRestoreCursor)
	}
}

// --- dynamic console sizing ---

// consoleHeight returns the number of rows to reserve at the bottom of
// the screen. It scales with terminal size (~15%) but enforces sensible
// minimums and maximums.
func (c *Console) consoleHeight() int {
	h := max(min(c.rows/6, 10), 3) // clamp to [3, 10]
	if h >= c.rows {
		return c.rows - 1
	}
	return h
}

// statusRow returns the 1-based row of the status bar within the
// console area (first row of the reserved region).
func (c *Console) statusRow() int {
	return c.rows - c.consoleHeight() + 1
}

// lastShellRow returns the last row of the scrolling shell region.
func (c *Console) lastShellRow() int {
	return c.rows - c.consoleHeight()
}

// --- screen split / unsplit (single responsibility) ---

// splitScreen changes margins without replacing the saved shell cursor.
// The caller owns the display lock and has already saved that cursor.
func (c *Console) splitScreen() {
	fmt.Fprintf(c.display, csi("1;%d"+SetScrollRegion), c.lastShellRow())
	c.clearFooter(c.rows)
}

func (c *Console) clearFooter(lastRow int) {
	for row := c.statusRow(); row <= lastRow; row++ {
		fmt.Fprintf(c.display, csi("%d;1"+CursorPos)+csi(EraseEntireLine), row)
	}
}

// unsplitScreen preserves the shell position across the margin reset,
// which itself moves the terminal cursor home.
func (c *Console) unsplitScreen() {
	c.clearFooter(c.rows)
	fmt.Fprint(c.display, csi(SetScrollRegion), DECRestoreCursor)
}

// reserveFooter advances by the footer height, scrolling only if needed,
// then moves back up by that height. This keeps the shell cursor aligned
// with its text, preserves the column, and leaves room below it. IND (ESC D)
// preserves the column even when newline mode is enabled.
func (c *Console) reserveFooter() {
	h := c.consoleHeight()
	fmt.Fprint(c.display, strings.Repeat("\x1bD", h), csi("%d"+CursorUp, h))
}

func (c *Console) show() {
	c.reserveFooter()
	fmt.Fprint(c.display, DECSaveCursor)
	c.visible = true
	c.focus = focusConsole
	c.splitScreen()
	c.renderStatusBar()
	c.renderCommandLine()
	c.reader.SetConsoleMode(true)
}

func (c *Console) hide() {
	if c.focus == focusShell {
		fmt.Fprint(c.display, DECSaveCursor)
	}
	c.unsplitScreen()
	c.visible = false
	c.statusDirty = false
	c.reader.SetConsoleMode(false)
	c.focus = focusShell
}

func (c *Console) toggleVisibility() {
	c.lockDisplay()
	defer c.unlockDisplay()
	if !c.running {
		return
	}
	if c.visible {
		c.hide()
	} else {
		c.show()
	}
}

// --- focus switching ---

func (c *Console) enterCommandMode() {
	c.lockDisplay()
	defer c.unlockDisplay()
	if !c.running || c.focus == focusConsole {
		return
	}
	if !c.visible {
		c.show()
		return
	}
	fmt.Fprint(c.display, DECSaveCursor)
	c.focus = focusConsole
	c.reader.SetConsoleMode(true)
	c.renderCommandLine()
}

func (c *Console) exitCommandMode() {
	c.lockDisplay()
	defer c.unlockDisplay()
	if !c.running || c.focus != focusConsole {
		return
	}
	c.focus = focusShell
	c.reader.SetConsoleMode(false)
	fmt.Fprintf(c.display, csi("%d;1"+CursorPos)+csi(EraseEntireLine), c.rows)
	fmt.Fprint(c.display, DECRestoreCursor)
}

// --- command dispatch ---

func (c *Console) dispatchCommand(cmd string) {
	if c.OnCommand != nil {
		c.OnCommand(cmd)
	}
}

// --- terminal measurement ---

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

// --- status bar rendering ---

// drawStatusBar is the public entry point for status bar updates. It
// saves and restores the cursor so the caller's position is undisturbed.
func (c *Console) drawStatusBar() {
	c.lockDisplay()
	defer c.unlockDisplay()
	if !c.running || !c.visible {
		return
	}
	if c.reader.lineMode && c.focus == focusShell {
		c.statusDirty = true
		return
	}
	c.paintStatusBar()
}

// While console focus is active, the save slot belongs to the shell.
// Repaint the command prompt directly instead of overwriting that slot.
func (c *Console) paintStatusBar() {
	if c.focus == focusShell {
		fmt.Fprint(c.display, DECSaveCursor)
	}
	c.renderStatusBar()
	if c.focus == focusConsole {
		c.renderCommandLine()
	} else {
		fmt.Fprint(c.display, DECRestoreCursor)
	}
}

func (c *Console) flushDeferredStatusBar() {
	c.lockDisplay()
	defer c.unlockDisplay()
	if !c.statusDirty {
		return
	}
	c.statusDirty = false
	if c.running && c.visible {
		c.paintStatusBar()
	}
}

// renderStatusBar paints the status line at the top of the console
// area. It does NOT save/restore the cursor — callers are responsible
// for positioning. After the call the cursor is at the status row.
func (c *Console) renderStatusBar() {
	sr := c.statusRow()
	if sr < 1 {
		return
	}
	parts := c.buildStatusLine()
	fmt.Fprintf(c.display, csi("%d;1"+CursorPos)+csi(EraseEntireLine)+"%s", sr, parts)
}

func (c *Console) buildStatusLine() string {
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
	if len(parts) > c.cols {
		parts = parts[:c.cols]
	}
	return parts
}

// --- command line rendering ---

func (c *Console) redrawCommandLine() {
	c.lockDisplay()
	defer c.unlockDisplay()
	if c.running && c.visible && c.focus == focusConsole {
		c.renderCommandLine()
	}
}

func (c *Console) renderCommandLine() {
	if c.rows < 1 {
		return
	}
	prompt := "console> " + string(c.reader.CmdBuf())
	if len(prompt) > c.cols {
		prompt = prompt[:c.cols]
	}
	fmt.Fprintf(c.display, csi("%d;1"+CursorPos)+csi(EraseEntireLine)+"%s", c.rows, prompt)
}

// formatSize formats a byte count for human display.
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
