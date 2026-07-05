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
