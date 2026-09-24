package gsocket

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// displayTerminal models the cursor, margins, and shared ANSI/DEC save slot.
// It intentionally does not give console and editor separate save slots.
type displayTerminal struct {
	rows, cols         int
	row, col           int
	savedRow, savedCol int
	top, bottom        int
	cells              [][]byte
	stream             bytes.Buffer
	pending            string
}

func newDisplayTerminal(rows, cols int) *displayTerminal {
	t := &displayTerminal{rows: rows, cols: cols, row: 1, col: 1, top: 1, bottom: rows}
	t.cells = make([][]byte, rows)
	for i := range t.cells {
		t.cells[i] = bytes.Repeat([]byte{' '}, cols)
	}
	return t
}

func (d *displayTerminal) Write(p []byte) (int, error) {
	d.stream.Write(p)
	d.pending += string(p)
	for len(d.pending) > 0 {
		if d.pending[0] == '\x1b' {
			if len(d.pending) < 2 {
				break
			}
			if d.pending[1] == '[' {
				end := 2
				for end < len(d.pending) && (d.pending[end] < 0x40 || d.pending[end] > 0x7e) {
					end++
				}
				if end == len(d.pending) {
					break
				}
				d.control(d.pending[2:end], d.pending[end])
				d.pending = d.pending[end+1:]
			} else {
				switch d.pending[1] {
				case '7':
					d.savedRow, d.savedCol = d.row, d.col
				case '8':
					d.row, d.col = min(d.rows, d.savedRow), min(d.cols, d.savedCol)
				case 'D':
					if d.row == d.bottom {
						d.scroll(1)
					} else {
						d.row = min(d.rows, d.row+1)
					}
				}
				d.pending = d.pending[2:]
			}
			continue
		}
		c := d.pending[0]
		d.pending = d.pending[1:]
		switch c {
		case '\r':
			d.col = 1
		case '\n':
			if d.row == d.bottom {
				d.scroll(1)
			} else {
				d.row = min(d.rows, d.row+1)
			}
		default:
			if c >= 0x20 {
				if d.row >= 1 && d.row <= d.rows && d.col >= 1 && d.col <= d.cols {
					d.cells[d.row-1][d.col-1] = c
				}
				d.col = min(d.cols, d.col+1)
			}
		}
	}
	return len(p), nil
}

func (d *displayTerminal) scroll(n int) {
	for i := 0; i < n; i++ {
		copy(d.cells[d.top-1:d.bottom-1], d.cells[d.top:d.bottom])
		d.cells[d.bottom-1] = bytes.Repeat([]byte{' '}, d.cols)
	}
}

func (d *displayTerminal) control(params string, op byte) {
	fields := strings.Split(params, ";")
	value := func(i, fallback int) int {
		if i >= len(fields) {
			return fallback
		}
		n, _ := strconv.Atoi(fields[i])
		if n == 0 {
			return fallback
		}
		return n
	}
	switch op {
	case 'H':
		d.row = min(d.rows, value(0, 1))
		d.col = min(d.cols, value(1, 1))
	case 'A':
		d.row = max(1, d.row-value(0, 1))
	case 'C':
		d.col = min(d.cols, d.col+value(0, 1))
	case 'D':
		d.col = max(1, d.col-value(0, 1))
	case 'r':
		d.top = value(0, 1)
		d.bottom = value(1, d.rows)
		d.row, d.col = 1, 1
	case 'S':
		d.scroll(value(0, 1))
	case 'K':
		start := d.col - 1
		if value(0, 0) == 2 {
			start = 0
		}
		for i := start; i < d.cols; i++ {
			d.cells[d.row-1][i] = ' '
		}
	case 's':
		d.savedRow, d.savedCol = d.row, d.col
	case 'u':
		d.row, d.col = d.savedRow, d.savedCol
	}
}

func testDisplayConsole() (*Console, *displayTerminal) {
	c := NewConsole(bytes.NewReader(nil))
	c.rows, c.cols = 24, 80
	d := newDisplayTerminal(c.rows, c.cols)
	c.display = d
	return c, d
}

func assertCursor(t *testing.T, d *displayTerminal, row, col int) {
	t.Helper()
	if d.row != row || d.col != col {
		t.Fatalf("cursor=(%d,%d), want (%d,%d); output=%q", d.row, d.col, row, col, d.stream.String())
	}
	if d.pending != "" {
		t.Fatalf("incomplete escape sequence %q", d.pending)
	}
}

func TestConsoleDisplayFocusAndOutput(t *testing.T) {
	c, d := testDisplayConsole()
	c.Write([]byte("\x1b[24;1H$ "))
	c.toggleVisibility()
	assertCursor(t, d, 24, 10)
	if !strings.HasPrefix(string(d.cells[23]), "console> ") {
		t.Fatal("prompt was not painted immediately")
	}
	c.enterCommandMode() // repeated focus-down must not replace the shell save
	c.SetComment("updated")
	c.Write([]byte("hello"))
	assertCursor(t, d, 24, 10)
	if !strings.HasPrefix(string(d.cells[19]), "$ hello") {
		t.Fatalf("shell output landed outside shell: %q", d.cells[19])
	}
	c.exitCommandMode()
	assertCursor(t, d, 20, 8)
	c.SetBPS(0, 0)
	assertCursor(t, d, 20, 8)
	c.toggleVisibility()
	assertCursor(t, d, 20, 8)
}

