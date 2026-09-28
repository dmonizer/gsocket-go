package gsocket

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type ftTestWriter struct{ packets chan []byte }

func (w ftTestWriter) Read([]byte) (int, error) { return 0, io.EOF }

func (w ftTestWriter) Write(p []byte) (int, error) {
	w.packets <- bytes.Clone(p)
	return len(p), nil
}

func testTransferPair(t *testing.T, clientDir, serverDir string) (*FileTransfer, *FileTransfer) {
	t.Helper()
	toClient := make(chan []byte, 256)
	toServer := make(chan []byte, 256)
	clientApp := NewAppProto(ftTestWriter{toServer})
	serverApp := NewAppProto(ftTestWriter{toClient})
	var failures []string
	var failuresMu sync.Mutex
	addFailure := func(s string) {
		failuresMu.Lock()
		failures = append(failures, s)
		failuresMu.Unlock()
	}
	client := NewFileTransfer(clientApp, false, func() string { return clientDir }, func(s string) {
		if strings.Contains(s, "error") {
			addFailure(s)
		}
	})
	server := NewFileTransfer(serverApp, true, func() string { return serverDir }, nil)
	stop := make(chan struct{})
	for _, side := range []struct {
		app *AppProto
		in  <-chan []byte
	}{{clientApp, toClient}, {serverApp, toServer}} {
		go func(app *AppProto, in <-chan []byte) {
			for {
				select {
				case packet := <-in:
					if _, err := app.Decode(packet); err != nil {
						// The test waits on transfer results and reports failures.
						addFailure(err.Error())
						return
					}
				case <-stop:
					return
				}
			}
		}(side.app, side.in)
	}
	t.Cleanup(func() {
		close(stop)
		client.Close()
		server.Close()
		failuresMu.Lock()
		defer failuresMu.Unlock()
		if len(failures) > 0 {
			t.Errorf("transfer errors: %v", failures)
		}
	})
	return client, server
}

func waitTransfer(t *testing.T, ft *FileTransfer, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stats := ft.Stats()
		if stats.Success+stats.Errors >= count {
			if stats.Errors != 0 {
				t.Fatalf("transfer stats: %+v", stats)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("transfer timed out: %+v", ft.Stats())
}

func TestFileTransferPutDirectoryAndResume(t *testing.T) {
	clientDir, serverDir := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(clientDir, "tree"), 0750); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("gsocket-data-"), 500)
	source := filepath.Join(clientDir, "tree", "data.bin")
	if err := os.WriteFile(source, content, 0640); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1700000000, 0)
	if err := os.Chtimes(source, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "tree", "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(serverDir, "tree"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "tree", "data.bin"), content[:1777], 0600); err != nil {
		t.Fatal(err)
	}
	client, _ := testTransferPair(t, clientDir, serverDir)
	if err := client.Put("tree"); err != nil {
		t.Fatal(err)
	}
	waitTransfer(t, client, 2)
	got, err := os.ReadFile(filepath.Join(serverDir, "tree", "data.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("uploaded data: %v, len=%d", err, len(got))
	}
	info, err := os.Stat(filepath.Join(serverDir, "tree", "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 || info.ModTime().Unix() != mtime.Unix() {
		t.Fatalf("uploaded metadata: mode=%o mtime=%v", info.Mode().Perm(), info.ModTime())
	}
	if info, err := os.Stat(filepath.Join(serverDir, "tree", "empty")); err != nil || info.Size() != 0 {
		t.Fatalf("empty file: %v", err)
	}
	if stats := client.Stats(); stats.Bytes != int64(len(content)-1777) {
		t.Fatalf("resumed bytes = %d", stats.Bytes)
	}
}

func TestFileTransferGetGlobAndResume(t *testing.T) {
	clientDir, serverDir := t.TempDir(), t.TempDir()
	first := bytes.Repeat([]byte("abc"), 1800)
	second := []byte("second")
	if err := os.WriteFile(filepath.Join(serverDir, "one.dat"), first, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "two.dat"), second, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "one.dat"), first[:1000], 0600); err != nil {
		t.Fatal(err)
	}
	client, _ := testTransferPair(t, clientDir, serverDir)
	if err := client.Get("*.dat"); err != nil {
		t.Fatal(err)
	}
	waitTransfer(t, client, 2)
	for name, want := range map[string][]byte{"one.dat": first, "two.dat": second} {
		got, err := os.ReadFile(filepath.Join(clientDir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v, len=%d", name, err, len(got))
		}
	}
	if stats := client.Stats(); stats.Bytes != int64(len(first)-1000+len(second)) {
		t.Fatalf("downloaded bytes = %d", stats.Bytes)
	}
}

