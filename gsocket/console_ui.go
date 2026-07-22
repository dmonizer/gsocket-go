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
// The console is initially hidden; Ctrl-E + c toggles visibility.
//
// When visible:
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
	focus   consoleFocus
	visible bool // true when the console footer is shown

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

	// Running state.
	running bool

	// Command dispatch callback (set by Peer).
	OnCommand func(string)
}

// NewConsole creates a Console wrapping the given stdin reader (normally
// os.Stdin). The console is initially hidden — status bar and command
// line are not shown until the user activates them with Ctrl-E + c.
func NewConsole(stdin io.Reader) *Console {
	c := &Console{
		reader:    NewConsoleReader(stdin),
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
// console footer — that is activated later by the user via Ctrl-E + c.
func (c *Console) Init() {
	if c.stdoutMu != nil {
		c.stdoutMu.Lock()
		defer c.stdoutMu.Unlock()
	}
	fmt.Fprint(os.Stdout, "\033[2J\033[H")
}

// EnableLineEditing enables local line editing on the underlying
// ConsoleReader. Called when the server reports no PTY.
func (c *Console) EnableLineEditing(stdoutMu *sync.Mutex) {
	c.stdoutMu = stdoutMu
	ed := NewLineEditor(os.Stdout, stdoutMu)
	c.reader.SetLineMode(true, ed)
	c.reader.onLineEntered = c.flushDeferredStatusBar
}

// Read delegates to the underlying ConsoleReader.
func (c *Console) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// Write writes shell output to the terminal. When the console is
// visible, the scroll region confines scrolling automatically.
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
	if c.visible {
		c.visible = false
		fmt.Fprint(os.Stdout, "\033[r")
		fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows-1)
		fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows)
	}
}

// Running returns false after the console has been closed.
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

// SetComment sets a transient comment in the status bar.
func (c *Console) SetComment(s string) {
	c.mu.Lock()
	c.comment = s
	c.mu.Unlock()
	c.drawStatusBar()
}

// HandleWinch updates the terminal dimensions and adjusts the scroll region.
func (c *Console) HandleWinch(rows, cols int) {
	c.mu.Lock()
	c.rows = rows
	c.cols = cols
	visible := c.visible
	conMode := c.focus == focusConsole
	c.mu.Unlock()
	if visible && c.rows > 2 {
		fmt.Fprintf(os.Stdout, "\033[1;%dr", c.rows-2)
	}
	c.drawStatusBar()
	if conMode {
		c.redrawCommandLine()
	}
}

// --- visibility toggling ---

// show sets the scroll region and draws the status bar.
func (c *Console) show() {
	c.visible = true
	if c.rows > 2 {
		fmt.Fprintf(os.Stdout, "\033[1;%dr", c.rows-2)
	}
	c.renderStatusBar()
}

// hide clears the footer and resets the scroll region.
func (c *Console) hide() {
	c.visible = false
	c.reader.SetConsoleMode(false)
	c.focus = focusShell
	fmt.Fprint(os.Stdout, "\033[r")
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows-1)
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows)
	if c.rows > 2 {
		fmt.Fprintf(os.Stdout, "\033[%d;1H", c.rows-2)
	}
}

// toggleVisibility toggles the console footer on/off (Ctrl-E + c).
func (c *Console) toggleVisibility() {
	c.mu.Lock()
	defer c.mu.Unlock()
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

// enterCommandMode switches focus to the console command line.
// If the console is hidden, shows it first.
func (c *Console) enterCommandMode() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	if !c.visible {
		c.show()
	}
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
	if !c.running || c.focus != focusConsole {
		c.mu.Unlock()
		return
	}
	c.focus = focusShell
	c.mu.Unlock()
	c.reader.SetConsoleMode(false)
	if c.stdoutMu != nil {
		c.stdoutMu.Lock()
		defer c.stdoutMu.Unlock()
	}
	// Clear command line, leave cursor in shell area.
	if c.rows > 0 {
		fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K", c.rows)
	}
	if c.rows > 2 {
		fmt.Fprintf(os.Stdout, "\033[%d;1H", c.rows-2)
	}
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

func (c *Console) drawStatusBar() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.visible {
		return
	}
	// Defer while line-editing in shell mode.
	if c.reader.lineMode && c.focus == focusShell {
		c.statusDirty = true
		return
	}
	c.renderStatusBar()
}

func (c *Console) flushDeferredStatusBar() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusDirty {
		c.statusDirty = false
		c.renderStatusBar()
	}
}

func (c *Console) renderStatusBar() {
	if c.rows < 2 {
		return
	}
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
	fmt.Fprintf(os.Stdout, "\033[s\033[%d;1H\033[K%s\033[u", c.rows-1, parts)
	if c.focus == focusConsole {
		c.renderCommandLine()
	}
}

// --- command line rendering ---

func (c *Console) redrawCommandLine() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.focus == focusConsole {
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
	fmt.Fprintf(os.Stdout, "\033[%d;1H\033[K%s", c.rows, prompt)
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
