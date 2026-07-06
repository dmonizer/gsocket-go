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
	x25519KeySize = 32
	aes256KeySize = 32
	gcmNonceSize  = 12
	authTagSize   = 16

	// CPace protocol identifiers (RFC 9383).
	cpaceSID            = "gsocket-cpace-v1"
	hkdfSaltConfirm     = "gsocket-cpace-confirm"
	hkdfSaltSessionCP   = "gsocket-cpace-session"

	// Confirmation domain tags prevent reflection attacks.
	confirmTagClient = "gsocket-cpace-client-confirm"
	confirmTagServer = "gsocket-cpace-server-confirm"
)

var (
	ErrAuthFailed    = errors.New("gsocket: peer authentication failed")
	ErrChannelClosed = errors.New("gsocket: channel closed")
)

// SecureChannel wraps a raw TCP connection with authenticated encryption.
//
// After the GSRN handshake connects two peers, the raw connection is
// upgraded to a SecureChannel via the Handshake function, which performs
// CPace (RFC 9383) — a balanced Password-Authenticated Key Exchange over
// X25519. All subsequent data is encrypted with AES-256-GCM.
type SecureChannel struct {
	conn    io.ReadWriteCloser
	aesgcm  cipher.AEAD
	sendCtr uint64
	recvCtr uint64
	writeMu sync.Mutex
	closed  bool
}

// Handshake performs a CPace (RFC 9383) balanced Password-Authenticated
// Key Exchange over X25519, then returns a SecureChannel for AES-256-GCM
// encrypted communication.
//
// Unlike the previous HMAC-auth exchange, CPace makes the Diffie-Hellman
// base point password-dependent. An eavesdropper cannot verify password
// guesses against the observed confirmation tags without solving a fresh
// Computational Diffie-Hellman problem per guess.
func Handshake(conn io.ReadWriteCloser, secret string, isServer bool) (*SecureChannel, error) {
	// Derive raw base key (32 bytes).
	baseKey := DeriveBaseKey(secret)

	// Generate ephemeral blinding keypair.
	ya, Ya, err := generateX25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}

	// Compute password-dependent scalar d.
	d := deriveCPaceScalar(baseKey)

	// ---- Round 1: exchange blinding keys ----
	var Yb []byte
	if isServer {
		Yb, err = receivePublicKey(conn)
		if err != nil {
			return nil, fmt.Errorf("receive peer blinding key: %w", err)
		}
		if err = sendPublicKey(conn, Ya); err != nil {
			return nil, fmt.Errorf("send blinding key: %w", err)
		}
	} else {
		if err = sendPublicKey(conn, Ya); err != nil {
			return nil, fmt.Errorf("send blinding key: %w", err)
		}
		Yb, err = receivePublicKey(conn)
		if err != nil {
			return nil, fmt.Errorf("receive peer blinding key: %w", err)
		}
	}

	// ---- Compute CPace shared secret ----
	// K = X25519(ya, X25519(d, Yb))
	dY, err := curve25519.X25519(d, Yb)
	if err != nil {
		return nil, fmt.Errorf("cpace: X25519(d, Yb): %w", err)
	}
	K, err := curve25519.X25519(ya, dY)
	if err != nil {
		return nil, fmt.Errorf("cpace: X25519(ya, dY): %w", err)
	}

	// ---- Derive session and confirmation keys ----
	kConfirm := deriveCPaceKey(K, []byte(hkdfSaltConfirm))
	sessionKey := deriveCPaceKey(K, []byte(hkdfSaltSessionCP))

	// ---- Round 2: exchange confirmations ----
	var clientPub, serverPub []byte
	if isServer {
		clientPub, serverPub = Yb, Ya
	} else {
		clientPub, serverPub = Ya, Yb
	}

	ourConfirm := computeCPaceConfirm(kConfirm, confirmTagClient, clientPub, serverPub)
	theirConfirmTag := confirmTagServer
	if isServer {
		ourConfirm = computeCPaceConfirm(kConfirm, confirmTagServer, clientPub, serverPub)
		theirConfirmTag = confirmTagClient
	}

	if isServer {
		// Server: receive confirmation first, then send.
		if err = verifyCPaceConfirm(conn, kConfirm, theirConfirmTag, clientPub, serverPub); err != nil {
			return nil, err
		}
		if err = sendPublicKey(conn, ourConfirm); err != nil {
			return nil, fmt.Errorf("send confirmation: %w", err)
		}
	} else {
		// Client: send confirmation first, then receive.
		if err = sendPublicKey(conn, ourConfirm); err != nil {
			return nil, fmt.Errorf("send confirmation: %w", err)
		}
		if err = verifyCPaceConfirm(conn, kConfirm, theirConfirmTag, clientPub, serverPub); err != nil {
			return nil, err
		}
	}

	// ---- Create cipher ----
	aesgcm, err := newAESGCM(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	return &SecureChannel{conn: conn, aesgcm: aesgcm}, nil
}

// generateX25519Keypair generates an ephemeral Curve25519 keypair.
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

// deriveCPaceScalar computes the password-dependent CPace scalar d.
// d = clamp(SHA256("gsocket-cpace-v1" || baseKey))
func deriveCPaceScalar(baseKey []byte) []byte {
	h := sha256.New()
	h.Write([]byte(cpaceSID))
	h.Write(baseKey)
	md := h.Sum(nil)
	return clampX25519Scalar(md)
}

// clampX25519Scalar applies Curve25519 scalar clamping:
// clear bits 0,1,2; set bit 254; clear bit 255.
func clampX25519Scalar(s []byte) []byte {
	c := make([]byte, 32)
	copy(c, s)
	c[0] &= 248   // clear low 3 bits
	c[31] &= 127  // clear high bit (255)
	c[31] |= 64   // set bit 254
	return c
}

// deriveCPaceKey derives a sub-key from the CPace shared secret K.
func deriveCPaceKey(K, salt []byte) []byte {
	r := hkdf.New(sha256.New, K, salt, nil)
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		panic("hkdf: " + err.Error())
	}
	return key
}

// computeCPaceConfirm computes a confirmation tag.
func computeCPaceConfirm(kConfirm []byte, domainTag string, Ya, Yb []byte) []byte {
	mac := hmac.New(sha256.New, kConfirm)
	mac.Write([]byte(domainTag))
	mac.Write(Ya)
	mac.Write(Yb)
	return mac.Sum(nil)[:authTagSize]
}

// sendPublicKey sends a 32-byte value over the connection.
func sendPublicKey(conn io.Writer, key []byte) error {
	_, err := conn.Write(key)
	return err
}

// receivePublicKey reads a 32-byte value from the connection.
func receivePublicKey(conn io.Reader) ([]byte, error) {
	key := make([]byte, x25519KeySize)
	if _, err := io.ReadFull(conn, key); err != nil {
		return nil, err
	}
	return key, nil
}

// verifyCPaceConfirm reads and verifies a peer's confirmation tag.
// Closes the connection on failure to unblock the peer.
func verifyCPaceConfirm(conn io.ReadCloser, kConfirm []byte, domainTag string, Ya, Yb []byte) error {
	received := make([]byte, authTagSize)
	if _, err := io.ReadFull(conn, received); err != nil {
		return fmt.Errorf("read peer confirmation: %w", err)
	}
	expected := computeCPaceConfirm(kConfirm, domainTag, Ya, Yb)
	if !hmac.Equal(received, expected) {
		conn.Close()
		return ErrAuthFailed
	}
	return nil
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