func TestConsoleDisplayHideFromCommandFocus(t *testing.T) {
	c, d := testDisplayConsole()
	c.Write([]byte("\x1b[12;7H"))
	c.toggleVisibility()
	c.SetComment("tick")
	c.toggleVisibility()
	assertCursor(t, d, 12, 7)
	if d.bottom != 24 {
		t.Fatalf("scroll region still ends at %d", d.bottom)
	}
}

func TestConsoleDisplayDeferredStatus(t *testing.T) {
	c, d := testDisplayConsole()
	c.EnableLineEditing(c.stdoutMu)
	c.Write([]byte("\x1b[12;5H"))
	c.toggleVisibility()
	c.exitCommandMode()
	c.SetComment("deferred")
	c.reader.editor.Newline()
	c.flushDeferredStatusBar()
	assertCursor(t, d, 13, 1)
	c.Write([]byte("response"))
	if !strings.HasPrefix(string(d.cells[12]), "response") {
		t.Fatal("response did not land at shell cursor")
	}
	c.SetComment("hidden")
	c.toggleVisibility()
	before := d.stream.Len()
	c.flushDeferredStatusBar()
	if d.stream.Len() != before {
		t.Fatal("hidden console was repainted")
	}
}

func TestConsoleDisplayResizeFocus(t *testing.T) {
	for _, focus := range []consoleFocus{focusShell, focusConsole} {
		t.Run(fmt.Sprint(focus), func(t *testing.T) {
			c, d := testDisplayConsole()
			c.Write([]byte("\x1b[12;7H"))
			c.toggleVisibility()
			if focus == focusShell {
				c.exitCommandMode()
			}
			c.HandleWinch(24, 60)
			if focus == focusConsole {
				assertCursor(t, d, 24, 10)
				c.exitCommandMode()
			}
			assertCursor(t, d, 12, 7)
		})
	}
}

func TestConsoleDisplayEditingAcrossFocus(t *testing.T) {
	c, d := testDisplayConsole()
	c.EnableLineEditing(c.stdoutMu)
	c.Write([]byte("\x1b[12;1H$ "))
	ed := c.reader.editor
	ed.Insert('a')
	ed.Redraw()
	c.toggleVisibility()
	c.SetComment("tick")
	c.exitCommandMode()
	ed.Insert('b')
	ed.Redraw()
	if got := string(d.cells[11][:4]); got != "$ ab" {
		t.Fatalf("local editing lost its position: %q", got)
	}
	assertCursor(t, d, 12, 5)
	ed.Home()
	ed.RepositionCursor()
	assertCursor(t, d, 12, 3)
}

func TestConsoleDisplayConcurrentUpdates(t *testing.T) {
	c, d := testDisplayConsole()
	c.Write([]byte("\x1b[12;1H"))
	c.toggleVisibility()
	var wg sync.WaitGroup
	for _, work := range []func(){
		func() {
			for i := 0; i < 100; i++ {
				c.Write([]byte("x\r\n"))
			}
		},
		func() {
			for i := 0; i < 100; i++ {
				c.SetComment("tick")
			}
		},
		func() {
			for i := 0; i < 100; i++ {
				c.HandleWinch(24, 80)
			}
		},
		func() {
			for i := 0; i < 100; i++ {
				c.exitCommandMode()
				c.enterCommandMode()
			}
		},
	} {
		wg.Add(1)
		go func(f func()) { defer wg.Done(); f() }(work)
	}
	wg.Wait()
	assertCursor(t, d, 24, 10)
	if !strings.HasPrefix(string(d.cells[23]), "console> ") {
		t.Fatal("concurrent output corrupted prompt")
	}
	c.Close()
}

func TestConsoleDisplayOpenAtTop(t *testing.T) {
	c, d := testDisplayConsole()
	c.Write([]byte("$ "))
	c.toggleVisibility()
	c.exitCommandMode()
	assertCursor(t, d, 1, 3)
	if string(d.cells[0][:2]) != "$ " {
		t.Fatal("opening console erased the prompt")
	}
}

func TestConsoleDisplayShrinkAtBottom(t *testing.T) {
	c, d := testDisplayConsole()
	c.Write([]byte("\x1b[24;1H$ "))
	c.toggleVisibility()
	// Model the resized terminal's cursor clamping. The shell prompt now
	// occupies its last physical row; old footer addresses are offscreen.
	d.rows, d.bottom = 18, 18
	d.cells = d.cells[:18]
	copy(d.cells[17], "$ ")
	d.row = 18
	c.HandleWinch(18, 80)
	assertCursor(t, d, 18, 10)
	c.exitCommandMode()
	assertCursor(t, d, 15, 3)
	if string(d.cells[14][:2]) != "$ " {
		t.Fatalf("resize erased shell prompt: %q", d.cells[14])
	}
}
