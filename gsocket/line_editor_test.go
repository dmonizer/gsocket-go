package gsocket

import (
	"bytes"
	"sync"
	"testing"
)

func newTestEditor() *LineEditor {
	var mu sync.Mutex
	return NewLineEditor(nil, &mu) // nil display — no ANSI output in tests
}

func TestLineEditorInsert(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('a')
	ed.Insert('b')
	ed.Insert('c')
	if s := ed.Flush(); s != "abc" {
		t.Fatalf("got %q, want %q", s, "abc")
	}
}

func TestLineEditorBackspace(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('a')
	ed.Insert('b')
	ed.Insert('c')
	ed.Backspace()
	if s := ed.Flush(); s != "ab" {
		t.Fatalf("got %q, want %q", s, "ab")
	}
}

func TestLineEditorBackspaceEmpty(t *testing.T) {
	ed := newTestEditor()
	ed.Backspace() // should not panic
	if s := ed.Flush(); s != "" {
		t.Fatalf("got %q, want empty", s)
	}
}

func TestLineEditorDelete(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('a')
	ed.Insert('b')
	ed.Insert('c')
	ed.MoveLeft() // cursor between 'b' and 'c'
	ed.Delete()   // delete 'c'
	if s := ed.Flush(); s != "ab" {
		t.Fatalf("got %q, want %q", s, "ab")
	}
}

func TestLineEditorDeleteAtEnd(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('a')
	ed.Delete() // cursor at end, nothing to delete
	if s := ed.Flush(); s != "a" {
		t.Fatalf("got %q, want %q", s, "a")
	}
}

func TestLineEditorMoveLeftRight(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('a')
	ed.Insert('b')
	ed.Insert('c')
	ed.MoveLeft()
	if ed.Pos() != 2 {
		t.Fatalf("after MoveLeft: pos=%d, want 2", ed.Pos())
	}
	ed.MoveLeft()
	if ed.Pos() != 1 {
		t.Fatalf("after second MoveLeft: pos=%d, want 1", ed.Pos())
	}
	ed.MoveRight()
	if ed.Pos() != 2 {
		t.Fatalf("after MoveRight: pos=%d, want 2", ed.Pos())
	}
}

func TestLineEditorHomeEnd(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('a')
	ed.Insert('b')
	ed.Insert('c')
	ed.Home()
	if ed.Pos() != 0 {
		t.Fatalf("after Home: pos=%d, want 0", ed.Pos())
	}
	ed.End()
	if ed.Pos() != 3 {
		t.Fatalf("after End: pos=%d, want 3", ed.Pos())
	}
}

func TestLineEditorInsertMiddle(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('a')
	ed.Insert('c')
	ed.MoveLeft()  // cursor between 'a' and 'c'
	ed.Insert('b') // insert 'b'
	if string(ed.Bytes()) != "abc" {
		t.Fatalf("got %q, want %q", ed.Bytes(), "abc")
	}
	if ed.Pos() != 2 {
		t.Fatalf("pos=%d, want 2", ed.Pos())
	}
}

func TestLineEditorFlushClears(t *testing.T) {
	ed := newTestEditor()
	ed.Insert('x')
	_ = ed.Flush()
	if len(ed.Bytes()) != 0 {
		t.Fatal("buffer not cleared after Flush")
	}
	if ed.Pos() != 0 {
		t.Fatal("pos not reset after Flush")
	}
	// Second line works.
	ed.Insert('y')
	if s := ed.Flush(); s != "y" {
		t.Fatalf("second line: got %q, want %q", s, "y")
	}
}

func TestLineEditorRedrawOutput(t *testing.T) {
	// With a real display buffer, verify ANSI output.
	var display bytes.Buffer
	var mu sync.Mutex
	ed := NewLineEditor(&display, &mu)

	ed.Insert('h')
	ed.Redraw()
	if got := display.String(); got != "\033[Kh" {
		t.Fatalf("first redraw: %q", got)
	}
	display.Reset()
	ed.Insert('i')
	ed.Redraw()
	if got := display.String(); got != "\033[1D\033[Khi" {
		t.Fatalf("second redraw: %q", got)
	}
	display.Reset()
	ed.Home()
	ed.RepositionCursor()
	if got := display.String(); got != "\033[2D" {
		t.Fatalf("home: %q", got)
	}
	display.Reset()
	ed.RepositionCursor()
	if display.Len() != 0 {
		t.Fatalf("stationary cursor moved: %q", display.String())
	}
}

func TestLineEditorNewline(t *testing.T) {
	var display bytes.Buffer
	var mu sync.Mutex
	ed := NewLineEditor(&display, &mu)
	ed.Newline()
	if display.String() != "\r\n" {
		t.Fatalf("Newline: got %q, want %q", display.String(), "\r\n")
	}
}
