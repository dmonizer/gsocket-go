package gsocket

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// GSRN protocol constants — match the C implementation exactly.
const (
	// Default GSRN ports.
	gsrnDefaultPort    = 443
	gsrnDefaultPortAlt = 7351

	// Protocol version.
	protoVersionMajor = 1
	protoVersionMinor = 3

	// Packet types.
	pktTypeListen = 0x01 // LC→GN: register address for listening
	pktTypeConnect = 0x02 // CC→GN: connect to a listening address
	pktTypePing    = 0x03 // all→GN: keepalive
	pktTypePong    = 0x04 // GN→all: keepalive reply
	pktTypeStart   = 0x05 // GN→all: new incoming connection
	pktTypeAccept  = 0x06 // all→GN: accepting connection
	pktTypeStatus  = 0x07 // GN→all: error/status

	// Protocol flags for _gs_connect.flags.
	flagProtoWait          = 0x01 // wait for server to become available
	flagProtoClientOrServer = 0x02 // allow client to become server if none exists
	flagProtoFastConnect   = 0x04 // skip waiting for _gs_start, data follows immediately
	flagProtoLowLatency    = 0x08 // prefer low-latency relay path
	flagProtoServerCheck   = 0x10 // check if server is listening

	// Start flags.
	flagStartServer = 0x01 // this peer acts as TLS server
	flagStartClient = 0x02 // this peer acts as TLS client

	// Status error types.
	statusTypeWarn  = 0x01
	statusTypeFatal = 0x02

	// Status codes.
	statusCodeBadAuth     = 0x01
	statusCodeConnRefused = 0x02
	statusCodeIdleTimeout = 0x03
	statusCodeConnDenied  = 0x04
	statusCodeProtoError  = 0x05
	statusCodeServerOK    = 0x06
	statusCodeNetError    = 0x07
	statusCodeNeedUpdate  = 0x2a

	// Keepalive interval: send ping every 45 seconds to prevent idle timeout.
	defaultPingInterval = 45 * time.Second

	// Packet sizes.
	gsListenSize  = 128
	gsConnectSize = 128
	gsPingSize    = 32
	gsPongSize    = 32
	gsStartSize   = 32
	gsAcceptSize  = 32
	gsStatusSize  = 32

	// Max message across all packet types.
	gsMaxMsgLen = 128
)

// Protocol errors.
var (
	ErrGSRNAuthFailed    = errors.New("gsocket: address already in use (bad auth)")
	ErrGSRNConnRefused   = errors.New("gsocket: no server listening")
	ErrGSRNIdleTimeout   = errors.New("gsocket: idle timeout")
	ErrGSRNConnDenied    = errors.New("gsocket: connection denied")
	ErrGSRNProtoError    = errors.New("gsocket: protocol error")
	ErrGSRNNetError      = errors.New("gsocket: network error")
	ErrGSRNNeedUpdate    = errors.New("gsocket: client needs update")
	ErrGSRNAuthFailedStr = "address already in use"
	ErrGSRNConnRefusedStr = "connection refused (no server listening)"
)

// raw packet structs — must match C layout exactly.

type gsListenPacket struct {
	Type         uint8
	VersionMajor uint8
	VersionMinor uint8
	Flags        uint8
	Reserved1    [4]uint8
	Reserved2    [8]uint8
	Token        [TokenSize]uint8
	Addr         [AddrSize]uint8
	Reserved3    [16]uint8
	Reserved4    [64]uint8
}

type gsConnectPacket struct {
	Type         uint8
	VersionMajor uint8
	VersionMinor uint8
	Flags        uint8
	Reserved1    [4]uint8
	Reserved2    [8]uint8
	Unused       [TokenSize]uint8
	Addr         [AddrSize]uint8
	Reserved3    [16]uint8
	Reserved4    [64]uint8
}

type gsPingPacket struct {
	Type     uint8
	Reserved [3]uint8
	Payload  [28]uint8
}

type gsPongPacket struct {
	Type     uint8
	Reserved [3]uint8
	Payload  [28]uint8
}

type gsStartPacket struct {
	Type     uint8
	Flags    uint8
	Reserved [2]uint8
	Padding  [28]uint8
}

