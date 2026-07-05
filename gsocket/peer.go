package gsocket

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/term"
)

// PeerRole indicates whether this peer initiated the connection.
type PeerRole int

// Peer roles.
const (
	RoleServer PeerRole = iota
	RoleClient
)

// Peer manages a single GS connection end-to-end: GSRN handshake,
// secure channel setup, and bidirectional data transfer with
// in-band application protocol support.
type Peer struct {
	role    PeerRole
	secret  string
	channel    *SecureChannel
	gsrn       *GSRNConn
	gsrnClient *GSRNClient // stored for token reuse (multi-sox)
	app        *AppProto

	// GSRN token cache for multi-sox token sharing.
	gsrnToken    [TokenSize]byte
	gsrnTokenSet bool

	// Configuration.
	targetAddr    string
	listenAddr    string
	execCmd       string
	interactive   bool
	socksServer   bool
	multiPeer     bool   // accept multiple connections (server + client -p)
	isUDP         bool   // use UDP transport with length-prefix framing
	socksProxyAddr string // SOCKS5 proxy address for GSRN connections
	sockWait      bool   // wait for server to become available (-w)
	logger        *log.Logger

	// Internal state.
	mu        sync.Mutex
	running   bool
	done      chan struct{}
	closeOnce sync.Once

	// PTY state (server side, Linux only).
	hasPTY      bool
	ptyMasterFd uintptr

	// Console state (client side).
	consoleReader *ConsoleReader

	// App-level ping state.
	pingTicker   *time.Ticker
	pingSentTime int64 // unix nano when last ping was sent

	// UDP unstack buffer — accumulates framed TCP data until a
	// complete datagram is available (matches C udp_unstack).
	udpBuf bytes.Buffer

	// Statistics.
	bytesRead    int64
	bytesWritten int64
}

// PeerOption configures a Peer.
type PeerOption func(*Peer)

// WithTargetAddr sets the TCP forwarding target (ip:port).
func WithTargetAddr(addr string) PeerOption {
	return func(p *Peer) { p.targetAddr = addr }
}

// WithListenAddr sets the local address to listen on for TCP forwarding.
func WithListenAddr(addr string) PeerOption {
	return func(p *Peer) { p.listenAddr = addr }
}

// WithExecCmd sets a command to execute on connection.
func WithExecCmd(cmd string) PeerOption {
	return func(p *Peer) { p.execCmd = cmd }
}

// WithInteractive enables interactive shell mode.
func WithInteractive() PeerOption {
	return func(p *Peer) { p.interactive = true }
}

// WithSOCKSServer enables SOCKS5 server mode. The peer acts as a SOCKS5
// proxy: after the GS tunnel is established, the server reads a SOCKS5
// CONNECT request from the channel and forwards the traffic to the
// requested target. Use with -l (listen/server mode).
func WithSOCKSServer() PeerOption {
	return func(p *Peer) { p.socksServer = true }
}

// WithUDP enables UDP transport mode. Data is framed with a 16-bit
// length prefix over the TCP tunnel, matching the C implementation's
// udp_unstack / wrap mechanism. Use with -p for port forwarding.
func WithUDP() PeerOption {
	return func(p *Peer) { p.isUDP = true }
}

// WithMultiPeer enables accepting multiple connections. On the server side
// (-l), multiple clients can connect simultaneously. On the client side
// (-p), each incoming TCP connection gets its own GS tunnel instead of
// sharing a single tunnel.
func WithMultiPeer() PeerOption {
	return func(p *Peer) { p.multiPeer = true }
}

// WithSOCKS5Proxy sets a SOCKS5 proxy address for the GSRN TCP connection.
// When set, all GSRN relay traffic is routed through the specified SOCKS5
// proxy (e.g. "127.0.0.1:9050" for TOR). Both listener and client peers
// can use a proxy independently.
func WithSOCKS5Proxy(addr string) PeerOption {
	return func(p *Peer) { p.socksProxyAddr = addr }
}

