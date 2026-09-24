# Cryptographic Security Analysis: Go vs C gsocket

## Scope

Comparison of the encrypted channel protocols in the Go implementation
(`gsocket-go`, this repo) and the original C implementation
(`gsocket` / gs-netcat). The analysis focuses on whether the Go version's
switch from TLS-SRP (RFC 5054) to X25519+HKDF+AES-256-GCM introduces
man-in-the-middle vulnerabilities or enables eavesdropping — on the wire
or at the GSRN relay server.

## Architecture Overview

Both implementations share the same **GSRN relay network** for peer
rendezvous (the relay matches two peers by a 128-bit address derived from
the shared secret), but differ entirely in the encrypted channel layer
that runs over the bridged TCP connection.

| Layer | C Implementation | Go Implementation |
|---|---|---|
| Key Exchange | TLS-SRP (RFC 5054), 4096-bit prime | Ephemeral X25519 ECDH |
| Authentication | SRP zero-knowledge proof | HMAC-SHA256 of public key + role domain tag |
| Encryption | AES-256-CBC-SHA (OpenSSL cipher suite) | AES-256-GCM (AEAD, stdlib) |
| Key Derivation | OpenSSL SRP internals + `GS_srp_setpassword()` | HKDF-SHA256 |
| Wire Compatibility | N/A | **Not compatible** with C (by design) |

## Key Derivation (Identical in Both)

Both derive the same intermediate materials from the shared secret
(`gsocket/addr.go:91-114`, `gsocket-util.c:328-350`):

```
srpPassword = hex(SHA256("/kd/srp/1" + secret))   → 64 hex chars (32 bytes raw)
gsAddr      = SHA256("/kd/addr/2" + secret)[0:16] → 16 bytes
```

- **`gsAddr`** determines which GSRN relay host to use (`a.gs.thc.org`
  through `z.gs.thc.org`) and identifies the rendezvous point.
- **`srpPassword`** is the SRP password in C; in Go, its raw bytes are
  used as `baseKey` — the HMAC key for peer authentication.

## Go Handshake — Step by Step

Reference: `gsocket/channel.go`, function `Handshake()` (lines 52–89).

### 1. Ephemeral Key Generation

```go
privKey, pubKey := generateX25519Keypair()  // crypto/rand
```

Each peer generates a fresh X25519 keypair per session. The private key
is never stored or transmitted — this provides **forward secrecy**.

### 2. Authenticated Key Exchange

```
Alice → Bob:  pubKey_A (32 bytes) || HMAC-SHA256(baseKey, pubKey_A, "gsocket-auth-client")[:16]
Bob → Alice:   pubKey_B (32 bytes) || HMAC-SHA256(baseKey, pubKey_B, "gsocket-auth-server")[:16]
```

Each peer:
1. Sends their public key with an authentication tag (48 bytes total).
2. Receives the peer's public key (32 bytes).
3. Receives the peer's auth tag (16 bytes).
4. Verifies `received_tag == expected_tag` using `hmac.Equal()`
   (constant-time comparison, prevents timing side-channels).
5. Aborts with `ErrAuthFailed` on mismatch.

### 3. Shared Secret Computation

```go
ecdhSecret = X25519(ourPriv, theirPub)      // 32 bytes
sessionKey = HKDF-SHA256(                   // 32 bytes
    IKM:  baseKey (64 bytes hex) || ecdhSecret (32 bytes),
    salt: "gsocket-session",
    info: nil,
)
```

### 4. Secure Channel

```go
aesgcm, _ := cipher.NewGCM(aes.NewCipher(sessionKey))
return &SecureChannel{conn: conn, aesgcm: aesgcm}
```

All subsequent data is AES-256-GCM encrypted with deterministic
counter-based nonces:

```
nonce: [0x00, 0x00, 0x00, 0x00, counter_be_uint64]
frame: [2-byte len] [ciphertext] [16-byte GCM auth tag]
```

Separate counters per direction (`sendCtr`, `recvCtr`) starting at 0 with
a fresh session key each handshake — no nonce reuse risk.

### Diagrams

