package gsocket

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"

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
	channel *SecureChannel
	gsrn    *GSRNConn
	app     *AppProto

	// Configuration.
	targetAddr    string
	listenAddr    string
	execCmd       string
	interactive   bool
	socksServer   bool
	multiPeer     bool   // accept multiple connections (server + client -p)
	socksProxyAddr string // SOCKS5 proxy address for GSRN connections
	logger        *log.Logger

	// Internal state.
	mu        sync.Mutex
	running   bool
	done      chan struct{}
	closeOnce sync.Once

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
	opts := p.gsrnClientOpts()
	client, err := NewGSRNClient(p.secret, opts...)
	if err != nil {
		return fmt.Errorf("create GSRN client: %w", err)
	}

	p.logger.Printf("Registering on %s", client.addr.GSRNHostname())

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
// It blocks until the secure channel is established.
func (p *Peer) DialAndConnect(ctx context.Context) error {
	opts := append(p.gsrnClientOpts(), WithFlags(flagProtoLowLatency))
	client, err := NewGSRNClient(p.secret, opts...)
	if err != nil {
		return fmt.Errorf("create GSRN client: %w", err)
	}

	p.logger.Printf("Connecting to %s", client.addr.GSRNHostname())

	gsrn, err := client.ConnectClient()
	if err != nil {
		return fmt.Errorf("connect to GSRN: %w", err)
	}

	return p.finishHandshake(gsrn, false)
}

// gsrnClientOpts builds GSRNClient options from the peer's configuration.
func (p *Peer) gsrnClientOpts() []GSRNClientOption {
	var opts []GSRNClientOption
	if p.socksProxyAddr != "" {
		opts = append(opts, WithSOCKS5(p.socksProxyAddr))
	}
	return opts
}

// finishHandshake completes the secure channel setup after GSRN connects peers.
func (p *Peer) finishHandshake(gsrn *GSRNConn, isServer bool) error {
	channel, err := Handshake(gsrn.RawConn(), p.secret, isServer)
	if err != nil {
		gsrn.Close()
		return fmt.Errorf("secure handshake: %w", err)
	}

	// Stop the GSRN keepalive — after the handshake the raw connection
	// carries encrypted channel data, not GSRN protocol packets. The
	// keepalive pings would corrupt the secure channel.
	gsrn.StopKeepalive()

	p.channel = channel
	p.gsrn = gsrn
	p.app = NewAppProto(channel)
	p.running = true

	p.logger.Printf("Secure channel established")
	return nil
}

// RunShell relays stdin/stdout through the encrypted channel.
// In listen mode with -e, it executes a command instead.
func (p *Peer) RunShell() error {
	if !p.running {
		return fmt.Errorf("peer not connected")
	}

	if p.listenAddr != "" {
		return p.forwardLocalPort()
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
			return nil
		}
	}
}

// runExecCmd executes a command specified by -e and connects its stdin/stdout
// to the encrypted channel. stderr is merged into stdout.
func (p *Peer) runExecCmd() error {
	cmd := exec.Command("/bin/sh", "-c", p.execCmd)
	cmd.Stderr = cmd.Stdout

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

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
// control, Ctrl-C forwarding, etc.).
func (p *Peer) runInteractive() error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	return p.runWithPTY(shell)
}

// runClientInteractive sets up the local terminal for interactive use with a
// remote shell: raw mode (no local echo, char-by-char input) and Ctrl-C
// forwarding (sends 0x03 byte instead of killing gs-netcat).
func (p *Peer) runClientInteractive() error {
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

	// Stdin → Channel.
	go func() {
		defer p.Close()
		buf := make([]byte, 8192)
		for {
			n, err := os.Stdin.Read(buf)
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
			return nil
		}
	}
}

// forwardLocalPort listens on a local port and forwards connections
// through the GS tunnel. In multi-peer mode each incoming TCP connection
// gets its own GS tunnel; otherwise all connections share one tunnel.
func (p *Peer) forwardLocalPort() error {
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

// relayConn copies data between a local TCP connection and the secure channel.
func (p *Peer) relayConn(local net.Conn) {
	defer local.Close()

	var wg sync.WaitGroup
	wg.Add(2)

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

	wg.Wait()
}

// Channel returns the secure channel for direct read/write access.
func (p *Peer) Channel() *SecureChannel {
	return p.channel
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
