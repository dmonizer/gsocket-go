package gsocket

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

// blockingPipe is a net.Conn-like in-memory pipe where reads block until
// data is available (unlike bytes.Buffer which returns EOF on empty).
type blockingPipe struct {
	mu     sync.Mutex
	buf    []byte
	cond   *sync.Cond
	closed bool
}

func newBlockingPipe() *blockingPipe {
	bp := &blockingPipe{}
	bp.cond = sync.NewCond(&bp.mu)
	return bp
}

func (p *blockingPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.buf) == 0 && p.closed {
		return 0, io.EOF
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

func (p *blockingPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	p.buf = append(p.buf, b...)
	p.cond.Signal()
	return len(b), nil
}

func (p *blockingPipe) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cond.Signal()
	return nil
}

// connectPipes creates two blockingPipes that are cross-connected.
// Data written to a appears readable from b, and vice versa.
func connectPipes() (a, b io.ReadWriteCloser) {
	ab := newBlockingPipe()
	ba := newBlockingPipe()
	return &bidirectional{r: ab, w: ba}, &bidirectional{r: ba, w: ab}
}

type bidirectional struct {
	r *blockingPipe
	w *blockingPipe
}

func (b *bidirectional) Read(p []byte) (int, error)  { return b.r.Read(p) }
func (b *bidirectional) Write(p []byte) (int, error)  { return b.w.Write(p) }
func (b *bidirectional) Close() error {
	b.r.Close()
	b.w.Close()
	return nil
}

func TestHandshakeSuccess(t *testing.T) {
	a, b := connectPipes()
	secret := "test-secret-42"

	var serverCh, clientCh *SecureChannel
	var serverErr, clientErr error

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		serverCh, serverErr = Handshake(a, secret, true)
	}()
	go func() {
		defer wg.Done()
		clientCh, clientErr = Handshake(b, secret, false)
	}()
	wg.Wait()

	if serverErr != nil {
		t.Fatalf("server handshake failed: %v", serverErr)
	}
	if clientErr != nil {
		t.Fatalf("client handshake failed: %v", clientErr)
	}
	defer serverCh.Close()
	defer clientCh.Close()

	// Test encrypted communication in both directions.
	message := []byte("Hello, secure world!")
	if _, err := clientCh.Write(message); err != nil {
		t.Fatalf("client write failed: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := serverCh.Read(buf)
	if err != nil {
		t.Fatalf("server read failed: %v", err)
	}
	if !bytes.Equal(buf[:n], message) {
		t.Errorf("server received %q, want %q", buf[:n], message)
	}

	reply := []byte("Response from server")
	if _, err := serverCh.Write(reply); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	n, err = clientCh.Read(buf)
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}
	if !bytes.Equal(buf[:n], reply) {
		t.Errorf("client received %q, want %q", buf[:n], reply)
	}
}

func TestHandshakeWrongSecret(t *testing.T) {
	a, b := connectPipes()

	var serverErr, clientErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, serverErr = Handshake(a, "server-secret", true)
	}()
	go func() {
		defer wg.Done()
		_, clientErr = Handshake(b, "client-secret", false)
	}()
	wg.Wait()

	if serverErr == nil && clientErr == nil {
		t.Error("expected authentication failure with mismatched secrets")
	}
}

func TestHandshakeRoleDeadlock(t *testing.T) {
	a, b := connectPipes()
	secret := "test-secret"

	errCh := make(chan error, 2)
	go func() { _, err := Handshake(a, secret, false); errCh <- err }()
	go func() { _, err := Handshake(b, secret, false); errCh <- err }()

	// Both are "client" — both try to write Ya first. One sends,
	// the other reads Ya instead of Yb. Either the confirmation
	// check fails or one side deadlocks (which is fine for CPace).
	err1 := <-errCh
	err2 := <-errCh
	if err1 == nil && err2 == nil {
		t.Error("expected deadlock or auth failure with two clients")
	}
}

