# gsocket-go

Pure Go implementation of the [Global Socket Toolkit](https://github.com/hackerschoice/gsocket) — connect TCP pipes between peers behind NAT/firewalls using only a shared secret.

**Go-native rewrite.** Runs on **Windows**, Linux, and macOS.

## Features

- **End-to-end encrypted** — CPace over X25519 + AES-256-GCM
- **Mutual authentication** — both peers prove knowledge of the shared secret via HMAC
- **Forward secrecy** — ephemeral keys per session
- **GSRN compatible** — uses the same Global Socket Relay Network (gsocket.org)
- **Interactive shell** — remote command shell through any firewall
- **TCP forwarding** — tunnel any TCP connection through GSRN
- **SOCKS5 / TOR support** — route through TOR for anonymity
- **Single binary** — `go build` produces one static executable

## Differences from the original C implementation

| | C (gsocket) | Go (gsocket-go) |
|---|---|---|
| Encryption | TLS-SRP (RFC 5054) + AES-256-CBC | CPace (RFC 9383) over X25519 + AES-256-GCM |
| Key exchange | SRP with 4096-bit prime | Ephemeral X25519 + HKDF |
| Build | autotools, OpenSSL dep | `go build`, zero C deps |
| Windows | Cygwin only | Native (no DLLs needed) |
| Interop with C clients | ✅ | ❌ (different crypto, same GSRN) |

Go and C clients can coexist on the same GSRN but cannot talk directly to each other — they use different encryption protocols on top of the same relay.

## Installation

### Prebuilt binaries

Every push to `main` publishes a GitHub prerelease named `main-<commit SHA>`
with all 11 Makefile targets and a `SHA256SUMS` file. Asset names include the
platform, architecture, and `oldwin` suffix for Windows 7 compatible builds.

### From source

```bash
git clone https://github.com/hackerschoice/gsocket-go.git
cd gsocket-go
go build ./cmd/gs-netcat
```

### Cross-compile for Windows

```bash
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o gs-netcat.exe ./cmd/gs-netcat
```

### Cross-compile for macOS

```bash
GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o gs-netcat-darwin ./cmd/gs-netcat
```

## Quick Start

### Interactive shell

**Server** (behind NAT, listens for connections):
```bash
gs-netcat -l -i -s MySecret
```

**Client** (anywhere, connects to server):
```bash
gs-netcat -i -s MySecret
```

Press `Ctrl-E` then `↓` to open the console. Use `put <path>` to upload and
`get <pattern>` to download; `lcd <dir>` changes the client's transfer
directory and `lpwd` shows it. `Ctrl-E` then `↑` returns focus to the shell.
Transfers support wildcards, recursive directories, resume, and C's `/./`
path marker (for example, `put /tmp/./reports` creates `reports` remotely).
On Unix, transfer patterns also accept C-style command substitution such as
`$(find ...)`. A remote `get` request evaluates its pattern on the server, so
only connect to peers you trust to run commands as the server user.

### TCP forwarding

**Server** (expose port 22 through GSRN):
```bash
gs-netcat -l -d 127.0.0.1:22 -s MySecret
```

**Client** (listen locally on port 2222, forwarded to server's port 22):
```bash
gs-netcat -p 2222 -s MySecret
```

Then on the client machine:
```bash
ssh -p 2222 root@127.0.0.1
```

### Execute a command on connection

```bash
gs-netcat -l -e "cmd.exe" -s MySecret   # Windows server
gs-netcat -l -e "/bin/bash" -s MySecret  # Linux server
```

### Use TOR

```bash
gs-netcat -l -i --tor -s MySecret  # Server via TOR
gs-netcat -i --tor -s MySecret     # Client via TOR
```

### Secret from environment

```bash
export GSOCKET_SECRET="MySecret"
gs-netcat -l -i
```

## Architecture

```
┌──────────────────────────────────────────┐
│             cmd/gs-netcat/main.go         │
│  CLI flags, mode dispatch                │
├──────────────────────────────────────────┤
│             gsocket/peer.go               │
│  Connection lifecycle, stream relay      │
├──────────────┬───────────────────────────┤
│ appproto.go  │  channel.go               │
│ In-band      │  ECDH + AES-256-GCM        │
│ signaling    │  (replaces TLS-SRP)        │
├──────────────┴───────────────────────────┤
│             gsocket/gsrn.go               │
│  GSRN binary protocol (listen/connect/   │
│  ping/pong/start/status)                 │
├──────────────────────────────────────────┤
│             gsocket/addr.go               │
│  SHA256-based address derivation from    │
│  shared secret (matches original C impl) │
└──────────────────────────────────────────┘
```

### Protocol flow

1. **Address derivation** — both peers derive the same 128-bit GS address from the shared secret via SHA256
2. **GSRN connection** — both peers connect to `<letter>.gsocket.org:443` and exchange binary protocol messages
3. **GSRN handoff** — GSRN connects the two TCP streams and sends `_gs_start`
4. **Key exchange** — peers exchange ephemeral X25519 public keys, authenticated by HMAC(baseKey, pubkey, domain)
5. **Encrypted channel** — all subsequent data is AES-256-GCM encrypted
6. **Application data** — in-band escape sequences multiplex shell control messages within the encrypted stream

### Cryptographic details

```
baseKey = hex(SHA256("/kd/srp/1" + secret))          # 64 hex chars (matches C srp_password)
gsAddr  = SHA256("/kd/addr/2" + secret)[0:16]        # 16 bytes (matches C gs_addr)

Handshake:
  Client → Server: [x25519_pub (32)] [HMAC-SHA256(baseKey, pub || "gsocket-auth-client") (16)]
  Server → Client: [x25519_pub (32)] [HMAC-SHA256(baseKey, pub || "gsocket-auth-server") (16)]

  ecdh_secret = X25519(client_priv, server_pub) = X25519(server_priv, client_pub)
  session_key = HKDF-SHA256(baseKey || ecdh_secret, salt="gsocket-session") (32 bytes)

Data: AES-256-GCM(session_key, 12-byte-nonce=counter, plaintext)
      Framed as [2-byte length][12-byte nonce][ciphertext+tag]
```

## Project structure

```
gsocket-go/
├── cmd/
│   └── gs-netcat/
│       └── main.go          # CLI entry point
├── gsocket/
│   ├── addr.go              # Address derivation
│   ├── addr_test.go
│   ├── gsrn.go              # GSRN protocol
│   ├── gsrn_test.go
│   ├── channel.go           # Secure channel (ECDH + AES-GCM)
│   ├── channel_test.go
│   ├── appproto.go          # Application protocol
│   ├── appproto_test.go
│   ├── peer.go              # Peer connection manager
│   ├── socks.go             # SOCKS5 proxy support
│   └── doc.go               # Package documentation
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## Building and testing

```bash
# Build
go build ./...

# Run tests
go test ./gsocket/...

# Run tests with verbose output
go test -v ./gsocket/...

# Cross-compile for all platforms
make all
```

## License

BSD 2-Clause — same as the original gsocket project.

## Security

This is a community port. The cryptographic design replaces TLS-SRP with CPace over X25519 and AES-256-GCM. Both approaches provide mutual authentication, forward secrecy, and data confidentiality. If you discover a vulnerability, please report it responsibly.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

Co-Authored-By: Claude <noreply@anthropic.com>
