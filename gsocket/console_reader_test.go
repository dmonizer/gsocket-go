package gsocket

import (
	"bytes"
	"io"
	"testing"
)

func TestConsoleReaderPassthrough(t *testing.T) {
	input := []byte("hello world\n")
	cr := NewConsoleReader(bytes.NewReader(input))
	buf := make([]byte, 1024)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Fatalf("got %d bytes, want %d", n, len(input))
	}
	if string(buf[:n]) != string(input) {
		t.Fatalf("got %q, want %q", buf[:n], input)
	}
}

func TestConsoleReaderCtrlEscape(t *testing.T) {
	input := []byte{0x05, 'E'}
	cr := NewConsoleReader(bytes.NewReader(input))
	buf := make([]byte, 1024)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 1 || buf[0] != 0x05 {
		t.Fatalf("got %d bytes [%x], want 1 byte [05]", n, buf[:n])
	}
}

func TestConsoleReaderCtrlE_CtrlE(t *testing.T) {
	input := []byte{0x05, 0x05}
	cr := NewConsoleReader(bytes.NewReader(input))
	buf := make([]byte, 1024)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 1 || buf[0] != 0x05 {
		t.Fatalf("got %d bytes [%x], want 1 byte [05]", n, buf[:n])
	}
}

func TestConsoleReaderCtrlE_Other(t *testing.T) {
	input := []byte{0x05, 'X'}
	cr := NewConsoleReader(bytes.NewReader(input))
	buf := make([]byte, 1024)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 1 || buf[0] != 'X' {
		t.Fatalf("got %d bytes [%x], want 1 byte [X]", n, buf[:n])
	}
}

// TestConsoleReaderPerKeystroke simulates raw-mode stdin where each
// keystroke arrives as a separate read. The fix for the "no input" bug
// ensures Read() returns after each chunk instead of blocking to fill p.
func TestConsoleReaderPerKeystroke(t *testing.T) {
	// Simulate typing "ls" + Enter in raw mode: each keystroke is separate.
	keystrokes := [][]byte{
		{'l'},
		{'s'},
		{0x0D}, // Enter (carriage return)
	}
	cr := NewConsoleReader(bytes.NewReader(nil)) // placeholder
	buf := make([]byte, 8192)

	for i, ks := range keystrokes {
		cr.src = bytes.NewReader(ks) // swap source for each "keystroke"
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("keystroke %d: unexpected error: %v", i, err)
		}
		if n != len(ks) {
			t.Fatalf("keystroke %d: got %d bytes, want %d", i, n, len(ks))
		}
		if !bytes.Equal(buf[:n], ks) {
			t.Fatalf("keystroke %d: got %x, want %x", i, buf[:n], ks)
		}
	}
}

// TestConsoleReaderReturnsAfterChunk verifies that Read returns immediately
// after processing available data, rather than blocking to fill the buffer.
// This is the regression test for the "no input" bug.
func TestConsoleReaderReturnsAfterChunk(t *testing.T) {
	// Only 3 bytes available — Read must return with n=3, not block.
	input := []byte("abc")
	cr := NewConsoleReader(bytes.NewReader(input))
	buf := make([]byte, 8192) // much larger than input
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("got %d bytes, want 3 — Read is blocking to fill buffer!", n)
	}
	if string(buf[:n]) != "abc" {
		t.Fatalf("got %q, want %q", buf[:n], "abc")
	}
}

// TestConsoleReaderCtrlC verifies that 0x03 (Ctrl-C) passes through
// unmodified in shell mode (no console UI).
func TestConsoleReaderCtrlC(t *testing.T) {
	input := []byte{0x03}
	cr := NewConsoleReader(bytes.NewReader(input))
	buf := make([]byte, 1024)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 1 || buf[0] != 0x03 {
		t.Fatalf("got %d bytes [%x], want 1 byte [03]", n, buf[:n])
	}
}

// TestConsoleReaderMultipleCtrlE verifies that repeated Ctrl-E sequences
// are handled correctly without blocking.
func TestConsoleReaderMultipleCtrlE(t *testing.T) {
	// Ctrl-E+E (→ 0x05) then regular 'a'
	input := []byte{0x05, 'E', 'a'}
	cr := NewConsoleReader(bytes.NewReader(input))
	buf := make([]byte, 1024)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	// Expect: literal 0x05 followed by 'a'
	if n != 2 || buf[0] != 0x05 || buf[1] != 'a' {
		t.Fatalf("got %d bytes [%x], want [05 61]", n, buf[:n])
	}
}