func TestFileTransferExplicitDotSlashAndDirectoryContents(t *testing.T) {
	clientDir, serverDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(serverDir, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "folder", "item.txt"), []byte("contents"), 0644); err != nil {
		t.Fatal(err)
	}
	client, _ := testTransferPair(t, clientDir, serverDir)
	if err := client.Get("folder/./item.txt"); err != nil {
		t.Fatal(err)
	}
	waitTransfer(t, client, 1)
	if data, err := os.ReadFile(filepath.Join(clientDir, "item.txt")); err != nil || string(data) != "contents" {
		t.Fatalf("dot-slash destination: data=%q, err=%v", data, err)
	}
	if err := client.Get("folder/"); err != nil {
		t.Fatal(err)
	}
	waitTransfer(t, client, 2)
	if _, err := os.Stat(filepath.Join(clientDir, "folder")); !os.IsNotExist(err) {
		t.Fatalf("trailing slash copied root directory: %v", err)
	}
}

func TestFileTransferPutContinuesAfterMissingSource(t *testing.T) {
	clientDir, serverDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(clientDir, "present"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	client, _ := testTransferPair(t, clientDir, serverDir)
	if err := client.Put("missing present"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stats := client.Stats()
		if stats.Success == 1 && stats.Errors == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stats := client.Stats(); stats.Success != 1 || stats.Errors != 1 {
		t.Fatalf("mixed source stats: %+v", stats)
	}
	if data, err := os.ReadFile(filepath.Join(serverDir, "present")); err != nil || string(data) != "ok" {
		t.Fatalf("present source missing: %q, %v", data, err)
	}
}

func TestFileTransferCPacketLayout(t *testing.T) {
	put, err := ftPutPacket(0x10203040, 0640, 123456789, 1700000000, 0, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if put.channel != 128 || len(put.payload) != 32+len("a.txt")+1 {
		t.Fatalf("PUT layout: %+v", put)
	}
	if binary.BigEndian.Uint32(put.payload[:4]) != 0x10203040 || binary.BigEndian.Uint64(put.payload[8:16]) != 123456789 {
		t.Fatal("PUT fields not in C network byte order")
	}
	list, err := ftListReplyPacket(7, 0755, 999, 1700000000, ftFlagLast|ftFlagDir, "/tmp/./dir")
	if err != nil {
		t.Fatal(err)
	}
	if list.channel != 134 || len(list.payload) != 40+len("/tmp/./dir")+1 || list.payload[28] != 3 {
		t.Fatalf("LIST_REPLY layout: %+v", list)
	}
	for _, ch := range []uint8{chnFTAccept, chnFTSwitch} {
		p := ftOffsetPacket(ch, 42, 4096)
		want := 16
		if ch == chnFTAccept {
			want = 24
		}
		if len(p.payload) != want || binary.BigEndian.Uint64(p.payload[8:16]) != 4096 {
			t.Fatalf("offset layout for %d: %+v", ch, p)
		}
	}
	if p := ftErrorPacket(4, ftErrCompleted, ""); p.channel != 132 || len(p.payload) != 9 {
		t.Fatalf("ERROR layout: %+v", p)
	}
}

func TestFileTransferPatternsAreData(t *testing.T) {
	words, err := expandFTWords("'a b' {one,two}.txt", t.TempDir())
	if err != nil || fmt.Sprint(words) != "[a b one.txt two.txt]" {
		t.Fatalf("pattern expansion = %v, %v", words, err)
	}
}

func TestFileTransferCommandSubstitution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("C wordexp command substitution requires a POSIX shell")
	}
	clientDir, serverDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(serverDir, "item.dat"), []byte("from command substitution"), 0644); err != nil {
		t.Fatal(err)
	}
	client, _ := testTransferPair(t, clientDir, serverDir)
	if err := client.Get("$(printf item.dat)"); err != nil {
		t.Fatal(err)
	}
	waitTransfer(t, client, 1)
	if got, err := os.ReadFile(filepath.Join(clientDir, "item.dat")); err != nil || string(got) != "from command substitution" {
		t.Fatalf("downloaded file = %q, %v", got, err)
	}
	if _, err := expandFTWords("$(printf item.dat); touch should-not-run", serverDir); err == nil {
		t.Fatal("accepted a shell control operator outside command substitution")
	}
	for _, pattern := range []string{"$(printf ')')", "$(printf '%s' $(printf item.dat))", "$((1+2))"} {
		if _, err := expandFTWords(pattern, serverDir); err != nil {
			t.Errorf("valid nested or quoted expression %q: %v", pattern, err)
		}
	}
	if _, err := os.Stat(filepath.Join(serverDir, "should-not-run")); !os.IsNotExist(err) {
		t.Fatalf("executed a control operator outside substitution: %v", err)
	}
}

func TestFileTransferDestinationRejectsTraversal(t *testing.T) {
	for _, wire := range []string{"../secret", "/home/./../secret", "dir//file", "/./"} {
		if _, err := ftDestination(t.TempDir(), wire); err == nil {
			t.Fatalf("accepted unsafe name %q", wire)
		}
	}
}
