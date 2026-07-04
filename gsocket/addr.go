// Package gsocket implements the Global Socket toolkit in pure Go.
//
// It allows two peers behind NAT/firewalls to establish an encrypted TCP
// connection via the Global Socket Relay Network (GSRN) using a shared secret.
//
// The package replaces the original C library's TLS-SRP (RFC 5054) with a
// modern ECDH-X25519 + HKDF + AES-256-GCM handshake, giving the same security
// properties (mutual authentication, forward secrecy) in pure Go.
package gsocket

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

const (
	// AddrSize is the length of a GS address in bytes (128 bits).
	AddrSize = 16

	// TokenSize is the length of a GS token in bytes (128 bits).
	TokenSize = 16

	// SecretMaxLen is the maximum secret length in bytes.
	SecretMaxLen = 256 / 8
)

// Domain suffixes for deriving GSRN hostnames and keys.
const (
	domainKD1 = "/kd/srp/1"  // SRP key derivation domain
	domainKD2 = "/kd/addr/2" // Address derivation domain

	// defaultGSRNDomain matches the C implementation's GS_NET_DEFAULT_HOST.
	// Subdomains {a-z}.gs.thc.org are the GSRN relay servers.
	defaultGSRNDomain = "gs.thc.org"
	envGSRNDomain     = "GSOCKET_DOMAIN"
)

// Addr is a 128-bit GS address that identifies a listening peer on the GSRN.
type Addr [AddrSize]byte

// String returns the hex-encoded address.
func (a Addr) String() string {
	return hex.EncodeToString(a[:])
}

// HostnameID returns a letter a-z (0-25) used to select a GSRN relay host.
// It matches the original C implementation's GS_ADDR_get_hostname_id().
func (a Addr) HostnameID() byte {
	var sum int
	for _, b := range a {
		sum += int(b)
	}
	return byte(sum % 26)
}

// GSRNHostname returns the relay hostname for this address,
// e.g. "f.gs.thc.org" or "x.gs.thc.org".
//
// The domain can be overridden with the GSOCKET_DOMAIN environment variable,
// matching the C implementation's behaviour.
func (a Addr) GSRNHostname() string {
	id := a.HostnameID()
	domain := os.Getenv(envGSRNDomain)
	if domain == "" {
		domain = defaultGSRNDomain
	}
	return fmt.Sprintf("%c.%s", 'a'+id, domain)
}

// GSRNHostnames returns all 26 GSRN relay hostnames (a-z), starting from
// the primary hostname for this address and wrapping around. If the primary
// GSRN relay is unreachable, the caller can try the remaining hosts.
//
// E.g. for address with HostnameID=5 (f): [f.gs.thc.org, g.gs.thc.org, ..., z.gs.thc.org, a.gs.thc.org, ..., e.gs.thc.org]
func (a Addr) GSRNHostnames() []string {
	id := a.HostnameID()
	domain := os.Getenv(envGSRNDomain)
	if domain == "" {
		domain = defaultGSRNDomain
	}
	hosts := make([]string, 26)
	for i := 0; i < 26; i++ {
		letter := byte('a') + byte((int(id)+i)%26)
		hosts[i] = fmt.Sprintf("%c.%s", letter, domain)
	}
	return hosts
}

// DeriveKeyMaterial derives the SRP-equivalent password and GS address from
// a shared secret. It exactly matches the C function GS_ADDR_sec2addr().
//
//	srpPassword = hex(SHA256("/kd/srp/1" + secret))
//	gsAddr      = SHA256("/kd/addr/2" + secret)[0:16]
func DeriveKeyMaterial(secret string) (srpPassword string, addr Addr) {
	secretBytes := []byte(secret)

	// Derive SRP-equivalent password (used as base key in our protocol).
	h := sha256.New()
	h.Write([]byte(domainKD1))
	h.Write(secretBytes)
	md := h.Sum(nil)
	srpPassword = hex.EncodeToString(md[:])

	// Derive GS address.
	h.Reset()
	h.Write([]byte(domainKD2))
	h.Write(secretBytes)
	md = h.Sum(nil)
	copy(addr[:], md[:AddrSize])

	return
}

// DeriveToken derives a connection token from an optional auth string and
// the GS address. If auth is empty, the caller should use a random token.
//
//	token = SHA256(auth + gsAddr)[0:16]
func DeriveToken(auth string, addr Addr) [TokenSize]byte {
	var token [TokenSize]byte
	if auth == "" {
		return token
	}

	h := sha256.New()
	h.Write([]byte(auth))
	h.Write(addr[:])
	md := h.Sum(nil)
	copy(token[:], md[:TokenSize])
	return token
}
