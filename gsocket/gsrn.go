package gsocket

import (
	"bytes"
	"context"
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

// sendPacket marshals a struct to binary and writes it to w.
func sendPacket(w io.Writer, pkt any) error {
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
	stopOnce   sync.Once // guards StopKeepalive against double-close
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

// StopKeepalive stops the periodic ping loop. Safe to call multiple times.
func (g *GSRNConn) StopKeepalive() {
	g.stopOnce.Do(func() {
		if g.pingTicker != nil {
			g.pingTicker.Stop()
		}
		close(g.done)
	})
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
	gsrnHosts  []string // all 26 GSRN hosts to try (primary first)
	gsrnPort   int
	addr       Addr
	token      [TokenSize]byte
	flags      uint8
	socksAddr  string // optional SOCKS5 proxy address
	verboseLog func(format string, args ...any)
	ctx        context.Context // cancellation context for dial

	// Connection state
	gsrnConn *GSRNConn
}

// PrimaryHost returns the first (preferred) GSRN hostname.
func (c *GSRNClient) PrimaryHost() string {
	if len(c.gsrnHosts) > 0 {
		return c.gsrnHosts[0]
	}
	return ""
}

// GSRNClientOption configures a GSRNClient.
type GSRNClientOption func(*GSRNClient)

// WithVerboseLog enables verbose logging for GSRN connection steps.
func WithVerboseLog(logf func(format string, args ...any)) GSRNClientOption {
	return func(c *GSRNClient) { c.verboseLog = logf }
}

// WithSOCKS5 sets a SOCKS5 proxy address (e.g. "127.0.0.1:9050" for TOR).
func WithSOCKS5(addr string) GSRNClientOption {
	return func(c *GSRNClient) {
		c.socksAddr = addr
	}
}

// WithContext sets the cancellation context for GSRN dial attempts.
// When the context is cancelled, dialGSRN stops trying remaining hosts.
func WithContext(ctx context.Context) GSRNClientOption {
	return func(c *GSRNClient) { c.ctx = ctx }
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
		ctx:      context.Background(),
	}
	for _, opt := range opts {
		opt(c)
	}

	// Resolve GSRN hostname list (26 hosts, primary first).
	c.gsrnHosts = addr.GSRNHostnames()

	return c, nil
}

// ConnectListener connects to the GSRN and sends a listen packet.
// Returns the raw connection after successful registration.
// If the client already has a token (set via SetToken or a previous
// ConnectListener call), it reuses that token. Otherwise a new random
// token is generated. Reusing the same token allows multiple concurrent
// listen connections (matching C's multi-sox behaviour).
func (c *GSRNClient) ConnectListener() (*GSRNConn, error) {
	conn, err := c.dialGSRN()
	if err != nil {
		return nil, fmt.Errorf("connect to GSRN: %w", err)
	}

	// Generate random token if not already set (first registration).
	if c.token == [TokenSize]byte{} {
		token, err := generateRandomToken()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("generate token: %w", err)
		}
		c.token = token
	}
	if c.verboseLog != nil {
		c.verboseLog("sending LISTEN addr=%s token=%x", c.addr, c.token[:4])
	}

	gsrn := NewGSRNConn(conn, c.addr, c.token, c.flags, false)
	if err := gsrn.SendListen(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send listen: %w", err)
	}

	if c.verboseLog != nil {
		c.verboseLog("LISTEN sent — starting keepalive (45s interval)")
	}
	gsrn.StartKeepalive()
	c.gsrnConn = gsrn
	return gsrn, nil
}

// SetToken sets the GSRN protocol token to use for listen registrations.
// Call this before ConnectListener to reuse a token across multiple
// connections (C's multi-sox pattern).
func (c *GSRNClient) SetToken(token [TokenSize]byte) {
	c.token = token
}

// Token returns the current GSRN protocol token.
func (c *GSRNClient) Token() [TokenSize]byte {
	return c.token
}

