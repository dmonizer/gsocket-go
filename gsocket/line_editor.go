package gsocket

import (
	"fmt"
	"io"
	"sync"
)

// LineEditor provides local line editing: characters accumulate in a
// buffer, arrow keys move the cursor, backspace deletes. Only Enter
// flushes the completed line. ANSI escape sequences are written to
// display for instant local echo.
//
// Safe for concurrent use: all methods that write to display must be
// called while holding the display mutex.
type LineEditor struct {
	buf       []byte
	pos       int     // cursor position within buf
	saved     bool    // true after saving cursor position for this line
	display   io.Writer
	displayMu *sync.Mutex // serialises display writes with channel output
}

// NewLineEditor creates a new line editor that writes redraw escape
// sequences to display (normally os.Stdout). displayMu should be the
// same mutex used by the channel→stdout path to avoid interleaving.
func NewLineEditor(display io.Writer, displayMu *sync.Mutex) *LineEditor {
	return &LineEditor{
		display:   display,
		displayMu: displayMu,
	}
}

// Insert adds a character at the cursor position.
func (le *LineEditor) Insert(c byte) {
	le.buf = append(le.buf, 0)
	copy(le.buf[le.pos+1:], le.buf[le.pos:])
	le.buf[le.pos] = c
	le.pos++
}

// Backspace deletes the character before the cursor.
func (le *LineEditor) Backspace() {
	if le.pos > 0 {
		le.pos--
		copy(le.buf[le.pos:], le.buf[le.pos+1:])
		le.buf = le.buf[:len(le.buf)-1]
	}
}

// Delete deletes the character at the cursor.
func (le *LineEditor) Delete() {
	if le.pos < len(le.buf) {
		copy(le.buf[le.pos:], le.buf[le.pos+1:])
		le.buf = le.buf[:len(le.buf)-1]
	}
}

// MoveLeft moves the cursor one position left.
func (le *LineEditor) MoveLeft() {
	if le.pos > 0 {
		le.pos--
	}
}

// MoveRight moves the cursor one position right.
func (le *LineEditor) MoveRight() {
	if le.pos < len(le.buf) {
		le.pos++
	}
}

// Home moves the cursor to the beginning of the line.
func (le *LineEditor) Home() {
	le.pos = 0
}

// End moves the cursor to the end of the line.
func (le *LineEditor) End() {
	le.pos = len(le.buf)
}

// Bytes returns the current buffer contents.
func (le *LineEditor) Bytes() []byte {
	return le.buf
}

// Pos returns the current cursor position.
func (le *LineEditor) Pos() int {
	return le.pos
}

// Flush returns the current buffer contents as a string and clears
// the editor state for the next line.
func (le *LineEditor) Flush() string {
	s := string(le.buf)
	le.buf = le.buf[:0]
	le.pos = 0
	le.saved = false
	return s
}

// Redraw clears the current line and redraws the buffer with cursor.
// On first call per line the cursor position is saved; subsequent
// calls restore that position so the shell prompt is preserved.
func (le *LineEditor) Redraw() {
	if le.display == nil {
		return
	}
	le.displayMu.Lock()
	defer le.displayMu.Unlock()

	if !le.saved {
		// First keystroke of this line — save cursor position.
		fmt.Fprintf(le.display, "\033[s")
		le.saved = true
	} else {
		// Restore saved position.
		fmt.Fprintf(le.display, "\033[u")
	}
	// Clear from saved position to end of line, write buffer, position cursor.
	fmt.Fprintf(le.display, "\033[K%s", le.buf)
	if le.pos < len(le.buf) {
		// Cursor is past the buffer end (after write) — move back.
		fmt.Fprintf(le.display, "\033[%dD", len(le.buf)-le.pos)
	}
}

// RepositionCursor moves the cursor without redrawing. Used after
// arrow key movement within an unchanged buffer.
func (le *LineEditor) RepositionCursor() {
	if le.display == nil || !le.saved {
		return
	}
	le.displayMu.Lock()
	defer le.displayMu.Unlock()
	fmt.Fprintf(le.display, "\033[u\033[%dC", le.pos)
}

// Newline advances to the next line (called on Enter before flush).
func (le *LineEditor) Newline() {
	if le.display == nil {
		return
	}
	le.displayMu.Lock()
	defer le.displayMu.Unlock()
	fmt.Fprintf(le.display, "\r\n")
}