type gsAcceptPacket struct {
	Type     uint8
	Reserved [3]uint8
	Padding  [28]uint8
}

type gsStatusPacket struct {
	Type    uint8
	ErrType uint8
	Code    uint8
	_       [1]uint8
	Msg     [28]uint8
}

// readFullPacket reads exactly size bytes from r into a buffer.
func readFullPacket(r io.Reader, size int) ([]byte, error) {
	buf := make([]byte, size)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

// sendPacket marshals a struct to binary and writes it to w.
func sendPacket(w io.Writer, pkt interface{}) error {
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, pkt); err != nil {
		return fmt.Errorf("marshal packet: %w", err)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// GSRNConn represents a single TCP connection to the GSRN relay.
// It handles the low-level GSRN binary protocol (listen, connect, ping, start, etc.)
// but does NOT handle encryption — that's done at the SecureChannel layer above.
type GSRNConn struct {
	conn      net.Conn
	addr      Addr
	token     [TokenSize]byte
	flags     uint8
	isClient  bool
	isServer  bool

	mu         sync.Mutex
	closed     bool
	lastPing   time.Time
	pingTicker *time.Ticker
	done       chan struct{}
}

// NewGSRNConn wraps an existing net.Conn with the GSRN protocol handler.
func NewGSRNConn(conn net.Conn, addr Addr, token [TokenSize]byte, flags uint8, isClient bool) *GSRNConn {
	return &GSRNConn{
		conn:     conn,
		addr:     addr,
		token:    token,
		flags:    flags,
		isClient: isClient,
		done:     make(chan struct{}),
	}
}

// SendListen sends a listen registration packet to the GSRN.
// Called by the listening peer after connecting to the relay.
func (g *GSRNConn) SendListen() error {
	pkt := gsListenPacket{
		Type:         pktTypeListen,
		VersionMajor: protoVersionMajor,
		VersionMinor: protoVersionMinor,
	}
	copy(pkt.Token[:], g.token[:])
	copy(pkt.Addr[:], g.addr[:])
	return sendPacket(g.conn, &pkt)
}

// SendConnect sends a connect request to the GSRN.
// Called by the connecting peer after connecting to the relay.
func (g *GSRNConn) SendConnect() error {
	pkt := gsConnectPacket{
		Type:         pktTypeConnect,
		VersionMajor: protoVersionMajor,
		VersionMinor: protoVersionMinor,
		Flags:        g.flags,
	}
	copy(pkt.Addr[:], g.addr[:])
	return sendPacket(g.conn, &pkt)
}

// SendPing sends a keepalive ping to the GSRN.
func (g *GSRNConn) SendPing() error {
	pkt := gsPingPacket{Type: pktTypePing}
	return sendPacket(g.conn, &pkt)
}

// SendAccept sends an accept confirmation to the GSRN.
// Called after receiving a _gs_start to acknowledge the connection.
func (g *GSRNConn) SendAccept() error {
	pkt := gsAcceptPacket{Type: pktTypeAccept}
	return sendPacket(g.conn, &pkt)
}

// StartKeepalive begins sending periodic pings to keep the GSRN connection alive.
func (g *GSRNConn) StartKeepalive() {
	g.pingTicker = time.NewTicker(defaultPingInterval)
	go func() {
		for {
			select {
			case <-g.pingTicker.C:
				g.mu.Lock()
				if g.closed {
					g.mu.Unlock()
					return
				}
				g.mu.Unlock()
				_ = g.SendPing()
			case <-g.done:
				return
			}
		}
	}()
}

// StopKeepalive stops the periodic ping loop.
func (g *GSRNConn) StopKeepalive() {
	if g.pingTicker != nil {
		g.pingTicker.Stop()
	}
	close(g.done)
}

// ReadPacket reads and dispatches the next GSRN protocol packet.
// Returns the packet type and raw payload for further processing.
func (g *GSRNConn) ReadPacket() (pktType uint8, payload []byte, err error) {
	// Read first byte to determine packet type.
	typeByte := make([]byte, 1)
	if _, err = io.ReadFull(g.conn, typeByte); err != nil {
		return 0, nil, err
	}
	pktType = typeByte[0]

	var size int
	switch pktType {
	case pktTypePong:
		size = gsPongSize
	case pktTypeStart:
		size = gsStartSize
	case pktTypeStatus:
		size = gsStatusSize
	default:
		// Unknown packet type — read remaining 31 bytes (minimum packet is 32).
		size = gsPongSize
	}

	// Read the rest of the packet.
	rest := make([]byte, size-1)
	if _, err = io.ReadFull(g.conn, rest); err != nil {
		return 0, nil, err
	}

	payload = append(typeByte, rest...)
	return pktType, payload, nil
}

// ParseStart parses a _gs_start packet and returns whether this peer
// should act as the server.
func ParseStart(payload []byte) (actAsServer bool, err error) {
	if len(payload) < 2 {
		return false, fmt.Errorf("start packet too short: %d bytes", len(payload))
	}
	if payload[0] != pktTypeStart {
		return false, fmt.Errorf("expected START packet, got type 0x%02x", payload[0])
	}
	flags := payload[1]
	switch {
	case flags&flagStartServer != 0:
		return true, nil
	case flags&flagStartClient != 0:
		return false, nil
	default:
		return false, fmt.Errorf("START packet has neither server nor client flag set")
	}
}

// ParseStatus parses a _gs_status packet and returns the error.
func ParseStatus(payload []byte) error {
	if len(payload) < 4 {
		return fmt.Errorf("status packet too short: %d bytes", len(payload))
	}
	errType := payload[1]
	code := payload[2]
	msg := string(bytes.TrimRight(payload[4:], "\x00"))

	baseErr := statusCodeToError(code, msg)
	if errType == statusTypeFatal {
		return baseErr
	}
	// Non-fatal status — log it but don't treat as error.
	return nil
}

func statusCodeToError(code uint8, msg string) error {
	switch code {
	case statusCodeBadAuth:
		return ErrGSRNAuthFailed
	case statusCodeConnRefused:
		return ErrGSRNConnRefused
	case statusCodeIdleTimeout:
		return ErrGSRNIdleTimeout
	case statusCodeConnDenied:
		return ErrGSRNConnDenied
	case statusCodeProtoError:
		return ErrGSRNProtoError
	case statusCodeNetError:
		return ErrGSRNNetError
	case statusCodeNeedUpdate:
		return ErrGSRNNeedUpdate
	case statusCodeServerOK:
		return nil // not an error, server exists
	default:
		if msg != "" {
			return fmt.Errorf("gsocket: GSRN error (code=%d): %s", code, msg)
		}
		return fmt.Errorf("gsocket: unknown GSRN error (code=%d)", code)
	}
}

// Close closes the GSRN connection.
func (g *GSRNConn) Close() error {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.StopKeepalive()
	return g.conn.Close()
}

// RawConn returns the underlying net.Conn.
// After the GSRN handshake completes (_gs_start received), the caller
// takes ownership of the raw connection for the encrypted channel.
func (g *GSRNConn) RawConn() net.Conn {
	return g.conn
}

// GSRNClient handles the full lifecycle of connecting to the GSRN
// and completing the relay handshake.
type GSRNClient struct {
	gsrnHost string
	gsrnPort int
	addr     Addr
	token    [TokenSize]byte
	flags    uint8
	socksAddr string // optional SOCKS5 proxy address

	// Connection state
	gsrnConn *GSRNConn
}

// GSRNClientOption configures a GSRNClient.
type GSRNClientOption func(*GSRNClient)

// WithSOCKS5 sets a SOCKS5 proxy address (e.g. "127.0.0.1:9050" for TOR).
func WithSOCKS5(addr string) GSRNClientOption {
	return func(c *GSRNClient) {
		c.socksAddr = addr
	}
}

// WithFlags sets protocol flags for the connect packet.
func WithFlags(flags uint8) GSRNClientOption {
	return func(c *GSRNClient) {
		c.flags = flags
	}
}

// NewGSRNClient creates a new GSRN client for the given address and secret.
func NewGSRNClient(secret string, opts ...GSRNClientOption) (*GSRNClient, error) {
	_, addr := DeriveKeyMaterial(secret)

	c := &GSRNClient{
		addr:     addr,
		gsrnPort: gsrnDefaultPort,
		flags:    flagProtoLowLatency,
	}
	for _, opt := range opts {
		opt(c)
	}

	// Resolve GSRN hostname.
	c.gsrnHost = addr.GSRNHostname()

	return c, nil
}

// ConnectListener connects to the GSRN and sends a listen packet.
// Returns the raw connection after successful registration.
func (c *GSRNClient) ConnectListener() (*GSRNConn, error) {
	conn, err := c.dialGSRN()
	if err != nil {
		return nil, fmt.Errorf("connect to GSRN: %w", err)
	}

	// Generate random token for this listening session.
	token, err := generateRandomToken()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("generate token: %w", err)
	}
	c.token = token

	gsrn := NewGSRNConn(conn, c.addr, c.token, c.flags, false)
	if err := gsrn.SendListen(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send listen: %w", err)
	}

	gsrn.StartKeepalive()
	c.gsrnConn = gsrn
	return gsrn, nil
}

// ConnectClient connects to the GSRN and sends a connect packet.
// Waits for the _gs_start response, then returns the raw connection.
func (c *GSRNClient) ConnectClient() (*GSRNConn, error) {
	conn, err := c.dialGSRN()
	if err != nil {
		return nil, fmt.Errorf("connect to GSRN: %w", err)
	}

	gsrn := NewGSRNConn(conn, c.addr, c.token, c.flags, true)
	if err := gsrn.SendConnect(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send connect: %w", err)
	}

	// Wait for START or STATUS from GSRN.
	pktType, payload, err := gsrn.ReadPacket()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read GSRN response: %w", err)
	}

	switch pktType {
	case pktTypeStart:
		// Send accept.
		if err := gsrn.SendAccept(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("send accept: %w", err)
		}
	case pktTypeStatus:
		if err := ParseStatus(payload); err != nil {
			conn.Close()
			return nil, err
		}
	default:
		conn.Close()
		return nil, fmt.Errorf("unexpected GSRN packet type: 0x%02x", pktType)
	}

	gsrn.StartKeepalive()
	c.gsrnConn = gsrn
	return gsrn, nil
}