// WithSockWait enables wait-for-server mode (-w flag). On the client side,
// DialAndConnect retries every 2 seconds until a server appears instead of
// failing immediately with "no server listening". Matches C's GS_OPT_SOCKWAIT.
func WithSockWait() PeerOption {
	return func(p *Peer) { p.sockWait = true }
}

// WithToken sets the GSRN protocol token to reuse across listen connections.
// This matches C's multi-sox pattern where all listen sockets share the same
// address+token so GSRN allows multiple concurrent registrations.
func WithToken(token [TokenSize]byte) PeerOption {
	return func(p *Peer) { p.gsrnToken = token; p.gsrnTokenSet = true }
}

// WithLogger sets the logger for peer diagnostics.
func WithLogger(l *log.Logger) PeerOption {
	return func(p *Peer) { p.logger = l }
}

// NewPeer creates a new Peer with the given shared secret and role.
func NewPeer(secret string, role PeerRole, opts ...PeerOption) *Peer {
	p := &Peer{
		role:   role,
		secret: secret,
		done:   make(chan struct{}),
		logger: log.New(os.Stderr, "gsocket: ", log.LstdFlags),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// AcceptOnListener registers with GSRN as a listener and waits for a client.
// It blocks until the secure channel is established.
func (p *Peer) AcceptOnListener(ctx context.Context) error {
	opts := p.gsrnClientOpts(ctx)
	client, err := NewGSRNClient(p.secret, opts...)
	if err != nil {
		return fmt.Errorf("create GSRN client: %w", err)
	}
	p.gsrnClient = client
	if p.gsrnTokenSet {
		client.SetToken(p.gsrnToken)
	}

	p.logger.Printf("Registering on %s", client.primaryHost())

	gsrn, err := client.ConnectListener()
	if err != nil {
		return fmt.Errorf("register as listener: %w", err)
	}

	p.logger.Printf("Waiting for client...")

	if _, err := client.WaitForClient(gsrn); err != nil {
		gsrn.Close()
		return fmt.Errorf("wait for client: %w", err)
	}

	return p.finishHandshake(gsrn, true)
}

// DialAndConnect connects to GSRN as a client and establishes a secure channel.
// It blocks until the secure channel is established. If sockWait is true, it
// retries every 2 seconds until a server appears (matching C's GS_OPT_SOCKWAIT).
func (p *Peer) DialAndConnect(ctx context.Context) error {
	opts := append(p.gsrnClientOpts(ctx), WithFlags(flagProtoLowLatency))

	for {
		client, err := NewGSRNClient(p.secret, opts...)
		if err != nil {
			return fmt.Errorf("create GSRN client: %w", err)
		}
		p.gsrnClient = client

		if p.sockWait {
			p.logger.Printf("Waiting for server on %s...", client.primaryHost())
		} else {
			p.logger.Printf("Connecting to %s", client.primaryHost())
		}

		gsrn, err := client.ConnectClient()
		if err == nil {
			return p.finishHandshake(gsrn, false)
		}

		// If not waiting, fail immediately.
		if !p.sockWait {
			return fmt.Errorf("connect to GSRN: %w", err)
		}

		// Only retry on "no server listening"; fail fast on other errors.
		if !errors.Is(err, ErrGSRNConnRefused) {
			return fmt.Errorf("connect to GSRN: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// gsrnClientOpts builds GSRNClient options from the peer's configuration.
// If ctx is non-nil, it's used for cancellation during dial (host rotation).
func (p *Peer) gsrnClientOpts(ctx context.Context) []GSRNClientOption {
	var opts []GSRNClientOption
	if p.socksProxyAddr != "" {
		opts = append(opts, WithSOCKS5(p.socksProxyAddr))
	}
	if ctx != nil {
		opts = append(opts, WithContext(ctx))
	}
	// Wire verbose logging: GSRN connection steps are logged via the
	// peer's logger. Format: "gsrn: <message>".
	opts = append(opts, WithVerboseLog(func(format string, args ...any) {
		p.logger.Printf("gsrn: "+format, args...)
	}))
	return opts
}

// finishHandshake completes the secure channel setup after GSRN connects peers.
func (p *Peer) finishHandshake(gsrn *GSRNConn, isServer bool) error {
	start := time.Now()
	roleStr := "client"
	if isServer {
		roleStr = "server"
	}
	p.logger.Printf("starting secure handshake (role=%s)...", roleStr)

	channel, err := Handshake(gsrn.RawConn(), p.secret, isServer)
	if err != nil {
		gsrn.Close()
		p.logger.Printf("secure handshake failed after %v: %v", time.Since(start).Round(time.Millisecond), err)
		return fmt.Errorf("secure handshake: %w", err)
	}

	p.logger.Printf("secure handshake complete (%v)", time.Since(start).Round(time.Millisecond))

	// Stop the GSRN keepalive — after the handshake the raw connection
	// carries encrypted channel data, not GSRN protocol packets. The
	// keepalive pings would corrupt the secure channel.
	gsrn.StopKeepalive()

	p.channel = channel
	p.gsrn = gsrn
	p.app = NewAppProto(channel)
	p.running = true

	// Wire up application protocol callbacks. These handle in-band
	// signalling (window resize, keepalive, logs, status) that is
	// multiplexed within the encrypted data stream.
	p.wireAppCallbacks()

	p.logger.Printf("Secure channel established")
	return nil
}

// RunShell relays stdin/stdout through the encrypted channel.
// In listen mode with -e, it executes a command instead.
func (p *Peer) RunShell() error {
	if !p.running {
		return fmt.Errorf("peer not connected")
	}

	// Client-side: listen on local port and forward to GS tunnel.
	if p.listenAddr != "" {
		return p.forwardLocalPort()
	}

	// Server-side: forward GS tunnel to a fixed target (-d addr:port).
	if p.targetAddr != "" && p.role == RoleServer {
		return p.forwardToTarget()
	}

	// SOCKS5 server mode: after the tunnel is established, act as a
	// SOCKS5 proxy — read CONNECT requests from the encrypted channel
	// and forward them to the requested destinations.
	if p.socksServer {
		return p.runSOCKSServer()
	}

	fmt.Fprintf(os.Stderr, "GS tunnel established. Press Ctrl-C to exit.\r\n")

	if p.execCmd != "" {
		return p.runExecCmd()
	}
	// -i on the server spawns an interactive shell. On the client it
	// puts the local terminal in raw mode and forwards Ctrl-C.
	if p.interactive {
		if p.role == RoleServer {
			return p.runInteractive()
		}
		return p.runClientInteractive()
	}
	return p.runRelay()
}

// runSOCKSServer handles the server side of SOCKS5 proxying through the GS
// tunnel. It reads a SOCKS5 CONNECT request from the encrypted channel,
// connects to the requested target, and relays data bidirectionally.
//
// On success a SOCKS5 reply is sent back through the channel before
// relaying begins. The SOCKS5 client (remote peer) drives the requests;
// each call to runSOCKSServer handles one CONNECT request.
func (p *Peer) runSOCKSServer() error {
	p.logger.Printf("SOCKS5 server mode active")

	for {
		// Read and handle one SOCKS5 CONNECT request.
		target, err := SOCKSServe(p.channel)
		if err != nil {
			return fmt.Errorf("SOCKS5 serve: %w", err)
		}

		p.logger.Printf("SOCKS5 CONNECT to %s", target.RemoteAddr())

		// Bidirectional relay.
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			io.Copy(p.channel, target)
			p.Close()
		}()

		go func() {
			defer wg.Done()
			io.Copy(target, p.channel)
			p.Close()
		}()

		wg.Wait()
		target.Close()

		// If the peer has been closed, stop serving.
		p.mu.Lock()
		running := p.running
		p.mu.Unlock()
		if !running {
			return nil
		}
	}
}

// runRelay is the default mode: bidirectional relay between local stdin/stdout
// and the encrypted channel. The channel→stdout path blocks until the remote
// side disconnects or the peer is closed (Ctrl-C).
func (p *Peer) runRelay() error {
	// Stdin → Channel (background, uninterruptible — killed at exit).
	go func() {
		defer p.Close()
		reader := bufio.NewReader(os.Stdin)
		buf := make([]byte, 8192)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				if _, werr := p.channel.Write(buf[:n]); werr != nil {
					return
				}
				p.mu.Lock()
				p.bytesWritten += int64(n)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Channel → Stdout (blocks until remote disconnects or peer is closed).
	buf := make([]byte, 8192)
	for {
		n, err := p.channel.Read(buf)
		if n > 0 {
			plaintext, derr := p.app.Decode(buf[:n])
			if derr != nil {
				return derr
			}
			if len(plaintext) > 0 {
				os.Stdout.Write(plaintext)
			}
			p.mu.Lock()
			p.bytesRead += int64(n)
			p.mu.Unlock()
		}
		if err != nil {
			// Remote closed the channel — close our stdin to unblock the
			// stdin→channel goroutine which may be stuck on os.Stdin.Read.
			os.Stdin.Close()
			return nil
		}
	}
}

// runExecCmd executes a command specified by -e and connects its stdin/stdout
// to the encrypted channel. stderr is merged into stdout.
// Uses the platform-appropriate shell: $SHELL or /bin/sh on Unix,
// powershell.exe or cmd.exe on Windows.
func (p *Peer) runExecCmd() error {
	sh := shellExec()
	args := shellExecArgs(p.execCmd)
	cmd := exec.Command(sh, args...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// Merge stderr into stdout AFTER StdoutPipe() creates the pipe.
	// If set before, cmd.Stdout is nil and stderr defaults to os.Stderr.
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// Kill command when peer is closed (Ctrl-C on server).
	go func() {
		<-p.done
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	// Channel → Command stdin.
	go func() {
		defer stdinPipe.Close()
		defer p.Close()
		buf := make([]byte, 8192)
		for {
			n, err := p.channel.Read(buf)
			if n > 0 {
				plaintext, derr := p.app.Decode(buf[:n])
				if derr != nil {
					return
				}
				if len(plaintext) > 0 {
					if _, werr := stdinPipe.Write(plaintext); werr != nil {
						return
					}
				}
				p.mu.Lock()
				p.bytesRead += int64(n)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Command stdout → Channel (blocks until command exits or channel breaks).
	io.Copy(p.channel, stdoutPipe)
	p.Close()
	cmd.Wait()
	return nil
}

// runInteractive spawns an interactive shell and connects it to the encrypted
// channel. On Linux, it allocates a PTY for proper terminal handling (job
// control, Ctrl-C forwarding, etc.). On Windows, uses powershell or cmd.exe.
func (p *Peer) runInteractive() error {
	shell := shellInteractive()

	return p.runWithPTY(shell)
}

// runClientInteractive sets up the local terminal for interactive use with a
// remote shell: raw mode (no local echo, char-by-char input), Ctrl-C
// forwarding (sends 0x03 byte instead of killing gs-netcat), Ctrl-E console
// escape handling, and SIGWINCH forwarding for terminal resize.
func (p *Peer) runClientInteractive() error {
	// Start app-level keepalive pings (every 30 s, matching C).
	p.startAppPing()

	// Put terminal in raw mode: no echo, char-by-char input, no line
	// buffering. term.MakeRaw also clears ISIG, so Ctrl-C produces the
	// byte 0x03 in the stdin stream instead of generating SIGINT. That
	// byte flows through the channel to the remote PTY, which translates
	// it into SIGINT for the remote foreground process.
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("raw terminal: %w", err)
	}
	defer term.Restore(fd, oldState)

	// Register SIGWINCH handler — when the terminal is resized, capture
	// the new dimensions and send a WSIZE message to the server.
	p.registerWinchHandler()

	// Wrap stdin with ConsoleReader to handle Ctrl-E escape sequences.
	// Ctrl-E + E/Ctrl-E sends literal 0x05 through the channel (for emacs
	// and other applications that use Ctrl-E).
	cr := NewConsoleReader(os.Stdin)

	// Stdin → Channel.
	go func() {
		defer p.Close()
		buf := make([]byte, 8192)
		for {
			n, err := cr.Read(buf)
			if n > 0 {
				if _, werr := p.channel.Write(buf[:n]); werr != nil {
					return
				}
				p.mu.Lock()
				p.bytesWritten += int64(n)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Channel → Stdout (blocks until remote disconnects or channel breaks).
	buf := make([]byte, 8192)
	for {
		n, err := p.channel.Read(buf)
		if n > 0 {
			plaintext, derr := p.app.Decode(buf[:n])
			if derr != nil {
				return derr
			}
			if len(plaintext) > 0 {
				os.Stdout.Write(plaintext)
			}
			p.mu.Lock()
			p.bytesRead += int64(n)
			p.mu.Unlock()
		}
		if err != nil {
			// Remote closed the channel — close stdin to unblock the
			// stdin→channel goroutine stuck on terminal read.
			os.Stdin.Close()
			return nil
		}
	}
}

// forwardToTarget connects to a fixed target address and relays data
// between it and the GS tunnel. Used on the server side (-d addr:port).
// Supports TCP and UDP.
func (p *Peer) forwardToTarget() error {
	network := "tcp"
	if p.isUDP {
		network = "udp"
	}

	target, err := net.Dial(network, p.targetAddr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", p.targetAddr, err)
	}
	defer target.Close()

	p.logger.Printf("Forwarding GS tunnel to %s [%s]", p.targetAddr, network)

	// Relay between channel and target. relayConn handles both TCP
	// (straight copy) and UDP (length-prefix framing).
	p.relayConn(target)
	return nil
}

// forwardLocalPort listens on a local port and forwards connections
// through the GS tunnel. In multi-peer mode each incoming TCP connection
// gets its own GS tunnel; otherwise all connections share one tunnel.
func (p *Peer) forwardLocalPort() error {
	// UDP mode: listen on UDP, relay all datagrams through the single
	// GS tunnel with length-prefix framing.
	if p.isUDP {
		return p.forwardLocalPortUDP()
	}

	listener, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", p.listenAddr, err)
	}
	defer listener.Close()

	if p.multiPeer {
		p.logger.Printf("Multi-peer: each connection to %s gets its own GS tunnel", p.listenAddr)
	} else {
		p.logger.Printf("Forwarding %s through GS tunnel", p.listenAddr)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-p.done:
				return nil
			default:
				p.logger.Printf("Accept error: %v", err)
				continue
			}
		}
		if p.multiPeer {
			go p.handleMultiPeerConn(conn)
		} else {
			go p.relayConn(conn)
		}
	}
}

// forwardLocalPortUDP listens on a UDP port and relays datagrams through
// the GS tunnel with length-prefix framing. Only one UDP "connection" is
// supported at a time — the first peer to send a datagram is pinned via
// connect(), matching the C implementation's recvfrom + connect pattern.
func (p *Peer) forwardLocalPortUDP() error {
	addr, err := net.ResolveUDPAddr("udp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", p.listenAddr, err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen UDP on %s: %w", p.listenAddr, err)
	}
	defer conn.Close()

	p.logger.Printf("UDP forwarding on %s through GS tunnel", p.listenAddr)

	// Read one datagram to discover the peer, then "connect" to pin the
	// socket to that peer (matches C recvfrom + connect pattern).
	buf := make([]byte, 65535)
	n, peerAddr, err := conn.ReadFromUDP(buf)
	if err != nil {
		return fmt.Errorf("UDP read: %w", err)
	}
	if err := conn.Close(); err != nil {
		return err
	}

	// Create a connected UDP socket pinned to the peer.
	peerConn, err := net.DialUDP("udp", nil, peerAddr)
	if err != nil {
		return fmt.Errorf("UDP connect to peer %s: %w", peerAddr, err)
	}
	defer peerConn.Close()

	p.logger.Printf("UDP peer connected: %s", peerAddr)

	// Replay the first datagram we already buffered.
	if _, err := peerConn.Write(buf[:n]); err != nil {
		return fmt.Errorf("UDP replay write: %w", err)
	}

	p.relayConn(peerConn)
	return nil
}

// handleMultiPeerConn creates a new GS tunnel for an incoming TCP
// connection and relays data between them. Used in client multi-peer
// mode (-p without -l) where each incoming connection gets its own
// encrypted tunnel to the GS server.
func (p *Peer) handleMultiPeerConn(local net.Conn) {
	defer local.Close()

	// Create a fresh peer for this connection.
	cp := NewPeer(p.secret, RoleClient)
	cp.socksProxyAddr = p.socksProxyAddr
	cp.logger = p.logger

	if err := cp.DialAndConnect(context.Background()); err != nil {
		p.logger.Printf("Multi-peer dial failed: %v", err)
		return
	}
	defer cp.Close()

	// Bidirectional relay between local connection and new GS tunnel.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(cp.channel, local)
		cp.Close()
	}()

	go func() {
		defer wg.Done()
		io.Copy(local, cp.channel)
		cp.Close()
	}()

	wg.Wait()
}

// relayConn copies data between a local TCP connection and the secure
// channel. In UDP mode each read from the local socket is prefixed with a
// 2-byte length before being written to the channel; data read from the
// channel is unbuffered via udpUnstack before being written to the socket.
func (p *Peer) relayConn(local net.Conn) {
	defer local.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	if p.isUDP {
		// Local → Channel: read datagram, prepend 2-byte length.
		go func() {
			defer wg.Done()
			defer p.Close()
			buf := make([]byte, 65535)
			for {
				n, err := local.Read(buf)
				if n > 0 {
					framed := wrapUDP(buf[:n])
					if _, werr := p.channel.Write(framed); werr != nil {
						return
					}
					p.mu.Lock()
					p.bytesWritten += int64(n)
					p.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()

		// Channel → Local: unstack framed datagrams.
		go func() {
			defer wg.Done()
			defer p.Close()
			buf := make([]byte, 8192)
			for {
				n, err := p.channel.Read(buf)
				if n > 0 {
					p.mu.Lock()
					p.udpBuf.Write(buf[:n])
					for p.udpBuf.Len() >= 2 {
						raw := p.udpBuf.Bytes()
						dlen := int(binary.BigEndian.Uint16(raw[:2]))
						if p.udpBuf.Len() < 2+dlen {
							break // need more data
						}
						p.udpBuf.Next(2) // consume length prefix
						payload := make([]byte, dlen)
						p.udpBuf.Read(payload)
						local.Write(payload)
						p.bytesRead += int64(dlen)
					}
					p.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
	} else {
		// TCP mode: straight io.Copy.
		go func() {
			defer wg.Done()
			io.Copy(p.channel, local)
			p.Close()
		}()

		go func() {
			defer wg.Done()
			io.Copy(local, p.channel)
			p.Close()
		}()
	}

	wg.Wait()
}

// wrapUDP prepends a 2-byte big-endian length prefix to data for UDP
// framing over the TCP tunnel. Matches the C implementation.
func wrapUDP(data []byte) []byte {
	framed := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(framed[:2], uint16(len(data)))
	copy(framed[2:], data)
	return framed
}

// Channel returns the secure channel for direct read/write access.
func (p *Peer) Channel() *SecureChannel {
	return p.channel
}

// GSClient returns the underlying GSRN client, if available.
// Used for accessing the GSRN protocol token (multi-sox token sharing).
func (p *Peer) GSClient() *GSRNClient {
	return p.gsrnClient
}

// Close shuts down the peer and releases resources.
func (p *Peer) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
		close(p.done)
	})
	if p.channel != nil {
		return p.channel.Close()
	}
	if p.gsrn != nil {
		return p.gsrn.Close()
	}
	return nil
}

// Stats returns the total bytes transferred.
func (p *Peer) Stats() (read, written int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytesRead, p.bytesWritten
}

// wireAppCallbacks registers handlers for in-band application protocol
// messages. These fire during p.app.Decode() calls when escape sequences
// are extracted from the encrypted data stream.
func (p *Peer) wireAppCallbacks() {
	// WSIZE — window resize from client → apply to PTY master (server only).
	p.app.OnMessage(msgWSize, func(msgType uint8, data []byte) error {
		if len(data) < 4 {
			return nil
		}
		cols := binary.BigEndian.Uint16(data[0:2])
		rows := binary.BigEndian.Uint16(data[2:4])

		p.mu.Lock()
		fd := p.ptyMasterFd
		hasPTY := p.hasPTY
		p.mu.Unlock()

		if !hasPTY || fd == 0 {
			return nil
		}
		return resizePTY(fd, rows, cols)
	})

	// PING — app-level keepalive from client → server replies with PONG.
	p.app.OnMessage(msgPing, func(msgType uint8, data []byte) error {
		// Send PONG with load/idle/user info.
		// Matches C's pkt_app_cb_ping() → pkt_app_send_pong().
		pong := AppPong{
			Load:  0,
			Idle:  0,
			NUsers: 1,
		}
		copy(pong.User[:], "gsocket")
		var buf bytes.Buffer
		if err := binary.Write(&buf, binary.BigEndian, pong); err != nil {
			return err
		}
		return p.app.SendMessage(msgPong, buf.Bytes())
	})

	// PONG — reply to our PING (client side).
	p.app.OnMessage(msgPong, func(msgType uint8, data []byte) error {
		// Log RTT if we were waiting for a pong.
		p.mu.Lock()
		sentAt := p.pingSentTime
		p.pingSentTime = 0
		p.mu.Unlock()
		if sentAt > 0 {
			rtt := time.Since(time.Unix(0, sentAt))
			p.logger.Printf("PONG received (RTT %v)", rtt.Round(time.Microsecond))
		}
		return nil
	})

	// LOG — server→client log message.
	p.app.OnMessage(msgLog, func(msgType uint8, data []byte) error {
		if len(data) < 1 {
			return nil
		}
		logType := data[0]
		msg := string(bytes.TrimRight(data[1:], "\x00"))
		if msg == "" {
			return nil
		}
		prefix := ""
		switch logType {
		case LogTypeAlert:
			prefix = "[ALERT] "
		case LogTypeNotice:
			prefix = "[NOTICE] "
		case LogTypeInfo:
			prefix = "[INFO] "
		}
		p.logger.Printf("%s%s", prefix, msg)
		return nil
	})

	// STATUS — server→client status message (e.g. NOPTY).
	p.app.OnMessage(msgStatus, func(msgType uint8, data []byte) error {
		if len(data) < 1 {
			return nil
		}
		switch data[0] {
		case StatusTypeNoPTY:
			p.logger.Printf("Server has no PTY — switching to pipe mode")
			p.mu.Lock()
			p.hasPTY = false
			p.mu.Unlock()
		}
		return nil
	})
}

// startAppPing starts periodic application-level ping messages.
// This keeps the connection alive at the application layer and
// allows RTT measurement. Only active on the client side.
func (p *Peer) startAppPing() {
	p.pingTicker = time.NewTicker(30 * time.Second)
	go func() {
		for {
			select {
			case <-p.pingTicker.C:
				ping := AppPing{}
				p.mu.Lock()
				p.pingSentTime = time.Now().UnixNano()
				p.mu.Unlock()
				var buf bytes.Buffer
				if err := binary.Write(&buf, binary.BigEndian, ping); err != nil {
					continue
				}
				_ = p.app.SendMessage(msgPing, buf.Bytes())
			case <-p.done:
				return
			}
		}
	}()
}
