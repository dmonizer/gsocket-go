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
with a modern ECDH-X25519 + HKDF + AES-256-GCM handshake, providing the
same security properties (mutual authentication, forward secrecy) without
any C dependencies.
*/
package gsocket