func TestSecureChannelWriteRead(t *testing.T) {
	a, b := connectPipes()
	secret := "test-secret"

	var serverCh, clientCh *SecureChannel
	var serverErr, clientErr error

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		serverCh, serverErr = Handshake(a, secret, true)
	}()
	go func() {
		defer wg.Done()
		clientCh, clientErr = Handshake(b, secret, false)
	}()
	wg.Wait()

	if serverErr != nil || clientErr != nil {
		t.Fatalf("handshake failed: server=%v, client=%v", serverErr, clientErr)
	}
	defer serverCh.Close()
	defer clientCh.Close()

	messages := [][]byte{
		[]byte("message 1"),
		make([]byte, 0), // empty message
		bytes.Repeat([]byte("A"), 4096), // large message
		[]byte("message 4"),
	}

	// Write all messages first.
	for _, msg := range messages {
		if _, err := clientCh.Write(msg); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}

	// Read them all on server side.
	buf := make([]byte, 8192)
	for i, expected := range messages {
		n, err := serverCh.Read(buf)
		if err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}
		if !bytes.Equal(buf[:n], expected) {
			t.Errorf("message %d: got %q (len=%d), want %q (len=%d)", i, buf[:n], n, expected, len(expected))
		}
	}
}

func TestSecureChannelClosed(t *testing.T) {
	a, b := connectPipes()
	secret := "test-secret"

	var serverCh, clientCh *SecureChannel
	var serverErr, clientErr error

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		serverCh, serverErr = Handshake(a, secret, true)
	}()
	go func() {
		defer wg.Done()
		clientCh, clientErr = Handshake(b, secret, false)
	}()
	wg.Wait()

	if serverErr != nil || clientErr != nil {
		t.Fatalf("handshake failed: server=%v, client=%v", serverErr, clientErr)
	}

	serverCh.Close()

	_, err := serverCh.Write([]byte("test"))
	if err == nil {
		t.Error("expected error writing to closed channel")
	}

	clientCh.Close()
}

func TestClampX25519Scalar(t *testing.T) {
	input := bytes.Repeat([]byte{0xFF}, 32)
	clamped := clampX25519Scalar(input)

	if len(clamped) != 32 {
		t.Errorf("length = %d, want 32", len(clamped))
	}
	if clamped[0]&0x07 != 0 {
		t.Error("low 3 bits not cleared")
	}
	if clamped[31]&0x40 == 0 {
		t.Error("bit 254 not set")
	}
	if clamped[31]&0x80 != 0 {
		t.Error("bit 255 not cleared")
	}
	if input[0] != 0xFF {
		t.Error("original slice mutated")
	}
}

func TestDeriveCPaceScalar(t *testing.T) {
	baseKey := bytes.Repeat([]byte{0x42}, 32)
	d1 := deriveCPaceScalar(baseKey)
	d2 := deriveCPaceScalar(baseKey)

	if len(d1) != 32 {
		t.Errorf("length = %d, want 32", len(d1))
	}
	if !bytes.Equal(d1, d2) {
		t.Error("scalar derivation not deterministic")
	}
	if d1[0]&0x07 != 0 {
		t.Error("low 3 bits not cleared")
	}
}

func TestCPaceConfirmReflection(t *testing.T) {
	kConfirm := bytes.Repeat([]byte{0x13}, 32)
	Ya := bytes.Repeat([]byte{0xAA}, 32)
	Yb := bytes.Repeat([]byte{0xBB}, 32)

	clientTag := computeCPaceConfirm(kConfirm, confirmTagClient, Ya, Yb)
	serverTag := computeCPaceConfirm(kConfirm, confirmTagServer, Ya, Yb)

	if bytes.Equal(clientTag, serverTag) {
		t.Error("client and server confirmation tags should differ")
	}
}

func TestMakeNonce(t *testing.T) {
	nonce1 := makeNonce(0)
	nonce2 := makeNonce(1)

	if len(nonce1) != gcmNonceSize {
		t.Errorf("nonce size = %d, want %d", len(nonce1), gcmNonceSize)
	}
	if bytes.Equal(nonce1, nonce2) {
		t.Error("different counters produced same nonce")
	}
}

func TestDeriveCPaceKey(t *testing.T) {
	K := bytes.Repeat([]byte{0xAA}, 32)
	salt := []byte("test-salt")

	k1 := deriveCPaceKey(K, salt)
	k2 := deriveCPaceKey(K, salt)

	if len(k1) != 32 {
		t.Errorf("length = %d, want 32", len(k1))
	}
	if !bytes.Equal(k1, k2) {
		t.Error("key derivation not deterministic")
	}

	k3 := deriveCPaceKey(bytes.Repeat([]byte{0xBB}, 32), salt)
	if bytes.Equal(k1, k3) {
		t.Error("different K should produce different keys")
	}
}
