package gsocket

import "io"

// ConsoleReader wraps an io.Reader (normally os.Stdin) and filters
// Ctrl-E (0x05) escape sequences according to the gsocket console
// protocol. This matches the C implementation's CONSOLE_check_esc()
// state machine.
//
// Escape sequences:
//
//	Ctrl-E + E  → emit literal 0x05 byte
//	Ctrl-E + e  → emit literal 0x05 byte
//	Ctrl-E + Ctrl-E → emit literal 0x05 byte
//	Ctrl-E + ↑  → NOP (switch focus to upper tier — not used without status bar)
//	Ctrl-E + ↓  → NOP (switch focus to lower tier — not used without status bar)
//	Ctrl-E + X  → emit X directly (non-screen behavior, matches C)
//
// This allows applications like emacs to receive Ctrl-E through the
// encrypted channel, while also providing an escape hatch for future
// console functionality.
type ConsoleReader struct {
	src     io.Reader
	pending bool // true when last char was 0x05 and we're in escape mode
}

// NewConsoleReader creates a new ConsoleReader wrapping the given source.
func NewConsoleReader(src io.Reader) *ConsoleReader {
	return &ConsoleReader{src: src}
}

// Read reads data from the underlying reader and filters Ctrl-E escape
// sequences. The returned slice contains only the data that should be
// forwarded (with escape sequences resolved).
//
// It reads a single byte at a time from the source to implement the
// state machine. Performance is not a concern because interactive
// terminal input is low-volume and already in raw mode (byte-at-a-time).
func (cr *ConsoleReader) Read(p []byte) (n int, err error) {
	// We process byte-by-byte from the source. This is acceptable because
	// in raw terminal mode, each keystroke produces a small number of bytes
	// (typically 1-6 for escape sequences like arrow keys).
	buf := make([]byte, 1)
	for n < len(p) {
		_, err := cr.src.Read(buf)
		if err != nil {
			return n, err
		}
		c := buf[0]

		if cr.pending {
			cr.pending = false
			switch c {
			case 'E', 'e':
				// Ctrl-E + E → literal Ctrl-E byte.
				p[n] = GS_CONSOLE_ESC
				n++
				continue
			case GS_CONSOLE_ESC:
				// Ctrl-E + Ctrl-E → literal Ctrl-E byte.
				p[n] = GS_CONSOLE_ESC
				continue
			case '[':
				// Arrow keys: read until we get the final character, then
				// either handle or discard the sequence.
				cr.readArrowSequence()
				continue
			default:
				// Non-screen behavior (matches C): emit the character directly.
				p[n] = c
				n++
				continue
			}
		}

		if c == GS_CONSOLE_ESC {
			cr.pending = true
			continue
		}

		p[n] = c
		n++
	}
	return n, nil
}

// readArrowSequence consumes the rest of an ANSI arrow key sequence
// (e.g., "[A" for Up, "[B" for Down). The '[' has already been consumed.
// Arrow keys after Ctrl-E are currently NOPs — we discard them.
func (cr *ConsoleReader) readArrowSequence() {
	// Read one more byte (the arrow direction: A/B/C/D).
	// In raw mode this should be available immediately.
	buf := make([]byte, 1)
	_, _ = cr.src.Read(buf)
	// Discard — arrow keys after Ctrl-E are NOPs.
}

// GS_CONSOLE_ESC is the Ctrl-E escape byte (0x05), matching C's
// GS_CONSOLE_ESC definition in console.h.
const GS_CONSOLE_ESC = 0x05
