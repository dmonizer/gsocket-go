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

func TestHandshakeBothServers(t *testing.T) {
	a, b := connectPipes()
	secret := "test-secret"

	var serverErr, clientErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, serverErr = Handshake(a, secret, true)
	}()
	go func() {
		defer wg.Done()
		_, clientErr = Handshake(b, secret, true)
	}()
	wg.Wait()

	if serverErr == nil && clientErr == nil {
		t.Error("expected failure when both peers act as server")
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

func TestComputeAuthTag(t *testing.T) {
	baseKey := []byte("test-base-key-32-bytes-long!!")
	pubKey := bytes.Repeat([]byte{0x42}, x25519KeySize)

	tag1 := computeAuthTag(baseKey, pubKey, authDomainServer)
	tag2 := computeAuthTag(baseKey, pubKey, authDomainClient)

	if len(tag1) != authTagSize {
		t.Errorf("auth tag size = %d, want %d", len(tag1), authTagSize)
	}
	if bytes.Equal(tag1, tag2) {
		t.Error("server and client auth tags should differ")
	}

	tag1Again := computeAuthTag(baseKey, pubKey, authDomainServer)
	if !bytes.Equal(tag1, tag1Again) {
		t.Error("auth tag not deterministic")
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

func TestDeriveSessionKey(t *testing.T) {
	baseKey := bytes.Repeat([]byte{0x01}, 32)
	ecdhSecret := bytes.Repeat([]byte{0x02}, 32)

	key1 := deriveSessionKey(baseKey, ecdhSecret)
	key2 := deriveSessionKey(baseKey, ecdhSecret)

	if len(key1) != aes256KeySize {
		t.Errorf("session key size = %d, want %d", len(key1), aes256KeySize)
	}
	if !bytes.Equal(key1, key2) {
		t.Error("session key derivation not deterministic")
	}

	key3 := deriveSessionKey(baseKey, bytes.Repeat([]byte{0x03}, 32))
	if bytes.Equal(key1, key3) {
		t.Error("different ECDH secrets produced same session key")
	}
}
