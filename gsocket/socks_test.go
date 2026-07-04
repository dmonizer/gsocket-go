package gsocket

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// serveEcho listens on a random port and echoes back all received data.
// Returns the listener address.
func serveEcho(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()
	return l.Addr().String()
}

// socks5ClientHandshake performs a full SOCKS5 client handshake over rw
// and returns the established connection (rw itself after negotiation).
func socks5ClientHandshake(t *testing.T, rw io.ReadWriter, targetHost string, targetPort uint16) {
	t.Helper()

	// Greeting: ver=5, 1 method, NO AUTH.
	if _, err := rw.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}

	// Read server auth choice.
	resp := make([]byte, 2)
	if _, err := io.ReadFull(rw, resp); err != nil {
		t.Fatalf("read auth choice: %v", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("auth choice: got %02x %02x, want 05 00", resp[0], resp[1])
	}

	// Build CONNECT request.
	var req []byte
	ip := net.ParseIP(targetHost)
	if ip4 := ip.To4(); ip4 != nil {
		req = []byte{0x05, 0x01, 0x00, 0x01} // IPv4
		req = append(req, ip4...)
	} else {
		req = []byte{0x05, 0x01, 0x00, 0x03} // FQDN
		req = append(req, byte(len(targetHost)))
		req = append(req, []byte(targetHost)...)
	}
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, targetPort)
	req = append(req, port...)

	if _, err := rw.Write(req); err != nil {
		t.Fatalf("write connect request: %v", err)
	}

	// Read CONNECT reply header (4 bytes).
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(rw, hdr); err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	if hdr[1] != 0x00 {
		t.Fatalf("connect reply code: %d (want 0 = success)", hdr[1])
	}

	// Read remaining bind address bytes.
	switch hdr[3] {
	case 0x01: // IPv4
		io.ReadFull(rw, make([]byte, 4+2))
	case 0x03: // FQDN
		lenBuf := make([]byte, 1)
		io.ReadFull(rw, lenBuf)
		io.ReadFull(rw, make([]byte, int(lenBuf[0])+2))
	case 0x04: // IPv6
		io.ReadFull(rw, make([]byte, 16+2))
	}
}

func TestSOCKSServeSuccess(t *testing.T) {
	echoAddr := serveEcho(t)
	host, portStr, _ := net.SplitHostPort(echoAddr)
	port, _ := parsePort(portStr)

	// Create a pipe pair: serverEnd is the SOCKS5 proxy side,
	// clientEnd is what a SOCKS5 client connects to.
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	// Run SOCKSServe in a goroutine.
	var (
		targetConn net.Conn
		serveErr   error
		wg         sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		targetConn, serveErr = SOCKSServe(serverEnd)
	}()

	// Perform SOCKS5 client handshake on clientEnd.
	socks5ClientHandshake(t, clientEnd, host, uint16(port))

	wg.Wait()
	if serveErr != nil {
		t.Fatalf("SOCKSServe failed: %v", serveErr)
	}
	if targetConn == nil {
		t.Fatal("SOCKSServe returned nil connection")
	}
	defer targetConn.Close()

	// --- Test data relay ---
	// Write from client (through clientEnd) → target → back.
	// After the handshake, clientEnd is directly connected to the
	// SOCKS5 proxy. Data written to clientEnd goes to serverEnd,
	// which (after SOCKSServe returns) is NOT relayed by SOCKSServe
	// itself — the caller is responsible for relaying between
	// targetConn and clientEnd/serverEnd.
	//
	// We verify two things:
	// 1. The returned targetConn IS connected to the echo server.
	// 2. Data can be relayed through the returned targetConn.

	// Test 1: Write to targetConn, read echo back.
	msg := []byte("Hello SOCKS5 echo!")
	if _, err := targetConn.Write(msg); err != nil {
		t.Fatalf("write to target: %v", err)
	}
	echo := make([]byte, len(msg))
	if _, err := io.ReadFull(targetConn, echo); err != nil {
		t.Fatalf("read echo from target: %v", err)
	}
	if !bytes.Equal(echo, msg) {
		t.Errorf("echo mismatch: got %q, want %q", echo, msg)
	}

	// Test 2: Relay through the pipe bidirectionally.
	// After SOCKSServe returns, the caller relays between serverEnd
	// (the proxy's end of the client connection) and targetConn.
	// The SOCKS5 client on clientEnd sends/receives through the pipe.
	var rw sync.WaitGroup
	rw.Add(2)

	go func() {
		defer rw.Done()
		defer targetConn.Close()
		io.Copy(targetConn, serverEnd)
	}()
	go func() {
		defer rw.Done()
		defer serverEnd.Close()
		io.Copy(serverEnd, targetConn)
	}()

	// Client sends a message through the pipe → serverEnd → targetConn → echo → back.
	msg2 := []byte("Relay test!")
	if _, err := clientEnd.Write(msg2); err != nil {
		t.Fatalf("client write: %v", err)
	}

	reply := make([]byte, len(msg2))
	if _, err := io.ReadFull(clientEnd, reply); err != nil {
		t.Fatalf("client read echo: %v", err)
	}
	if !bytes.Equal(reply, msg2) {
		t.Errorf("relay echo mismatch: got %q, want %q", reply, msg2)
	}

	// Close clientEnd to signal EOF to relay goroutines.
	clientEnd.Close()
	rw.Wait()
}

