package gsocket

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// SOCKS5 constants.
const (
	socks5Version     = 0x05
	socks5CmdConnect  = 0x01
	socks5AddrTypeIPv4 = 0x01
	socks5AddrTypeFQDN = 0x03
	socks5AuthNone    = 0x00
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
