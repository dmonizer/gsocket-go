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
// Editing methods are called by the input goroutine. Display methods
// acquire the shared display mutex internally.
type LineEditor struct {
	buf       []byte
	pos       int  // cursor position within buf
	saved     bool // true after drawing this line
	drawnPos  int  // cursor offset at the last display update
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
	le.drawnPos = 0
	return s
}

// Redraw clears the current line and redraws the buffer with cursor.
// Cursor movement is relative to the last rendered position. This leaves
// the terminal save slot available to the console UI.
func (le *LineEditor) Redraw() {
	if le.display == nil {
		return
	}
	le.displayMu.Lock()
	defer le.displayMu.Unlock()

	if le.saved && le.drawnPos > 0 {
		fmt.Fprintf(le.display, "\033[%dD", le.drawnPos)
	}
	le.saved = true
	// Clear from the input start to end of line, then position the cursor.
	fmt.Fprintf(le.display, "\033[K%s", le.buf)
	if le.pos < len(le.buf) {
		// Cursor is past the buffer end (after write) — move back.
		fmt.Fprintf(le.display, "\033[%dD", len(le.buf)-le.pos)
	}
	le.drawnPos = le.pos
}

// RepositionCursor moves the cursor without redrawing. Used after
// arrow key movement within an unchanged buffer.
func (le *LineEditor) RepositionCursor() {
	if le.display == nil || !le.saved {
		return
	}
	le.displayMu.Lock()
	defer le.displayMu.Unlock()
	if delta := le.pos - le.drawnPos; delta > 0 {
		fmt.Fprintf(le.display, "\033[%dC", delta)
	} else if delta < 0 {
		fmt.Fprintf(le.display, "\033[%dD", -delta)
	}
	le.drawnPos = le.pos
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