func TestSOCKSServeUnsupportedCommand(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	var serveErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, serveErr = SOCKSServe(serverEnd)
	}()

	// Send greeting.
	clientEnd.Write([]byte{0x05, 0x01, 0x00})
	io.ReadFull(clientEnd, make([]byte, 2)) // auth choice

	// Send BIND command (0x02) instead of CONNECT (0x01).
	req := []byte{0x05, 0x02, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	clientEnd.Write(req)

	// Read error reply.
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(clientEnd, hdr); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if hdr[1] != socks5RepCmdNotSupported {
		t.Errorf("reply code = %d, want %d (command not supported)", hdr[1], socks5RepCmdNotSupported)
	}

	wg.Wait()
	if serveErr == nil {
		t.Error("expected error for unsupported command")
	}
}

func TestSOCKSServeBadVersion(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	var serveErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, serveErr = SOCKSServe(serverEnd)
	}()

	// Send wrong SOCKS version.
	clientEnd.Write([]byte{0x04, 0x01, 0x00})

	wg.Wait()
	if serveErr == nil {
		t.Error("expected error for wrong SOCKS version")
	}
}

func TestSOCKSServeHostUnreachable(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	var serveErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, serveErr = SOCKSServe(serverEnd)
	}()

	// Greeting.
	clientEnd.Write([]byte{0x05, 0x01, 0x00})
	io.ReadFull(clientEnd, make([]byte, 2))

	// CONNECT to a non-existent host (port 1 on 127.0.0.1 — should fail quickly).
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.IPv4(127, 0, 0, 1).To4()...)
	req = append(req, 0x00, 0x01) // port 1
	clientEnd.Write(req)

	// Read error reply.
	hdr := make([]byte, 4)
	io.ReadFull(clientEnd, hdr)

	wg.Wait()
	if serveErr == nil {
		t.Error("expected connection refused error")
	}
}

func TestDialSOCKS5Success(t *testing.T) {
	// Start a real SOCKS5 proxy server that proxies to an echo server.
	echoAddr := serveEcho(t)
	echoHost, echoPortStr, _ := net.SplitHostPort(echoAddr)
	echoPort, _ := parsePort(echoPortStr)

	// Start a mini SOCKS5 proxy on a random port.
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyListener.Close()

	proxyAddr := proxyListener.Addr().String()

	// Accept SOCKS5 clients and call SOCKSServe.
	go func() {
		for {
			clientConn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				target, err := SOCKSServe(conn)
				if err != nil {
					return
				}
				defer target.Close()
				// Relay.
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); io.Copy(target, conn) }()
				go func() { defer wg.Done(); io.Copy(conn, target) }()
				wg.Wait()
			}(clientConn)
		}
	}()

	// Use dialSOCKS5 to connect through the proxy.
	target := net.JoinHostPort(echoHost, fmt.Sprintf("%d", echoPort))
	conn, err := dialSOCKS5(proxyAddr, target)
	if err != nil {
		t.Fatalf("dialSOCKS5 failed: %v", err)
	}
	defer conn.Close()

	// Test the relay through the full chain: client → proxy → echo → back.
	msg := []byte("Full chain test message!")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(reply, msg) {
		t.Errorf("full chain echo: got %q, want %q", reply, msg)
	}
}

func TestDialSOCKS5BadProxy(t *testing.T) {
	// Connect to a port with no SOCKS5 proxy.
	conn, err := dialSOCKS5("127.0.0.1:1", "example.com:80")
	if err == nil {
		conn.Close()
		t.Error("expected error connecting to bad proxy")
	}
}

