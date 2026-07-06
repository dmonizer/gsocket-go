package gsocket

import (
	"io"
	"sync"
)

// ConsoleReader wraps an io.Reader (normally os.Stdin) and filters
// Ctrl-E (0x05) escape sequences according to the gsocket console
// protocol. This matches the C implementation's CONSOLE_check_esc()
// state machine.
//
// When lineMode is enabled (via SetLineMode), the reader provides
// local line editing via LineEditor: characters are buffered, arrow
// keys move the cursor, backspace deletes, and only Enter sends the
// completed line. This is used when the server has no PTY.
//
// Escape sequences (shell mode):
//
//	Ctrl-E + E       → emit literal 0x05 byte
//	Ctrl-E + e       → emit literal 0x05 byte
//	Ctrl-E + Ctrl-E  → emit literal 0x05 byte
//	Ctrl-E + ↑       → call onFocusUp
//	Ctrl-E + ↓       → call onFocusDown
//	Ctrl-E + c       → call onCloseConsole (console mode only)
//	Ctrl-E + X       → emit X directly (non-screen behavior, matches C)
//
// In console mode, printable characters accumulate in cmdBuf instead of
// being forwarded. Enter dispatches the command via onCommand and clears
// the buffer. Backspace (0x7F) deletes the last character.
type ConsoleReader struct {
	src     io.Reader
	mu      sync.Mutex // guards cmdBuf
	pending bool       // true when last char was 0x05 and we're in escape mode

	// Console mode state (set by Console UI via SetConsoleMode).
	consoleMode bool
	cmdBuf      []byte

	// Callbacks for Ctrl-E events. Set by Console UI.
	onFocusUp      func()
	onFocusDown    func()
	onCloseConsole func()
	onCommand      func(string) // Enter pressed in console mode
	onCmdChanged   func()       // cmdBuf contents changed (for redraw)

	// Local line editing (enabled when server has no PTY).
	lineMode      bool
	editor        *LineEditor
	onLineEntered func() // called after Enter flushes line (Console defers status bar)
}

// NewConsoleReader creates a new ConsoleReader wrapping the given source.
func NewConsoleReader(src io.Reader) *ConsoleReader {
	return &ConsoleReader{src: src}
}

// SetConsoleMode toggles command accumulation mode.
func (cr *ConsoleReader) SetConsoleMode(on bool) {
	cr.consoleMode = on
}

// SetLineMode enables local line editing. Characters are buffered
// locally; arrow keys, backspace, and delete work on the buffer.
// Only Enter sends a completed line to output. stdoutMu serialises
// display writes with the channel→stdout path.
func (cr *ConsoleReader) SetLineMode(on bool, editor *LineEditor) {
	cr.lineMode = on
	cr.editor = editor
}

// CmdBuf returns a copy of the current command buffer contents.
func (cr *ConsoleReader) CmdBuf() []byte {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return append([]byte{}, cr.cmdBuf...)
}

// Lock locks the ConsoleReader mutex for atomic access to cmdBuf.
func (cr *ConsoleReader) Lock() {
	cr.mu.Lock()
}

// Unlock unlocks the ConsoleReader mutex.
func (cr *ConsoleReader) Unlock() {
	cr.mu.Unlock()
}

// Read reads data from the underlying reader and filters Ctrl-E escape
// sequences. In shell mode, the returned slice contains only data to
// forward to the channel. In console mode, bytes accumulate in the
// command buffer and the returned count is typically 0.
//
// It reads a single chunk from the source (blocking only until data is
// available) and processes it through the state machine. We do NOT loop
// to fill p — that would block indefinitely on stdin in raw mode.
func (cr *ConsoleReader) Read(p []byte) (n int, err error) {
	// When line editing is active, process locally.
	if cr.lineMode && !cr.consoleMode {
		return cr.readWithLineEdit(p)
	}

	// Read whatever is available from the source in one shot.
	nr, err := cr.src.Read(p)
	if nr == 0 {
		return 0, err
	}

	// Process in-place: src spans the bytes we just read, dst tracks
	// the write position. Since we only remove bytes (never add), the
	// write position never overtakes the read position.
	src := p[:nr]
	dst := 0
	for i := 0; i < len(src); i++ {
		c := src[i]

		if cr.pending {
			cr.pending = false
			if cr.consoleMode {
				cr.handleConsoleEscape(c)
			} else {
				if c == 'c' && cr.onCloseConsole != nil {
					cr.onCloseConsole()
					continue
				}
				dst += cr.forwardEscape(c, p[dst:])
			}
			continue
		}

		if c == GS_CONSOLE_ESC {
			cr.pending = true
			continue
		}

		if cr.consoleMode {
			cr.handleConsoleChar(c)
			continue
		}

		p[dst] = c
		dst++
	}
	return dst, err
}