// ConnectClient connects to the GSRN and sends a connect packet.
// Waits for the _gs_start response, then returns the raw connection.
func (c *GSRNClient) ConnectClient() (*GSRNConn, error) {
	conn, err := c.dialGSRN()
	if err != nil {
		return nil, fmt.Errorf("connect to GSRN: %w", err)
	}

	if c.verboseLog != nil {
		c.verboseLog("sending CONNECT addr=%s flags=0x%02x", c.addr, c.flags)
	}

	gsrn := NewGSRNConn(conn, c.addr, c.token, c.flags, true)
	if err := gsrn.SendConnect(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send connect: %w", err)
	}

	// Wait for START or STATUS from GSRN.
	if c.verboseLog != nil {
		c.verboseLog("waiting for GSRN response (START or STATUS)...")
	}
	pktType, payload, err := gsrn.ReadPacket()
	if err != nil {
		conn.Close()
		if c.verboseLog != nil {
			c.verboseLog("GSRN read error: %v", err)
		}
		return nil, fmt.Errorf("read GSRN response: %w", err)
	}
	if c.verboseLog != nil {
		c.verboseLog("received GSRN packet type=0x%02x (%d bytes)", pktType, len(payload))
	}

	switch pktType {
	case pktTypeStart:
		if c.verboseLog != nil {
			c.verboseLog("GSRN START — server found, sending ACCEPT")
		}
		// Send accept.
		if err := gsrn.SendAccept(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("send accept: %w", err)
		}
	case pktTypeStatus:
		if c.verboseLog != nil {
			errType := payload[1]
			code := payload[2]
			c.verboseLog("GSRN STATUS errType=%d code=%d", errType, code)
		}
		if err := ParseStatus(payload); err != nil {
			conn.Close()
			if c.verboseLog != nil {
				c.verboseLog("GSRN STATUS is fatal: %v", err)
			}
			return nil, err
		}
		// Non-fatal status — e.g. server exists but not ready yet.
		if c.verboseLog != nil {
			c.verboseLog("GSRN STATUS non-fatal — proceeding")
		}
	default:
		conn.Close()
		if c.verboseLog != nil {
			c.verboseLog("unexpected GSRN packet type: 0x%02x", pktType)
		}
		return nil, fmt.Errorf("unexpected GSRN packet type: 0x%02x", pktType)
	}

	gsrn.StartKeepalive()
	c.gsrnConn = gsrn
	return gsrn, nil
}

// WaitForClient blocks until a client connects to this listening GSRN endpoint.
// Returns the raw connection after _gs_start is received and accepted.
// PONG (keepalive reply) packets are consumed and discarded.
func (c *GSRNClient) WaitForClient(gsrn *GSRNConn) (*GSRNConn, error) {
	if c.verboseLog != nil {
		c.verboseLog("waiting for client START on addr=%s...", c.addr)
	}
	for {
		pktType, payload, err := gsrn.ReadPacket()
		if err != nil {
			if c.verboseLog != nil {
				c.verboseLog("read error while waiting for client: %v", err)
			}
			return nil, fmt.Errorf("wait for client: %w", err)
		}
		if c.verboseLog != nil {
			c.verboseLog("received packet type=0x%02x while waiting for client", pktType)
		}

		switch pktType {
		case pktTypePong:
			// Keepalive reply from GSRN — consume and continue waiting.
			if c.verboseLog != nil {
				c.verboseLog("consumed keepalive PONG, continuing to wait")
			}
			continue
		case pktTypeStart:
			if c.verboseLog != nil {
				c.verboseLog("GSRN START — client connected, sending ACCEPT")
			}
			if err := gsrn.SendAccept(); err != nil {
				return nil, fmt.Errorf("send accept: %w", err)
			}
			_, err := ParseStart(payload)
			if err != nil {
				return nil, err
			}
			return gsrn, nil
		case pktTypeStatus:
			if c.verboseLog != nil {
				errType := payload[1]
				code := payload[2]
				c.verboseLog("GSRN STATUS errType=%d code=%d while waiting for client", errType, code)
			}
			if err := ParseStatus(payload); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("unexpected status while waiting for client")
		default:
			return nil, fmt.Errorf("unexpected packet type while waiting: 0x%02x", pktType)
		}
	}
}

