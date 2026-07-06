package gsocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKS5 constants.
const (
	socks5Version      = 0x05
	socks5CmdConnect   = 0x01
	socks5AddrTypeIPv4 = 0x01
	socks5AddrTypeFQDN = 0x03
	socks5AddrTypeIPv6 = 0x04
	socks5AuthNone     = 0x00

	// SOCKS5 reply codes.
	socks5RepSucceeded        = 0x00
	socks5RepGeneralFailure   = 0x01
	socks5RepConnNotAllowed   = 0x02
	socks5RepNetUnreachable   = 0x03
	socks5RepHostUnreachable  = 0x04
	socks5RepConnRefused      = 0x05
	socks5RepTTLExpired       = 0x06
	socks5RepCmdNotSupported  = 0x07
	socks5RepAddrNotSupported = 0x08
)

// dialSOCKS5 establishes a TCP connection through a SOCKS5 proxy.
func dialSOCKS5(proxyAddr, targetAddr string) (net.Conn, error) {
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial SOCKS5 proxy %s: %w", proxyAddr, err)
	}

	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse target address %s: %w", targetAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse port %s: %w", portStr, err)
	}

	// SOCKS5 handshake: method negotiation.
	if _, err := conn.Write([]byte{socks5Version, 1, socks5AuthNone}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 auth request: %w", err)
	}

	resp := make([]byte, 2)
	if _, err := conn.Read(resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 auth response: %w", err)
	}
	if resp[0] != socks5Version || resp[1] != socks5AuthNone {
		conn.Close()
		return nil, errors.New("SOCKS5: no acceptable auth method")
	}

	// SOCKS5 handshake: connect request.
	ip := net.ParseIP(host)
	var req []byte
	if ip != nil && ip.To4() != nil {
		req = append([]byte{socks5Version, socks5CmdConnect, 0x00, socks5AddrTypeIPv4},
			ip.To4()...)
	} else {
		req = append([]byte{socks5Version, socks5CmdConnect, 0x00, socks5AddrTypeFQDN, byte(len(host))},
			[]byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 connect request: %w", err)
	}

	// Read connect response.
	resp = make([]byte, 4)
	if _, err := conn.Read(resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5 connect response: %w", err)
	}
	if resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS5: connection failed (code=%d)", resp[1])
	}

	// Read remaining address bytes based on address type.
	switch resp[3] {
	case socks5AddrTypeIPv4:
		if _, err := readFull(conn, make([]byte, 4+2)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("SOCKS5 read bind addr: %w", err)
		}
	case socks5AddrTypeFQDN:
		lenBuf := make([]byte, 1)
		if _, err := conn.Read(lenBuf); err != nil {
			conn.Close()
			return nil, err
		}
		if _, err := readFull(conn, make([]byte, int(lenBuf[0])+2)); err != nil {
			conn.Close()
			return nil, err
		}
	default:
		conn.Close()
		return nil, fmt.Errorf("SOCKS5: unsupported address type %d", resp[3])
	}

	return conn, nil
}

func readFull(conn net.Conn, buf []byte) ([]byte, error) {
	n := 0
	for n < len(buf) {
		nn, err := conn.Read(buf[n:])
		if err != nil {
			return buf[:n], err
		}
		n += nn
	}
	return buf, nil
}

