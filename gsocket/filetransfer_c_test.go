package gsocket

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type ftPipe struct {
	io.Reader
	io.Writer
}

// Set GSOCKET_C_FT_TEST to tools/filetransfer-test from the C beta tree to
// exercise both implementations against each other without the crypto layer.
// The C server test additionally needs its standalone harness to pass getpid()
// to GS_FT_init; upstream passes zero and writes to /dev/shm instead of cwd.
func testCTransfer(t *testing.T, mode, dir, otherDir, file string, goIsServer bool) (*FileTransfer, *exec.Cmd) {
	t.Helper()
	bin := os.Getenv("GSOCKET_C_FT_TEST")
	if bin == "" {
		t.Skip("set GSOCKET_C_FT_TEST to the C beta filetransfer-test binary")
	}
	args := []string{mode}
	if file != "" {
		args = append(args, file)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var logs bytes.Buffer
	cmd.Stderr = &logs
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	app := NewAppProto(ftPipe{stdout, stdin})
	ft := NewFileTransfer(app, goIsServer, func() string { return otherDir }, nil)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				_, _ = app.Decode(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		ft.Close()
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("C transfer log:\n%s", logs.String())
		}
	})
	return ft, cmd
}

func waitForFile(t *testing.T, path string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && bytes.Equal(data, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(path)
	t.Fatalf("file %s: got %q (%v), want %q", path, data, err, want)
}

func TestFileTransferWithCServer(t *testing.T) {
	if os.Getenv("GSOCKET_C_FT_TEST_SERVER_CWD") != "1" {
		t.Skip("C server harness must use getpid() for its transfer working directory")
	}
	clientDir, serverDir := t.TempDir(), t.TempDir()
	want := bytes.Repeat([]byte("from Go to C"), 300)
	if err := os.WriteFile(filepath.Join(clientDir, "item.bin"), want, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "item.bin"), want[:777], 0600); err != nil {
		t.Fatal(err)
	}
	ft, _ := testCTransfer(t, "s", serverDir, clientDir, "", false)
	if err := ft.Put("item.bin"); err != nil {
		t.Fatal(err)
	}
	waitTransfer(t, ft, 1)
	waitForFile(t, filepath.Join(serverDir, "item.bin"), want)
	if got := ft.Stats().Bytes; got != int64(len(want)-777) {
		t.Fatalf("C server resume sent %d bytes, want %d", got, len(want)-777)
	}
}

func TestFileTransferWithCClientPut(t *testing.T) {
	clientDir, serverDir := t.TempDir(), t.TempDir()
	want := bytes.Repeat([]byte("from C to Go"), 300)
	if err := os.WriteFile(filepath.Join(clientDir, "item.bin"), want, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "item.bin"), want[:777], 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = testCTransfer(t, "c", clientDir, serverDir, "item.bin", true)
	waitForFile(t, filepath.Join(serverDir, "item.bin"), want)
}

func TestFileTransferWithCClientGet(t *testing.T) {
	clientDir, serverDir := t.TempDir(), t.TempDir()
	want := bytes.Repeat([]byte("from Go to C GET"), 300)
	if err := os.WriteFile(filepath.Join(serverDir, "item.bin"), want, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "item.bin"), want[:777], 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = testCTransfer(t, "C", clientDir, serverDir, "item.bin", true)
	waitForFile(t, filepath.Join(clientDir, "item.bin"), want)
}

func TestFileTransferWithCClientGetCommandSubstitution(t *testing.T) {
	clientDir, serverDir := t.TempDir(), t.TempDir()
	want := []byte("expanded by the Go server")
	if err := os.WriteFile(filepath.Join(serverDir, "item.bin"), want, 0644); err != nil {
		t.Fatal(err)
	}
	_, _ = testCTransfer(t, "C", clientDir, serverDir, "$(printf item.bin)", true)
	waitForFile(t, filepath.Join(clientDir, "item.bin"), want)
}