// WaitForClient blocks until a client connects to this listening GSRN endpoint.
// Returns the raw connection after _gs_start is received and accepted.
func (c *GSRNClient) WaitForClient(gsrn *GSRNConn) (*GSRNConn, error) {
	pktType, payload, err := gsrn.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("wait for client: %w", err)
	}

	switch pktType {
	case pktTypeStart:
		if err := gsrn.SendAccept(); err != nil {
			return nil, fmt.Errorf("send accept: %w", err)
		}
		_, err := ParseStart(payload)
		if err != nil {
			return nil, err
		}
		return gsrn, nil
	case pktTypeStatus:
		if err := ParseStatus(payload); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected status while waiting for client")
	default:
		return nil, fmt.Errorf("unexpected packet type while waiting: 0x%02x", pktType)
	}
}

// dialGSRN establishes a TCP connection to the GSRN relay.
func (c *GSRNClient) dialGSRN() (net.Conn, error) {
	addr := net.JoinHostPort(c.gsrnHost, fmt.Sprintf("%d", c.gsrnPort))

	if c.socksAddr != "" {
		return dialSOCKS5(c.socksAddr, addr)
	}

	// Try port 443 first, then fall back to 7351.
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		altAddr := net.JoinHostPort(c.gsrnHost, fmt.Sprintf("%d", gsrnDefaultPortAlt))
		conn, err = net.DialTimeout("tcp", altAddr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", addr, err)
		}
	}
	return conn, nil
}

// generateRandomToken creates a random 16-byte token using crypto/rand.
func generateRandomToken() ([TokenSize]byte, error) {
	var token [TokenSize]byte
	if _, err := io.ReadFull(rand.Reader, token[:]); err != nil {
		return token, fmt.Errorf("read random bytes: %w", err)
	}
	return token, nil
}