// dialGSRN establishes a TCP connection to the GSRN relay.
// It tries each GSRN hostname in sequence (a-z.gs.thc.org), starting from
// the primary host derived from the address. For each host, it tries port
// 443 first, then 7351. This provides resilience when a specific GSRN
// relay node is unreachable.
func (c *GSRNClient) dialGSRN() (net.Conn, error) {
	if c.socksAddr != "" {
		addr := net.JoinHostPort(c.PrimaryHost(), fmt.Sprintf("%d", c.gsrnPort))
		if c.verboseLog != nil {
			c.verboseLog("dialing GSRN via SOCKS5 proxy %s → %s", c.socksAddr, addr)
		}
		start := time.Now()
		conn, err := dialSOCKS5(c.socksAddr, addr)
		if c.verboseLog != nil {
			c.verboseLog("SOCKS5 dial %s → %s: err=%v (%v)", c.socksAddr, addr, err, time.Since(start).Round(time.Millisecond))
		}
		return conn, err
	}

	ports := []int{gsrnDefaultPort, gsrnDefaultPortAlt}
	var lastErr error

	for _, host := range c.gsrnHosts {
		// Check for cancellation between hostname attempts so Ctrl-C
		// doesn't leave the user waiting through 26 × 10s timeouts.
		if err := c.ctx.Err(); err != nil {
			if c.verboseLog != nil {
				c.verboseLog("dial cancelled: %v", err)
			}
			return nil, err
		}
		for _, port := range ports {
			addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
			start := time.Now()
			if c.verboseLog != nil {
				c.verboseLog("dialing GSRN tcp %s (timeout=10s)", addr)
			}
			dialer := net.Dialer{Timeout: 10 * time.Second}
			conn, err := dialer.DialContext(c.ctx, "tcp", addr)
			if err != nil {
				if c.verboseLog != nil {
					c.verboseLog("GSRN %s: %v (%v)", addr, err, time.Since(start).Round(time.Millisecond))
				}
				lastErr = err
				// If context was cancelled, stop trying more hosts.
				if c.ctx.Err() != nil {
					return nil, c.ctx.Err()
				}
				continue
			}
			if c.verboseLog != nil {
				c.verboseLog("GSRN TCP connected to %s (%v)", conn.RemoteAddr(), time.Since(start).Round(time.Millisecond))
			}
			return conn, nil
		}
	}
	// All 26 × 2 = 52 attempts failed.
	return nil, fmt.Errorf("dial %s: %w", c.PrimaryHost(), lastErr)
}

// ProbeServer checks whether a server is listening for the given secret.
// It connects to GSRN with flagProtoServerCheck, which tells the relay
// to report server presence without establishing a full connection.
// Returns nil if a server is listening, or an error describing the result.
func (c *GSRNClient) ProbeServer() error {
	conn, err := c.dialGSRN()
	if err != nil {
		return fmt.Errorf("connect to GSRN: %w", err)
	}
	defer conn.Close()

	if c.verboseLog != nil {
		c.verboseLog("sending CONNECT (server check) addr=%s", c.addr)
	}

	flags := c.flags | flagProtoServerCheck
	gsrn := NewGSRNConn(conn, c.addr, c.token, flags, true)
	if err := gsrn.SendConnect(); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}

	if c.verboseLog != nil {
		c.verboseLog("waiting for GSRN response (server check)...")
	}

	pktType, payload, err := gsrn.ReadPacket()
	if err != nil {
		return fmt.Errorf("read GSRN response: %w", err)
	}

	if c.verboseLog != nil {
		c.verboseLog("received GSRN packet type=0x%02x (%d bytes)", pktType, len(payload))
	}

	switch pktType {
	case pktTypeStatus:
		if len(payload) >= 4 && payload[2] == statusCodeServerOK {
			if c.verboseLog != nil {
				c.verboseLog("GSRN: server IS listening")
			}
			return nil
		}
		if err := ParseStatus(payload); err != nil {
			return err
		}
		return fmt.Errorf("unexpected status response")
	default:
		return fmt.Errorf("unexpected GSRN packet type: 0x%02x (expected STATUS)", pktType)
	}
}

// generateRandomToken creates a random 16-byte token using crypto/rand.
func generateRandomToken() ([TokenSize]byte, error) {
	var token [TokenSize]byte
	if _, err := io.ReadFull(rand.Reader, token[:]); err != nil {
		return token, fmt.Errorf("read random bytes: %w", err)
	}
	return token, nil
}