```
┌──────────┐                          ┌──────────┐
│  Alice   │────── GSRN Relay ────────│   Bob    │
│ (client) │    (bridges TCP after    │ (server) │
│          │     GSRN handshake)      │          │
└────┬─────┘                          └────┬─────┘
     │                                     │
     │  pubKey_A || HMAC(baseKey, A, "client")[:16]
     │────────────────────────────────────→│
     │                                     │
     │  pubKey_B                            │
     │←────────────────────────────────────│
     │                                     │
     │  HMAC(baseKey, B, "server")[:16]    │
     │←────────────────────────────────────│
     │                                     │
     │  ═══ AES-256-GCM encrypted ═══════  │
     │  (session key via HKDF-SHA256)      │
     │←───────────────────────────────────→│
```

## Threat Analysis

### Can the GSRN Relay perform MITM?

**No.**

For a classic MITM, the relay would need to substitute its own X25519
public key for each peer's key and compute a valid authentication tag:

```
tag_forged = HMAC-SHA256(baseKey, relayPubKey, domainTag)[:16]
```

This requires `baseKey = hex(SHA256("/kd/srp/1" + secret))`. The relay
does **not** know the shared secret. It only sees:

- `gsAddr = SHA256("/kd/addr/2" + secret)[0:16]` — a different derivation
  domain; SHA-256 preimage resistance prevents inverting this to recover
  `secret` or `baseKey`.
- The X25519 public keys — public by design.
- The HMAC tags — useless without `baseKey`.

Without the secret, the relay cannot forge a valid auth tag. Any
substituted key is detected by `verifyPeerAuth()` and the connection is
aborted.

**Verified by test:** `channel_test.go:137-157` (`TestHandshakeWrongSecret`)
— mismatched secrets always produce `ErrAuthFailed`.

### Can a Network Eavesdropper perform MITM?

**No.** Same reasoning as above. A passive eavesdropper sees the same
handshake messages but cannot forge auth tags without the secret.

### Can the Relay Eavesdrop on Traffic?

**No.** After the handshake, all data is AES-256-GCM encrypted with a
session key derived from both `baseKey` (known only to peers) and the
ECDH shared secret (ephemeral, per-session). The relay sees:

- GSRN protocol packets (address, token, flags) — plaintext, but not
  useful for decryption.
- The X25519 public keys — public information.
- AES-256-GCM ciphertext — indistinguishable from random without the key.

### Can Traffic Be Eavesdropped on the Wire?

**No.** AES-256-GCM provides confidentiality. Without the session key,
the ciphertext is opaque. A network eavesdropper sees even less than the
relay (no GSRN metadata).

### Ciphertext Manipulation?

**No.** GCM is an AEAD mode — every frame carries a 16-byte
authentication tag. Any tampering with the ciphertext is detected by
`aesgcm.Open()` and causes a decrypt error. The connection is effectively
torn down on the first tampered frame.

## Weakness: Offline Dictionary Attack

This is the **only** meaningful security difference from the C
implementation.

### The Problem

The HMAC authentication tags are transmitted in cleartext during the
handshake:

```
HMAC-SHA256(baseKey, pubKey, "gsocket-auth-client")[:16]
HMAC-SHA256(baseKey, pubKey, "gsocket-auth-server")[:16]
```

An eavesdropper who captures these can perform an **offline dictionary
attack**:

1. Guess a candidate secret S.
2. Compute `baseKey' = hex(SHA256("/kd/srp/1" + S))`.
3. Compute `HMAC-SHA256(baseKey', pubKey, domainTag)[:16]`.
4. Compare with the observed tag. Match → secret found.

Each guess costs ~1 SHA-256 + ~1 HMAC-SHA256 — microseconds on a CPU,
nanoseconds on a GPU. An attacker can test **billions of candidates per
second**.

### Why TLS-SRP Prevents This

TLS-SRP (RFC 5054) is a zero-knowledge password proof. Even after
capturing the complete SRP exchange, an attacker cannot verify password
guesses offline — each guess requires an online interaction with one of
the peers. SRP's design inherently resists offline dictionary attacks.

### Mitigating Factors

1.  **Strong secrets eliminate the risk.** If the secret has ≥128 bits of
    entropy (e.g. `gs-netcat -g` generates 16 random bytes → 32 hex
    chars), dictionary attacks are computationally infeasible regardless
    of the protocol.

2.  **The GSRN address has the same exposure in both versions.**
    `SHA256("/kd/addr/2" + secret)[0:16]` is transmitted in plaintext to
    the GSRN relay. An attacker who sees this 128-bit value can also
    perform offline dictionary attacks against it — this affects the C
    version too. Both versions leak a 128-bit secret derivative to the
    relay.

