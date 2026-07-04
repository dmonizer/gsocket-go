// gs-netcat is a netcat-like tool that connects TCP pipes through the
// Global Socket Relay Network (GSRN) with end-to-end encryption.
//
// It allows two peers behind NAT/firewalls to establish a TCP connection
// using only a shared secret — no IP addresses or port forwarding needed.
//
// Usage:
//
//	# Interactive shell (server listens, client connects)
//	Server:  gs-netcat -l -i -s MySecret
//	Client:  gs-netcat -i -s MySecret
//
//	# TCP forwarding (expose port 22 through GSRN)
//	Server:  gs-netcat -l -d 127.0.0.1:22 -s MySecret
//	Client:  gs-netcat -p 2222 -s MySecret
//
//	# Execute a command on connection
//	Server:  gs-netcat -l -e "/bin/sh -i" -s MySecret
//
//	# Use TOR
//	Server:  gs-netcat -l -i --tor -s MySecret
//	Client:  gs-netcat -i --tor -s MySecret
//
//	# SOCKS5 proxy via custom proxy
//	GSOCKET_SOCKS_IP=10.0.0.1 GSOCKET_SOCKS_PORT=1080 gs-netcat -l -i -s MySecret
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/hackerschoice/gsocket-go/gsocket"
)

const (
	envSecret        = "GSOCKET_SECRET"
	envSOCKSIP       = "GSOCKET_SOCKS_IP"
	envSOCKSPort     = "GSOCKET_SOCKS_PORT"
	defaultSOCKSIP   = "127.0.0.1"
	defaultSOCKSPort = "9050"
	appName          = "gs-netcat"

	// Internal env vars for daemon/watchdog re-exec (matching C's
	// _GSOCKET_INTERNAL pattern).
	envDaemonChild = "_GSOCKET_DAEMON_CHILD"
	envWorker      = "_GSOCKET_WORKER"

	// EX_BAD_AUTH matches the C codebase — used by the watchdog to
	// detect that another daemon with the same secret is already listening.
	exitBadAuth = 201
)

