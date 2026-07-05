package gsocket

import "io"

// ConsoleReader wraps an io.Reader (normally os.Stdin) and filters
// Ctrl-E (0x05) escape sequences according to the gsocket console
// protocol. This matches the C implementation's CONSOLE_check_esc()
// state machine.
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
//
// This allows applications like emacs to receive Ctrl-E through the
// encrypted channel, while also providing console functionality.
type ConsoleReader struct {
	src     io.Reader
	pending bool // true when last char was 0x05 and we're in escape mode

	// Console mode state (set by Console UI via SetConsoleMode).
	consoleMode bool
	cmdBuf      []byte

	// Callbacks for Ctrl-E events. Set by Console UI.
	onFocusUp      func()
	onFocusDown    func()
	onCloseConsole func()
	onCommand      func(string) // Enter pressed in console mode
	onCmdChanged   func()       // cmdBuf contents changed (for redraw)
}

// NewConsoleReader creates a new ConsoleReader wrapping the given source.
func NewConsoleReader(src io.Reader) *ConsoleReader {
	return &ConsoleReader{src: src}
}

// SetConsoleMode toggles command accumulation mode.
// In console mode, printable characters go to the command buffer
// instead of being forwarded. Call with false to return to shell mode.
func (cr *ConsoleReader) SetConsoleMode(on bool) {
	cr.consoleMode = on
}

// CmdBuf returns a copy of the current command buffer contents.
func (cr *ConsoleReader) CmdBuf() []byte {
	return append([]byte{}, cr.cmdBuf...)
}

// Read reads data from the underlying reader and filters Ctrl-E escape
// sequences. In shell mode, the returned slice contains only data to
// forward to the channel. In console mode, bytes accumulate in the
// command buffer and the returned count is typically 0.
//
// It reads a single byte at a time from the source to implement the
// state machine. Performance is not a concern because interactive
// terminal input is low-volume and already in raw mode.
func (cr *ConsoleReader) Read(p []byte) (n int, err error) {
	buf := make([]byte, 1)
	for n < len(p) {
		_, err := cr.src.Read(buf)
		if err != nil {
			return n, err
		}
		c := buf[0]

		if cr.pending {
			cr.pending = false
			if cr.consoleMode {
				cr.handleConsoleEscape(c)
			} else {
				// In shell mode, Ctrl-E + c also closes, but only if
				// the console is active (onCloseConsole is set).
				if c == 'c' && cr.onCloseConsole != nil {
					cr.onCloseConsole()
					continue
				}
				n += cr.forwardEscape(c, p[n:])
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

		p[n] = c
		n++
	}
	return n, nil
}

// handleConsoleEscape processes Ctrl-E sequences when in console mode.
// Ctrl-E + E/e/Ctrl-E → literal Ctrl-E in cmdBuf.
// Ctrl-E + ↑ → onFocusUp.
// Ctrl-E + c → onCloseConsole.
func (cr *ConsoleReader) handleConsoleEscape(c byte) {
	switch c {
	case GS_CONSOLE_ESC, 'E', 'e':
		cr.cmdBuf = append(cr.cmdBuf, GS_CONSOLE_ESC)
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
		// Non-screen behavior: forward to cmdBuf.
		cr.cmdBuf = append(cr.cmdBuf, c)
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
		// Ctrl-E + E/e/Ctrl-E → literal Ctrl-E byte.
		p[0] = GS_CONSOLE_ESC
		return 1
	case '[':
		cr.readArrowSequence()
		return 0
	default:
		// Non-screen behavior (matches C): emit the character directly.
		p[0] = c
		return 1
	}
}

// handleConsoleChar processes a regular (non-escape) byte in console mode.
func (cr *ConsoleReader) handleConsoleChar(c byte) {
	switch c {
	case 0x0D: // Enter
		if cr.onCommand != nil && len(cr.cmdBuf) > 0 {
			cr.onCommand(string(cr.cmdBuf))
		}
		cr.cmdBuf = cr.cmdBuf[:0]
		if cr.onCmdChanged != nil {
			cr.onCmdChanged()
		}
	case 0x7F: // Backspace
		if len(cr.cmdBuf) > 0 {
			cr.cmdBuf = cr.cmdBuf[:len(cr.cmdBuf)-1]
			if cr.onCmdChanged != nil {
				cr.onCmdChanged()
			}
		}
	default:
		// Printable ASCII only.
		if c >= 0x20 && c < 0x7F {
			cr.cmdBuf = append(cr.cmdBuf, c)
			if cr.onCmdChanged != nil {
				cr.onCmdChanged()
			}
		}
	}
}

// readArrowSequence consumes the rest of an ANSI arrow key sequence
// (e.g. "[A" for Up, "[B" for Down, "[C" for Right, "[D" for Left).
// The '[' has already been consumed. Dispatches onFocusUp/Down for
// A/B; discards C/D.
func (cr *ConsoleReader) readArrowSequence() {
	buf := make([]byte, 1)
	_, _ = cr.src.Read(buf)
	switch buf[0] {
	case 'A': // Up arrow
		if cr.onFocusUp != nil {
			cr.onFocusUp()
		}
	case 'B': // Down arrow
		if cr.onFocusDown != nil {
			cr.onFocusDown()
		}
	}
	// C (right), D (left) — silently discard.
}

// GS_CONSOLE_ESC is the Ctrl-E escape byte (0x05), matching C's
// GS_CONSOLE_ESC definition in console.h.
const GS_CONSOLE_ESC = 0x05
