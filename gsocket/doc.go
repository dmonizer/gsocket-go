/*
Package gsocket implements the Global Socket Toolkit in pure Go.

It allows two peers behind NAT/firewalls to establish an encrypted TCP
connection via the Global Socket Relay Network (GSRN) using only a shared
secret — no IP addresses, port forwarding, or PKI required.

Quick start:

	// Server (listener):
	peer := gsocket.NewPeer("MySecret", gsocket.RoleServer)
	peer.AcceptOnListener(context.Background())
	peer.RunShell()

	// Client:
	peer := gsocket.NewPeer("MySecret", gsocket.RoleClient)
	peer.DialAndConnect(context.Background())
	peer.RunShell()

The package replaces the original C library's TLS-SRP (RFC 5054) encryption
with CPace (RFC 9383) — a balanced Password-Authenticated Key Exchange over
X25519 — followed by AES-256-GCM. This provides mutual authentication,
forward secrecy, and offline dictionary attack resistance without any C
dependencies.
*/
package gsocket