3.  **If you don't trust the relay**, the address exposure is already a
    concern. The HMAC tag exposure doesn't meaningfully worsen the
    situation — an attacker who can see the address already has a 128-bit
    hash target for dictionary attacks.

### Recommendation

Always use strong, randomly-generated secrets. The `-g` flag generates
128 bits of entropy:

```sh
gs-netcat -g   # generates and prints a random secret
```

Alternatively, use a long, high-entropy passphrase (≥20 random
characters).

## Other Security Properties

| Property | Status | Notes |
|---|---|---|
| **Forward Secrecy** | ✅ | Ephemeral X25519 keys discarded after handshake |
| **Reflection Attack Resistance** | ✅ | Domain tags `"gsocket-auth-server"` / `"gsocket-auth-client"` |
| **Replay Attack Resistance** | ✅ | Fresh ephemeral keys per session |
| **Ciphertext Integrity** | ✅ | AES-GCM built-in auth tag (16 bytes) |
| **Nonce Safety** | ✅ | Counter-based, per-direction, fresh key per session |
| **Timing Side-Channel Resistance** | ✅ | `hmac.Equal()` constant-time comparison |
| **Key Confirmation** | ✅ | Both sides prove key knowledge before data flows |
| **Length-Prefix Authentication** | ⚠️ | 2-byte frame length is outside GCM's AAD. Tampering causes GCM auth failure (safe), but message sizes are visible to observers (traffic analysis). |
| **Downgrade Protection** | ⚠️ | Protocol is fixed — no version negotiation, so no downgrade possible. But also no upgrade path for future cipher changes. |
| **Compromise Recovery** | ⚠️ | If the shared secret is compromised, *active* MITM becomes possible for future sessions. Past sessions remain secure (forward secrecy). |

## Comparison with C Implementation

| Threat / Property | C (TLS-SRP + AES-256-CBC) | Go (X25519 + AES-256-GCM) |
|---|---|---|
| Mutual Authentication | ✅ SRP ZKP | ✅ HMAC-SHA256 |
| Forward Secrecy | ✅ SRP ephemerals | ✅ Ephemeral X25519 |
| Offline Dictionary Attack Resistance | ✅ SRP is resistant | ❌ HMAC tags enable offline guessing |
| Ciphertext Integrity | ✅ SHA-1 HMAC (cipher suite) | ✅ GCM auth tag |
| AEAD (encrypt + auth in one pass) | ❌ CBC + separate HMAC | ✅ GCM |
| C Dependency | OpenSSL / GnuTLS | None (Go stdlib + `golang.org/x/crypto`) |
| Prime / Curve Size | 4096-bit SRP group | Curve25519 (~128-bit security) |

## Relay Server Visibility Summary

What the GSRN relay can see at each protocol layer:

| Layer | Visible to Relay | Useful for Attack? |
|---|---|---|
| GSRN handshake | Address, token, flags | Address links sessions by secret; token is random |
| Go crypto handshake | X25519 public keys, HMAC tags | Public keys are public; tags enable offline dictionary attack on weak secrets |
| Data channel | AES-256-GCM ciphertext | No — indistinguishable from random |
| Traffic analysis | Message sizes, timing | Yes — frame lengths are plaintext (2-byte prefix) |

## Conclusion

The Go protocol is **cryptographically sound** and does **not** introduce
MITM vulnerabilities. The HMAC-based peer authentication effectively
replaces TLS-SRP's mutual authentication — neither the relay server nor a
network eavesdropper can decrypt traffic or impersonate a peer.

The single security regression from the C implementation is the loss of
**offline dictionary attack resistance** — the HMAC authentication tags
are visible to passive observers and can be used to brute-force weak
secrets offline. This is a deliberate trade-off: a simpler,
dependency-free implementation at the cost of requiring strong secrets.

**For users:** Use `gs-netcat -g` to generate random 128-bit secrets, or
choose long, high-entropy passphrases. With strong secrets, the Go and C
implementations provide equivalent security in practice.

**For developers:** If offline dictionary attack resistance is desired in
a future version, the handshake could be upgraded to use a
password-authenticated key exchange (PAKE) such as OPAQUE, CPace, or
SPAKE2. These provide SRP-like dictionary attack resistance without
requiring OpenSSL.