func main() {
	log.SetFlags(0)
	log.SetPrefix(appName + ": ")

	// Parse flags.
	var (
		listen      = flag.Bool("l", false, "Listen mode (wait for incoming connection)")
		secret      = flag.String("s", "", "Shared secret for authentication")
		interactive = flag.Bool("i", false, "Interactive shell mode")
		execCmd     = flag.String("e", "", "Execute command on connection")
		targetAddr  = flag.String("d", "", "Target address for TCP forwarding (ip:port)")
		listenPort  = flag.String("p", "", "Local port to listen on for TCP forwarding")
		useTor      = flag.Bool("tor", false, "Use TOR via SOCKS5 proxy (127.0.0.1:9050)")
		socksServer = flag.Bool("S", false, "SOCKS5 server mode (needs -l)")
		useUDP      = flag.Bool("u", false, "Use UDP transport (requires -p)")
		verbose     = flag.Bool("v", false, "Verbose output")
		wait        = flag.Bool("w", false, "Wait for server to become available")
		genSecret   = flag.Bool("g", false, "Generate a random secret and exit")
		daemon      = flag.Bool("D", false, "Daemon mode — fork into background with auto-restart")
		watchdog    = flag.Bool("W", false, "Watchdog mode — auto-restart on crash")
	)
	flag.Parse()

	// -g: Generate a cryptographically random secret, print it, and exit.
	// Matches C's `-g` flag.
	if *genSecret {
		fmt.Println(generateSecret())
		os.Exit(0)
	}

	// --- Daemon / Watchdog pre-flight ---
	// -D: Re-exec as a daemon child, parent exits immediately.
	// Daemon mode includes watchdog (auto-restart) behaviour, matching C's
	// GS_daemonize() which combines daemonizing + watchdog in one function.
	if *daemon && os.Getenv(envDaemonChild) == "" {
		fmt.Fprintf(os.Stderr, "%s: daemon starting (pid=%d)\n", appName, os.Getpid())
		reexecAsDaemon()
		os.Exit(0)
	}

	// --- Daemon child: detach from terminal ---
	// We are the re-exec'd daemon child. Detach from the controlling
	// terminal: new session, chdir to /, close standard file descriptors.
	// Do this BEFORE watchdog so the watchdog also runs detached.
	if os.Getenv(envDaemonChild) != "" {
		detachFromTerminal()
	}

	// -D implies watchdog (matching C's GS_daemonize which combines
	// daemonizing + watchdog in one function). -W alone gives watchdog
	// without daemonizing. Either path spawns a worker child and monitors
	// it with restart-on-crash backoff.
	shouldWatchdog := (*daemon || *watchdog) && os.Getenv(envWorker) == ""
	if shouldWatchdog {
		runWatchdog()
		return // runWatchdog loops forever, worker does the work
	}

	// Resolve secret from flag, environment, or interactive prompt.
	// When -s is not provided and GSOCKET_SECRET is not set, the user is
	// prompted — matching C's GS_user_secret() behaviour. Pressing Enter
	// at the prompt generates a cryptographically random secret.
	sec := resolveSecret(*secret)
	if sec == "" {
		log.Fatal("No secret provided. Use -s <secret> or set GSOCKET_SECRET environment variable.")
	}

	// Configure logger.
	logger := log.New(os.Stderr, appName+": ", log.LstdFlags)
	if !*verbose {
		logger.SetOutput(os.Stderr)
	}

	// Build peer options.
	var opts []gsocket.PeerOption
	if *execCmd != "" {
		opts = append(opts, gsocket.WithExecCmd(*execCmd))
	}
	if *interactive {
		opts = append(opts, gsocket.WithInteractive())
	}
	if *targetAddr != "" {
		opts = append(opts, gsocket.WithTargetAddr(*targetAddr))
	}
	if *listenPort != "" {
		opts = append(opts, gsocket.WithListenAddr(*listenPort))
	}
	if *socksServer {
		opts = append(opts, gsocket.WithSOCKSServer())
	}
	if *useUDP {
		opts = append(opts, gsocket.WithUDP())
	}
	// Resolve SOCKS5 proxy address for GSRN TCP connections.
	// Priority: --tor flag > GSOCKET_SOCKS_IP / GSOCKET_SOCKS_PORT env vars.
	if socksAddr := resolveSOCKS5Addr(*useTor); socksAddr != "" {
		opts = append(opts, gsocket.WithSOCKS5Proxy(socksAddr))
	}
	// Multi-peer: any flag that implies multiple sessions.
	// Server: -i, -e, -d, -S. Client: -p.
	if *interactive || *execCmd != "" || *targetAddr != "" || *socksServer || *listenPort != "" {
		opts = append(opts, gsocket.WithMultiPeer())
	}
	opts = append(opts, gsocket.WithLogger(logger))

	// Dispatch based on mode. Signal handling is set up inside each
	// function because the behaviour differs: the listener should exit
	// on Ctrl-C, while an interactive client forwards Ctrl-C to the
	// remote shell.
	if *wait {
		opts = append(opts, gsocket.WithSockWait())
	}
	if *listen {
		runListener(sec, opts)
	} else {
		runClient(sec, opts, *interactive)
	}
}

// resolveSecret returns the shared secret, resolved in order:
//
//  1. -s flag (explicit CLI argument)
//  2. GSOCKET_SECRET environment variable
//  3. Interactive prompt (press Enter to auto-generate)
//
// Matches C's GS_user_secret() behaviour:
//   - If -s or env is set → use it directly
//   - Otherwise prompt "Enter Secret (or press Enter to generate): "
//   - Empty input → generate a random 32-char hex secret and print it
func resolveSecret(flagSecret string) string {
	if flagSecret != "" {
		return flagSecret
	}
	if s := os.Getenv(envSecret); s != "" {
		return s
	}

	// No secret from flag or env — prompt user (matching C).
	fmt.Fprintf(os.Stderr, "Enter Secret (or press Enter to generate): ")
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return ""
	}
	input = strings.TrimRight(input, "\r\n")
	if input != "" {
		return input
	}

	// Empty input → generate a random secret.
	s := generateSecret()
	fmt.Fprintf(os.Stderr, "=Secret         : %s\n", s)
	return s
}

