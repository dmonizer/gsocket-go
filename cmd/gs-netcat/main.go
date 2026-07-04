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

// realMain is the actual work function, set by main() and called either
// directly (console mode) or by the Windows service handler.
var realMain func() error

func main() {
	log.SetFlags(log.LstdFlags)
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

	// Capture flag values for realMain closure (flag pointers change).
	listenFlag := *listen
	interactiveFlag := *interactive
	secretFlag := *secret
	execCmdFlag := *execCmd
	targetAddrFlag := *targetAddr
	listenPortFlag := *listenPort
	socksServerFlag := *socksServer
	useUDPFlag := *useUDP
	useTorFlag := *useTor
	verboseFlag := *verbose
	waitFlag := *wait
	daemonFlag := *daemon
	watchdogFlag := *watchdog

	// realMain is the actual work: resolve secret, connect, run shell.
	// On Windows, the service handler calls this in a goroutine.
	realMain = func() error {
		// --- Daemon / Watchdog pre-flight ---
		if daemonFlag && os.Getenv(envDaemonChild) == "" {
			fmt.Fprintf(os.Stderr, "%s: daemon starting (pid=%d)\n", appName, os.Getpid())
			reexecAsDaemon()
			os.Exit(0)
		}

		if os.Getenv(envDaemonChild) != "" {
			detachFromTerminal()
		}

		shouldWatchdog := (daemonFlag || watchdogFlag) && os.Getenv(envWorker) == ""
		if shouldWatchdog {
			runWatchdog()
			return nil // unreachable — runWatchdog loops forever
		}

		sec := resolveSecret(secretFlag)
		if sec == "" {
			return fmt.Errorf("no secret provided")
		}

		logger := log.New(os.Stderr, appName+": ", log.LstdFlags)
		if !verboseFlag {
			logger.SetOutput(os.Stderr)
		}

		var opts []gsocket.PeerOption
		if execCmdFlag != "" {
			opts = append(opts, gsocket.WithExecCmd(execCmdFlag))
		}
		if interactiveFlag {
			opts = append(opts, gsocket.WithInteractive())
		}
		if targetAddrFlag != "" {
			opts = append(opts, gsocket.WithTargetAddr(targetAddrFlag))
		}
		if listenPortFlag != "" {
			opts = append(opts, gsocket.WithListenAddr(listenPortFlag))
		}
		if socksServerFlag {
			opts = append(opts, gsocket.WithSOCKSServer())
		}
		if useUDPFlag {
			opts = append(opts, gsocket.WithUDP())
		}
		if socksAddr := resolveSOCKS5Addr(useTorFlag); socksAddr != "" {
			opts = append(opts, gsocket.WithSOCKS5Proxy(socksAddr))
		}
		if interactiveFlag || execCmdFlag != "" || targetAddrFlag != "" || socksServerFlag || listenPortFlag != "" {
			opts = append(opts, gsocket.WithMultiPeer())
		}
		opts = append(opts, gsocket.WithLogger(logger))

		if waitFlag {
			opts = append(opts, gsocket.WithSockWait())
		}
		if listenFlag {
			runListener(sec, opts)
		} else {
			runClient(sec, opts, interactiveFlag)
		}
		return nil
	}

	// Windows: if running as a service, enter the service dispatcher loop.
	// The service handler calls realMain() in a goroutine.
	if isWindowsService() {
		if err := runAsService(); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := realMain(); err != nil {
		log.Fatal(err)
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

	sessionCount := 0

	// Accept the first client (retry on transient failures).
	var sharedToken [gsocket.TokenSize]byte
	var firstPeer *gsocket.Peer
	for ctx.Err() == nil {
		firstPeer = gsocket.NewPeer(secret, gsocket.RoleServer, opts...)
		if err := firstPeer.AcceptOnListener(ctx); err != nil {
			if ctx.Err() != nil {
				firstPeer.Close()
				return
			}
			delay := 3 * time.Second
			log.Printf("Accept failed: %v — retrying in %v...", err, delay.Round(time.Second))
			firstPeer.Close()
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			continue
		}
		break // success
	}
	if ctx.Err() != nil {
		return
	}

	// Save the shared token for subsequent registrations (C's multi-sox).
	if fc := firstPeer.GSClient(); fc != nil {
		sharedToken = fc.Token()
		opts = append(opts, gsocket.WithToken(sharedToken))
	}

	sessionCount++
	log.Printf("Client connected [#%d]", sessionCount)

	// Handle the first session in a goroutine so we can pre-open the
	// next listen connection while this session runs. When ctx is
	// cancelled, close the peer to unblock RunShell immediately.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer firstPeer.Close()
		if err := firstPeer.RunShell(); err != nil {
			log.Printf("Session ended: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		firstPeer.Close()
	}()

	// Accept loop: keep one spare listen connection open so the next
	// client can connect immediately even while a session is active.
	// Matches C's multi-sox: multiple GSRN connections share one token.
loop:
	for ctx.Err() == nil {
		// Open the next listen connection while the current session runs.
		nextPeer := gsocket.NewPeer(secret, gsocket.RoleServer, opts...)
		// sharedToken is passed via WithToken in opts — no need to set manually.

		if err := nextPeer.AcceptOnListener(ctx); err != nil {
			if ctx.Err() != nil {
				nextPeer.Close()
				break
			}
			log.Printf("Accept failed: %v — retrying in 3s...", err)
			nextPeer.Close()
			select {
			case <-ctx.Done():
				break loop
			case <-time.After(3 * time.Second):
			}
			continue
		}

		// Save token from first successful registration and add to opts.
		if nc := nextPeer.GSClient(); nc != nil && sharedToken == [gsocket.TokenSize]byte{} {
			sharedToken = nc.Token()
			opts = append(opts, gsocket.WithToken(sharedToken))
		}

		// Wait for the previous session to end, then start this one.
		<-done
		sessionCount++
		log.Printf("Client connected [#%d]", sessionCount)

		done = make(chan struct{})
		go func(p *gsocket.Peer) {
			defer close(done)
			defer p.Close()
			if err := p.RunShell(); err != nil {
				log.Printf("Session ended: %v", err)
			}
		}(nextPeer)

	}

	// Wait for the last session to end (with timeout on shutdown).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("Timed out waiting for session to end.")
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
