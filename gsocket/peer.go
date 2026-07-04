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
	targetAddr  string
	listenAddr  string
	execCmd     string
	interactive bool
	logger      *log.Logger

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
	client, err := NewGSRNClient(p.secret)
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
	client, err := NewGSRNClient(p.secret, WithFlags(flagProtoLowLatency))
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

// forwardLocalPort listens on a local port and forwards each connection
// through the secure channel.
func (p *Peer) forwardLocalPort() error {
	listener, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", p.listenAddr, err)
	}
	defer listener.Close()

	p.logger.Printf("Forwarding %s through GS tunnel", p.listenAddr)

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
		go p.relayConn(conn)
	}
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
