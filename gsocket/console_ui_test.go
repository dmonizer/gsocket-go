package gsocket

import (
	"bytes"
	"testing"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{512, "512B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1048576, "1.0MB"},
		{1073741824, "1.0GB"},
		{3221225472, "3.0GB"},
	}
	for _, tt := range tests {
		got := formatSize(tt.n)
		if got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestConsoleCommandDispatch(t *testing.T) {
	cases := []struct {
		name  string
		input string // input to feed through ConsoleReader in console mode
		want  string // expected command string (empty means nothing dispatched)
	}{
		{"ping", "ping\r", "ping"},
		{"pwd", "pwd\r", "pwd"},
		{"log", "log\r", "log"},
		{"ids", "ids\r", "ids"},
		{"clear", "clear\r", "clear"},
		{"unknown_cmd", "unknown_cmd\r", "unknown_cmd"},
		{"empty", "\r", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			src := bytes.NewBufferString(tt.input)
			c := NewConsole(src)
			defer c.Close()

			var gotCmd string
			c.OnCommand = func(cmd string) {
				gotCmd = cmd
			}

			// Enter console mode and read to trigger command dispatch.
			c.reader.SetConsoleMode(true)
			buf := make([]byte, 1024)
			_, _ = c.Read(buf)

			if gotCmd != tt.want {
				t.Errorf("dispatch %q: got %q, want %q", tt.name, gotCmd, tt.want)
			}
		})
	}
}

func TestNewConsole(t *testing.T) {
	c := NewConsole(bytes.NewReader(nil))
	if c == nil {
		t.Fatal("NewConsole returned nil")
	}
	if !c.Running() {
		t.Error("NewConsole should be running initially")
	}
	c.Close()
	if c.Running() {
		t.Error("Console should not be running after Close")
	}
}

func TestConsoleSetMethods(t *testing.T) {
	c := NewConsole(bytes.NewReader(nil))
	defer c.Close()
	// Verify SetPingRTT, SetBPS, SetComment do not panic.
	c.SetPingRTT(0)
	c.SetPingRTT(45000000) // 45ms
	c.SetBPS(0, 0)
	c.SetBPS(1024, 2048)
	c.SetComment("")
	c.SetComment("test comment")
}