func TestSOCKSServeIPv6(t *testing.T) {
	echoAddr := serveEcho(t)
	_, portStr, _ := net.SplitHostPort(echoAddr)
	port, _ := parsePort(portStr)

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	var serveErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, serveErr = SOCKSServe(serverEnd)
	}()

	// Greeting + auth.
	clientEnd.Write([]byte{0x05, 0x01, 0x00})
	io.ReadFull(clientEnd, make([]byte, 2))

	// CONNECT to ::1 (IPv6 localhost) → should fail with host unreachable
	// because the echo server is on IPv4. But the parsing should work.
	req := []byte{0x05, 0x01, 0x00, 0x04} // IPv6
	req = append(req, net.IPv6loopback.To16()...)
	req = append(req, byte(port>>8), byte(port&0xff))
	clientEnd.Write(req)

	// Read reply (will be an error since IPv6 echo unlikely to be listening).
	hdr := make([]byte, 4)
	io.ReadFull(clientEnd, hdr)

	wg.Wait()
	// Either success (if IPv6 echo works) or host unreachable — both valid;
	// we just care that the IPv6 address type was parsed without panic.
	if serveErr != nil {
		t.Logf("IPv6 connect result (expected): %v", serveErr)
	}
}

func TestSOCKSServeFQDN(t *testing.T) {
	// Connect to the echo server by hostname "localhost".
	_, portStr, _ := net.SplitHostPort(serveEcho(t))
	port, _ := parsePort(portStr)

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	var (
		targetConn net.Conn
		serveErr   error
		wg         sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		targetConn, serveErr = SOCKSServe(serverEnd)
	}()

	// Greeting + auth.
	clientEnd.Write([]byte{0x05, 0x01, 0x00})
	io.ReadFull(clientEnd, make([]byte, 2))

	// CONNECT with FQDN "localhost".
	host := "localhost"
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port&0xff))
	clientEnd.Write(req)

	// Read success reply.
	hdr := make([]byte, 4)
	io.ReadFull(clientEnd, hdr)
	if hdr[1] != 0x00 {
		t.Fatalf("FQDN connect failed with code %d (may be DNS issue in test env)", hdr[1])
	}

	// Read bind address (should be IPv4).
	switch hdr[3] {
	case 0x01:
		io.ReadFull(clientEnd, make([]byte, 4+2))
	case 0x03:
		lenBuf := make([]byte, 1)
		io.ReadFull(clientEnd, lenBuf)
		io.ReadFull(clientEnd, make([]byte, int(lenBuf[0])+2))
	}

	wg.Wait()
	if serveErr != nil {
		t.Fatalf("SOCKSServe with FQDN failed: %v", serveErr)
	}
	defer targetConn.Close()

	// Verify relay.
	msg := []byte("FQDN test!")
	targetConn.Write(msg)
	reply := make([]byte, len(msg))
	io.ReadFull(targetConn, reply)
	if !bytes.Equal(reply, msg) {
		t.Errorf("FQDN echo: got %q, want %q", reply, msg)
	}
}

func TestSendSocks5Reply(t *testing.T) {
	tests := []struct {
		name     string
		rep      uint8
		bindIP   net.IP
		bindPort int
	}{
		{"success IPv4", socks5RepSucceeded, net.IPv4(127, 0, 0, 1), 12345},
		{"host unreachable", socks5RepHostUnreachable, net.IPv4zero, 0},
		{"IPv6 bind", socks5RepSucceeded, net.IPv6loopback, 443},
		{"nil bind defaults to 0.0.0.0", socks5RepSucceeded, nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := sendSocks5Reply(&buf, tt.rep, tt.bindIP, tt.bindPort)
			if err != nil {
				t.Fatalf("sendSocks5Reply: %v", err)
			}
			data := buf.Bytes()
			if len(data) < 10 {
				t.Errorf("reply too short: %d bytes", len(data))
			}
			if data[0] != socks5Version {
				t.Errorf("version = %d, want %d", data[0], socks5Version)
			}
			if data[1] != tt.rep {
				t.Errorf("rep = %d, want %d", data[1], tt.rep)
			}
		})
	}
}

func TestSOCKSServeTimeout(t *testing.T) {
	// Test that SOCKSServe doesn't hang forever on a silent client.
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := SOCKSServe(serverEnd)
		errCh <- err
	}()

	// Send just the greeting then close — should error quickly.
	clientEnd.Write([]byte{0x05, 0x01, 0x00})
	time.Sleep(50 * time.Millisecond)
	clientEnd.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected error when client disconnects mid-handshake")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SOCKSServe hung after client disconnect")
	}
}

// parsePort parses a port string to int.
func parsePort(s string) (int, error) {
	var p int
	_, err := fmt.Sscanf(s, "%d", &p)
	return p, err
}