// readWithLineEdit processes input with local line editing.
func (cr *ConsoleReader) readWithLineEdit(p []byte) (n int, err error) {
	nr, err := cr.src.Read(p)
	if nr == 0 {
		return 0, err
	}

	src := p[:nr]
	dst := 0
	ed := cr.editor

	for i := 0; i < len(src); i++ {
		c := src[i]

		// Ctrl-E escape handling.
		if cr.pending {
			cr.pending = false
			if c == GS_CONSOLE_ESC || c == 'E' || c == 'e' {
				ed.Insert(GS_CONSOLE_ESC)
				ed.Redraw()
				continue
			}
			ed.Insert(c)
			ed.Redraw()
			continue
		}

		if c == GS_CONSOLE_ESC {
			cr.pending = true
			continue
		}

		// Ctrl-C / Ctrl-D — forward immediately.
		if c == 0x03 || c == 0x04 {
			p[dst] = c
			dst++
			continue
		}

		// Enter — flush the line, send \n.
		if c == 0x0D {
			ed.Newline()
			line := ed.Flush()
			dst += copy(p[dst:], line)
			p[dst] = '\n'
			dst++
			if cr.onLineEntered != nil {
				cr.onLineEntered()
			}
			continue
		}

		// Backspace (DEL or BS).
		if c == 0x7F || c == 0x08 {
			ed.Backspace()
			ed.Redraw()
			continue
		}

		// ESC — arrow key sequence.
		if c == 0x1B && i+1 < len(src) {
			if src[i+1] == '[' {
				i++ // consume '['
				if i+1 < len(src) {
					cr.handleLineArrowKey(src[i+1])
					i++
					continue
				}
				continue
			}
			continue
		}

		// Tab — insert spaces.
		if c == '\t' {
			ed.Insert(' ')
			ed.Insert(' ')
			ed.Redraw()
			continue
		}

		// Printable characters.
		if c >= 0x20 && c < 0x7F {
			ed.Insert(c)
			ed.Redraw()
		}
	}

	return dst, err
}

func (cr *ConsoleReader) handleLineArrowKey(dir byte) {
	ed := cr.editor
	switch dir {
	case 'D': // Left
		ed.MoveLeft()
	case 'C': // Right
		ed.MoveRight()
	case 'H': // Home
		ed.Home()
	case 'F': // End
		ed.End()
	case '3': // Delete (~ follows)
		ed.Delete()
		ed.Redraw()
		return
	default:
		return
	}
	ed.RepositionCursor()
}

// handleConsoleEscape processes Ctrl-E sequences when in console mode.
func (cr *ConsoleReader) handleConsoleEscape(c byte) {
	switch c {
	case GS_CONSOLE_ESC, 'E', 'e':
		cr.mu.Lock()
		cr.cmdBuf = append(cr.cmdBuf, GS_CONSOLE_ESC)
		cr.mu.Unlock()
		if cr.onCmdChanged != nil {
			cr.onCmdChanged()
		}
	case 'c':
		if cr.onCloseConsole != nil {
			cr.onCloseConsole()
		}
	case '[':
		cr.readArrowSequence()
	default:
		cr.mu.Lock()
		cr.cmdBuf = append(cr.cmdBuf, c)
		cr.mu.Unlock()
		if cr.onCmdChanged != nil {
			cr.onCmdChanged()
		}
	}
}

// forwardEscape processes Ctrl-E sequences when in shell mode.
// Returns the number of bytes written to p (0 or 1).
func (cr *ConsoleReader) forwardEscape(c byte, p []byte) int {
	switch c {
	case 'E', 'e', GS_CONSOLE_ESC:
		p[0] = GS_CONSOLE_ESC
		return 1
	case '[':
		cr.readArrowSequence()
		return 0
	default:
		p[0] = c
		return 1
	}
}

// handleConsoleChar processes a regular (non-escape) byte in console mode.
func (cr *ConsoleReader) handleConsoleChar(c byte) {
	switch c {
	case 0x0D: // Enter
		cr.mu.Lock()
		var cmd string
		if len(cr.cmdBuf) > 0 {
			cmd = string(cr.cmdBuf)
		}
		cr.cmdBuf = cr.cmdBuf[:0]
		cr.mu.Unlock()
		if cmd != "" && cr.onCommand != nil {
			cr.onCommand(cmd)
		}
		if cr.onCmdChanged != nil {
			cr.onCmdChanged()
		}
	case 0x7F: // Backspace
		cr.mu.Lock()
		if len(cr.cmdBuf) > 0 {
			cr.cmdBuf = cr.cmdBuf[:len(cr.cmdBuf)-1]
		}
		cr.mu.Unlock()
		if cr.onCmdChanged != nil {
			cr.onCmdChanged()
		}
	default:
		if c >= 0x20 && c < 0x7F {
			cr.mu.Lock()
			cr.cmdBuf = append(cr.cmdBuf, c)
			cr.mu.Unlock()
			if cr.onCmdChanged != nil {
				cr.onCmdChanged()
			}
		}
	}
}

// readArrowSequence consumes the rest of an ANSI arrow key sequence.
func (cr *ConsoleReader) readArrowSequence() {
	buf := make([]byte, 1)
	_, _ = cr.src.Read(buf)
	switch buf[0] {
	case 'A':
		if cr.onFocusUp != nil {
			cr.onFocusUp()
		}
	case 'B':
		if cr.onFocusDown != nil {
			cr.onFocusDown()
		}
	}
}

// GS_CONSOLE_ESC is the Ctrl-E escape byte (0x05).
const GS_CONSOLE_ESC = 0x05