// TestConsoleReaderArrowKeys verifies that Ctrl-E + arrow keys trigger
// focus callbacks in shell mode without forwarding any bytes.
func TestConsoleReaderArrowKeys(t *testing.T) {
	t.Run("CtrlE+Up_ShellMode", func(t *testing.T) {
		// Ctrl-E + ESC [ A  → onFocusUp
		input := []byte{0x05, 0x1B, '[', 'A'}
		cr := NewConsoleReader(bytes.NewReader(input))
		var focusUpCalled bool
		cr.onFocusUp = func() { focusUpCalled = true }
		buf := make([]byte, 1024)
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("arrow should forward 0 bytes, got %d", n)
		}
		if !focusUpCalled {
			t.Fatal("onFocusUp was not called")
		}
	})
	t.Run("CtrlE+Down_ShellMode", func(t *testing.T) {
		// Ctrl-E + ESC [ B  → onFocusDown
		input := []byte{0x05, 0x1B, '[', 'B'}
		cr := NewConsoleReader(bytes.NewReader(input))
		var focusDownCalled bool
		cr.onFocusDown = func() { focusDownCalled = true }
		buf := make([]byte, 1024)
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("arrow should forward 0 bytes, got %d", n)
		}
		if !focusDownCalled {
			t.Fatal("onFocusDown was not called")
		}
	})
	t.Run("CtrlE+Up_ConsoleMode", func(t *testing.T) {
		input := []byte{0x05, 0x1B, '[', 'A'}
		cr := NewConsoleReader(bytes.NewReader(input))
		cr.SetConsoleMode(true)
		var focusUpCalled bool
		cr.onFocusUp = func() { focusUpCalled = true }
		buf := make([]byte, 1024)
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("arrow should forward 0 bytes, got %d", n)
		}
		if !focusUpCalled {
			t.Fatal("onFocusUp was not called in console mode")
		}
	})
	t.Run("CtrlE+Down_ConsoleMode", func(t *testing.T) {
		input := []byte{0x05, 0x1B, '[', 'B'}
		cr := NewConsoleReader(bytes.NewReader(input))
		cr.SetConsoleMode(true)
		var focusDownCalled bool
		cr.onFocusDown = func() { focusDownCalled = true }
		buf := make([]byte, 1024)
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("arrow should forward 0 bytes, got %d", n)
		}
		if !focusDownCalled {
			t.Fatal("onFocusDown was not called in console mode")
		}
	})
}

// TestConsoleReaderCtrlE_C_ShellMode verifies Ctrl-E + c triggers
// the close-console callback in shell mode.
func TestConsoleReaderCtrlE_C_ShellMode(t *testing.T) {
	input := []byte{0x05, 'c'}
	cr := NewConsoleReader(bytes.NewReader(input))
	var closeCalled bool
	cr.onCloseConsole = func() { closeCalled = true }
	buf := make([]byte, 1024)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Ctrl-E+c should forward 0 bytes, got %d", n)
	}
	if !closeCalled {
		t.Fatal("onCloseConsole was not called")
	}
}

// TestConsoleReaderArrowKeys_LineEdit verifies arrow-based focus
// switching when line editing is active.
func TestConsoleReaderArrowKeys_LineEdit(t *testing.T) {
	t.Run("CtrlE+Up_SwitchesToShell", func(t *testing.T) {
		input := []byte{0x05, 0x1B, '[', 'A'}
		cr := NewConsoleReader(bytes.NewReader(input))
		var focusUpCalled bool
		cr.onFocusUp = func() { focusUpCalled = true }
		ed := NewLineEditor(nil, nil)
		cr.SetLineMode(true, ed)
		buf := make([]byte, 1024)
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("arrow should forward 0 bytes, got %d", n)
		}
		if !focusUpCalled {
			t.Fatal("onFocusUp was not called in line-edit mode")
		}
	})
	t.Run("CtrlE+Down_SwitchesToConsole", func(t *testing.T) {
		input := []byte{0x05, 0x1B, '[', 'B'}
		cr := NewConsoleReader(bytes.NewReader(input))
		var focusDownCalled bool
		cr.onFocusDown = func() { focusDownCalled = true }
		ed := NewLineEditor(nil, nil)
		cr.SetLineMode(true, ed)
		buf := make([]byte, 1024)
		n, err := cr.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("arrow should forward 0 bytes, got %d", n)
		}
		if !focusDownCalled {
			t.Fatal("onFocusDown was not called in line-edit mode")
		}
	})
}
