package gsocket

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// Secure channel constants.
const (
	x25519KeySize   = 32
	aes256KeySize   = 32
	gcmNonceSize    = 12
	authTagSize     = 16

	hkdfSaltSession = "gsocket-session"

	authDomainServer = "gsocket-auth-server"
	authDomainClient = "gsocket-auth-client"
)

var (
	ErrAuthFailed    = errors.New("gsocket: peer authentication failed")
	ErrChannelClosed = errors.New("gsocket: channel closed")
)

// SecureChannel wraps a raw TCP connection with authenticated encryption.
//
// After the GSRN handshake connects two peers, the raw connection is
// upgraded to a SecureChannel via the Handshake function, which performs
// an ECDH-X25519 key exchange authenticated by the shared secret.
// All subsequent data is encrypted with AES-256-GCM.
type SecureChannel struct {
	conn    io.ReadWriteCloser
	aesgcm  cipher.AEAD
	sendCtr uint64
	recvCtr uint64
	writeMu sync.Mutex
	closed  bool
}

// Handshake performs the authenticated ECDH key exchange over the given
// connection and returns a SecureChannel ready for encrypted communication.
func Handshake(conn io.ReadWriteCloser, secret string, isServer bool) (*SecureChannel, error) {
	srpPassword, _ := DeriveKeyMaterial(secret)
	baseKey := []byte(srpPassword)

	privKey, pubKey, err := generateX25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}

	ourTag := authDomainClient
	theirTag := authDomainServer
	if isServer {
		ourTag, theirTag = authDomainServer, authDomainClient
	}

	theirPub, err := exchangeKeys(conn, pubKey, baseKey, ourTag)
	if err != nil {
		return nil, fmt.Errorf("key exchange: %w", err)
	}

	if err := verifyPeerAuth(conn, theirPub, baseKey, theirTag); err != nil {
		return nil, err
	}

	ecdhSecret, err := curve25519.X25519(privKey, theirPub)
	if err != nil {
		return nil, fmt.Errorf("compute ECDH secret: %w", err)
	}

	sessionKey := deriveSessionKey(baseKey, ecdhSecret)

	aesgcm, err := newAESGCM(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	return &SecureChannel{conn: conn, aesgcm: aesgcm}, nil
}

// Write encrypts and sends data through the secure channel.
// Each call produces one framed and encrypted message.
// Frame format: [2-byte ciphertext length][ciphertext+16-byte-GCM-tag]
// Nonces are derived deterministically from a counter on each side.
func (sc *SecureChannel) Write(data []byte) (int, error) {
	sc.writeMu.Lock()
	defer sc.writeMu.Unlock()

	if sc.closed {
		return 0, ErrChannelClosed
	}

	nonce := makeNonce(sc.sendCtr)
	sc.sendCtr++

	ciphertext := sc.aesgcm.Seal(nil, nonce, data, nil)

	// Frame: [2-byte length][ciphertext+tag].
	frame := make([]byte, 2+len(ciphertext))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(ciphertext)))
	copy(frame[2:], ciphertext)

	if _, err := sc.conn.Write(frame); err != nil {
		return 0, err
	}
	return len(data), nil
}

// Read decrypts the next framed message from the secure channel.
func (sc *SecureChannel) Read(buf []byte) (int, error) {
	if sc.closed {
		return 0, ErrChannelClosed
	}

	lenBuf := make([]byte, 2)
	if _, err := io.ReadFull(sc.conn, lenBuf); err != nil {
		return 0, err
	}
	frameLen := binary.BigEndian.Uint16(lenBuf)

	frame := make([]byte, frameLen)
	if _, err := io.ReadFull(sc.conn, frame); err != nil {
		return 0, err
	}

	nonce := makeNonce(sc.recvCtr)
	sc.recvCtr++

	plaintext, err := sc.aesgcm.Open(nil, nonce, frame, nil)
	if err != nil {
		return 0, fmt.Errorf("decrypt: %w", err)
	}

	return copy(buf, plaintext), nil
}

// Close shuts down the secure channel and the underlying connection.
func (sc *SecureChannel) Close() error {
	sc.closed = true
	return sc.conn.Close()
}

// --- internal helpers ---

func generateX25519Keypair() (privKey, pubKey []byte, err error) {
	privKey = make([]byte, x25519KeySize)
	if _, err := io.ReadFull(rand.Reader, privKey); err != nil {
		return nil, nil, fmt.Errorf("generate private key: %w", err)
	}
	pubKey, err = curve25519.X25519(privKey, curve25519.Basepoint)
	if err != nil {
		return nil, nil, fmt.Errorf("compute public key: %w", err)
	}
	return
}

func exchangeKeys(conn io.ReadWriter, ourPub, baseKey []byte, ourTag string) ([]byte, error) {
	authTag := computeAuthTag(baseKey, ourPub, ourTag)

	msg := make([]byte, x25519KeySize+authTagSize)
	copy(msg[:x25519KeySize], ourPub)
	copy(msg[x25519KeySize:], authTag)

	if _, err := conn.Write(msg); err != nil {
		return nil, fmt.Errorf("send key exchange: %w", err)
	}

	theirPub := make([]byte, x25519KeySize)
	if _, err := io.ReadFull(conn, theirPub); err != nil {
		return nil, fmt.Errorf("read peer public key: %w", err)
	}
	return theirPub, nil
}

func verifyPeerAuth(conn io.Reader, theirPub, baseKey []byte, theirTag string) error {
	receivedTag := make([]byte, authTagSize)
	if _, err := io.ReadFull(conn, receivedTag); err != nil {
		return fmt.Errorf("read peer auth tag: %w", err)
	}

	expected := computeAuthTag(baseKey, theirPub, theirTag)
	if !hmac.Equal(receivedTag, expected) {
		return ErrAuthFailed
	}
	return nil
}

func computeAuthTag(baseKey, pubKey []byte, domainTag string) []byte {
	mac := hmac.New(sha256.New, baseKey)
	mac.Write(pubKey)
	mac.Write([]byte(domainTag))
	return mac.Sum(nil)[:authTagSize]
}

func deriveSessionKey(baseKey, ecdhSecret []byte) []byte {
	combined := make([]byte, len(baseKey)+len(ecdhSecret))
	copy(combined, baseKey)
	copy(combined[len(baseKey):], ecdhSecret)

	r := hkdf.New(sha256.New, combined, []byte(hkdfSaltSession), nil)
	key := make([]byte, aes256KeySize)
	if _, err := io.ReadFull(r, key); err != nil {
		panic("hkdf: " + err.Error())
	}
	return key
}

func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func makeNonce(counter uint64) []byte {
	nonce := make([]byte, gcmNonceSize)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}