// SOCKSServe handles the server side of a SOCKS5 CONNECT handshake over rw.
// It parses the client's greeting and connect request, connects to the
// requested target, and returns the established connection.
//
// On success the returned net.Conn carries the SOCKS5 success reply that has
// already been sent back to the client via rw. The caller should relay data
// between the returned conn and the original rw stream.
func SOCKSServe(rw io.ReadWriter) (net.Conn, error) {
	// --- Step 1: Read greeting ---
	// [version(1), nmethods(1), methods(nmethods)]
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(rw, greeting); err != nil {
		return nil, fmt.Errorf("SOCKS5 read greeting: %w", err)
	}
	if greeting[0] != socks5Version {
		// Drain remaining greeting bytes (methods list) to avoid
		// deadlocking synchronous transports like net.Pipe.
		drainReader(rw, int(greeting[1]))
		return nil, fmt.Errorf("SOCKS5: unsupported version %d", greeting[0])
	}
	nmethods := int(greeting[1])
	methods := make([]byte, nmethods)
	if nmethods > 0 {
		if _, err := io.ReadFull(rw, methods); err != nil {
			return nil, fmt.Errorf("SOCKS5 read methods: %w", err)
		}
	}

	// --- Step 2: Send auth choice (no auth) ---
	if _, err := rw.Write([]byte{socks5Version, socks5AuthNone}); err != nil {
		return nil, fmt.Errorf("SOCKS5 write auth choice: %w", err)
	}

	// --- Step 3: Read CONNECT request ---
	// [version(1), cmd(1), reserved(1), addrtype(1), addr(variable), port(2)]
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(rw, hdr); err != nil {
		return nil, fmt.Errorf("SOCKS5 read connect request: %w", err)
	}
	if hdr[0] != socks5Version {
		return nil, fmt.Errorf("SOCKS5: bad version in connect request: %d", hdr[0])
	}
	if hdr[1] != socks5CmdConnect {
		// Drain remaining connect request bytes before replying.
		// Without this, synchronous transports (e.g. net.Pipe in tests)
		// deadlock: the pipe still has unread address+port bytes, so
		// writing the reply blocks waiting for a reader that's also
		// blocked trying to finish writing the request.
		drainSocks5Addr(rw, hdr[3])
		sendSocks5Reply(rw, socks5RepCmdNotSupported, nil, 0)
		return nil, fmt.Errorf("SOCKS5: unsupported command %d", hdr[1])
	}

	var targetAddr string
	switch hdr[3] {
	case socks5AddrTypeIPv4:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(rw, addr); err != nil {
			return nil, fmt.Errorf("SOCKS5 read IPv4 addr: %w", err)
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(rw, port); err != nil {
			return nil, fmt.Errorf("SOCKS5 read IPv4 port: %w", err)
		}
		targetAddr = net.JoinHostPort(
			net.IP(addr).String(),
			strconv.Itoa(int(binary.BigEndian.Uint16(port))),
		)

	case socks5AddrTypeFQDN:
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(rw, lenByte); err != nil {
			return nil, fmt.Errorf("SOCKS5 read FQDN length: %w", err)
		}
		host := make([]byte, int(lenByte[0]))
		if _, err := io.ReadFull(rw, host); err != nil {
			return nil, fmt.Errorf("SOCKS5 read FQDN: %w", err)
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(rw, port); err != nil {
			return nil, fmt.Errorf("SOCKS5 read FQDN port: %w", err)
		}
		targetAddr = net.JoinHostPort(
			string(host),
			strconv.Itoa(int(binary.BigEndian.Uint16(port))),
		)

	case socks5AddrTypeIPv6:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(rw, addr); err != nil {
			return nil, fmt.Errorf("SOCKS5 read IPv6 addr: %w", err)
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(rw, port); err != nil {
			return nil, fmt.Errorf("SOCKS5 read IPv6 port: %w", err)
		}
		targetAddr = net.JoinHostPort(
			net.IP(addr).String(),
			strconv.Itoa(int(binary.BigEndian.Uint16(port))),
		)

	default:
		sendSocks5Reply(rw, socks5RepAddrNotSupported, nil, 0)
		return nil, fmt.Errorf("SOCKS5: unsupported address type %d", hdr[3])
	}

	// --- Step 4: Connect to the target ---
	target, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		sendSocks5Reply(rw, socks5RepHostUnreachable, nil, 0)
		return nil, fmt.Errorf("SOCKS5 connect to %s: %w", targetAddr, err)
	}

	// --- Step 5: Send success reply with bind address ---
	tcpAddr := target.LocalAddr().(*net.TCPAddr)
	if err := sendSocks5Reply(rw, socks5RepSucceeded, tcpAddr.IP, tcpAddr.Port); err != nil {
		target.Close()
		return nil, fmt.Errorf("SOCKS5 write success reply: %w", err)
	}

	return target, nil
}

// drainSocks5Addr reads and discards the variable-length address and port
// bytes that follow a SOCKS5 request header. addrType is the ATYP field
// from byte 3 of the request header.
func drainSocks5Addr(rw io.Reader, addrType byte) {
	switch addrType {
	case socks5AddrTypeIPv4:
		io.ReadFull(rw, make([]byte, 4+2)) // 4-byte IPv4 + 2-byte port
	case socks5AddrTypeFQDN:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(rw, lenBuf); err != nil {
			return
		}
		io.ReadFull(rw, make([]byte, int(lenBuf[0])+2)) // domain + port
	case socks5AddrTypeIPv6:
		io.ReadFull(rw, make([]byte, 16+2)) // 16-byte IPv6 + 2-byte port
	}
}

// drainReader reads and discards up to n bytes from r. It stops early on
// any error (including EOF) and does not return an error — best-effort only.
func drainReader(r io.Reader, n int) {
	for i := 0; i < n; i++ {
		var b [1]byte
		if _, err := r.Read(b[:]); err != nil {
			return
		}
	}
}

// sendSocks5Reply sends a SOCKS5 reply packet through w.
func sendSocks5Reply(w io.Writer, rep uint8, bindIP net.IP, bindPort int) error {
	// Default to 0.0.0.0:0 if no address specified.
	if bindIP == nil {
		bindIP = net.IPv4zero
	}
	reply := []byte{socks5Version, rep, 0x00}
	if ip4 := bindIP.To4(); ip4 != nil {
		reply = append(reply, socks5AddrTypeIPv4)
		reply = append(reply, ip4...)
	} else {
		reply = append(reply, socks5AddrTypeIPv6)
		reply = append(reply, bindIP.To16()...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(bindPort))
	reply = append(reply, portBytes...)

	_, err := w.Write(reply)
	return err
}