// generateSecret returns a 32-character hex string from 16 random bytes
// (128 bits of entropy). Equivalent to C's GS_gen_secret() which produces
// a 21-char base58-encoded secret from 16 random bytes.
func generateSecret() string {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		log.Fatalf("generate secret: %v", err)
	}
	return hex.EncodeToString(buf)
}

// resolveSOCKS5Addr returns a SOCKS5 proxy address ("ip:port") if one is
// configured via the --tor flag or GSOCKET_SOCKS_IP / GSOCKET_SOCKS_PORT
// environment variables. Returns "" if no proxy should be used.
func resolveSOCKS5Addr(useTor bool) string {
	ip := os.Getenv(envSOCKSIP)
	port := os.Getenv(envSOCKSPort)

	if useTor {
		if ip == "" {
			ip = defaultSOCKSIP
		}
		if port == "" {
			port = defaultSOCKSPort
		}
		return fmt.Sprintf("%s:%s", ip, port)
	}

	// No --tor, but user set GSOCKET_SOCKS_IP explicitly.
	if ip != "" {
		if port == "" {
			port = defaultSOCKSPort
		}
		return fmt.Sprintf("%s:%s", ip, port)
	}

	return ""
}

func runListener(secret string, opts []gsocket.PeerOption) {
	// Shared shutdown: one Ctrl-C stops the entire listener loop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		log.Printf("Interrupted, shutting down...")
		cancel()
	}()

	// Track active sessions for clean shutdown.
	var wg sync.WaitGroup
	activeCount := 0

	// Accept loop: each accepted client gets its own goroutine so
	// multiple clients can be connected simultaneously. The server
	// re-registers with GSRN immediately after each accept to
	// minimise the window where no listener is present.
loop:
	for ctx.Err() == nil {
		peer := gsocket.NewPeer(secret, gsocket.RoleServer, opts...)

		if err := peer.AcceptOnListener(ctx); err != nil {
			if ctx.Err() != nil {
				break // normal shutdown
			}
			log.Printf("Accept failed: %v — retrying in 3s...", err)
			peer.Close()
			select {
			case <-ctx.Done():
				break loop
			case <-time.After(3 * time.Second):
			}
			continue
		}

		// Spawn session handler — does not block the accept loop.
		wg.Add(1)
		activeCount++
		log.Printf("Client connected [%d active session(s)]", activeCount)

		go func(p *gsocket.Peer) {
			defer wg.Done()
			defer p.Close()
			if err := p.RunShell(); err != nil {
				log.Printf("Session ended: %v", err)
			}
		}(peer)

		// Brief yield to let the goroutine start and to avoid
		// hammering GSRN with immediate re-registration.
		select {
		case <-ctx.Done():
			break loop
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Wait for active sessions to finish (with timeout).
	log.Printf("Waiting for active sessions to finish...")
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("All sessions finished.")
	case <-time.After(30 * time.Second):
		log.Printf("Timed out waiting for sessions to finish.")
	}
}

func runClient(secret string, opts []gsocket.PeerOption, interactive bool) {
	peer := gsocket.NewPeer(secret, gsocket.RoleClient, opts...)

	// Cancellable context for DialAndConnect + dialGSRN. On Ctrl-C the
	// signal handler cancels the context, which propagates through to the
	// GSRN dial loop (stopping hostname rotation between attempts).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !interactive {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		go func() {
			<-sigCh
			log.Printf("Interrupted, shutting down...")
			signal.Stop(sigCh)
			cancel()
			peer.Close()
		}()
	}

	if err := peer.DialAndConnect(ctx); err != nil {
		if ctx.Err() != nil {
			os.Exit(1) // cancelled by user
		}
		log.Fatalf("Connection failed: %v", err)
	}
	defer peer.Close()

	if err := peer.RunShell(); err != nil {
		log.Fatalf("Shell session error: %v", err)
	}
}
