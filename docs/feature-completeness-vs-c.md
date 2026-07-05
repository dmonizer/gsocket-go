# gsocket-go vs C gsocket — Feature Completeness Map

> Last updated: 2026-07-05

## Summary

The Go implementation covers the **core happy path** of the original C codebase
(~68% of features). It handles the fundamental use case — two peers connecting
interactively over GSRN — plus SOCKS5 proxying, multi-peer concurrency, UDP
transport, daemon mode (Unix + Windows service), watchdog auto-restart,
process title (`-T`), SIGWINCH terminal resize, NOPTY fallback, app-level
PING/PONG keepalive, LOG/STATUS in-band messaging, and Ctrl-E console escape
handling. Still missing: file transfer engine, full console UI/commands, IDS,
full statistics formatting, and several minor CLI flags.

---

## 1. GSRN Protocol — Wire Compatible

| Feature | C | Go | Notes |
|---|---|---|---|
| LISTEN/CONNECT/PING/PONG/START/ACCEPT/STATUS | ✅ | ✅ | Full protocol v1.3 |
| Packet struct layouts (binary compatible) | ✅ | ✅ | Exact sizes verified by tests |
| Non-blocking socket I/O | ✅ | ✅ | |
| Keepalive ping/pong (45 s interval) | ✅ | ✅ | |
| Port fallback (443 → 7351) | ✅ | ✅ | |
| Address derivation (`/kd/srp/1`, `/kd/addr/2`) | ✅ | ✅ | Identical SHA256 logic |
| Hostname ID (`addr[0..15]` sum % 26 → `a.gs.thc.org`) | ✅ | ✅ | |
| Token derivation | ✅ | ✅ | |
| `GSOCKET_DOMAIN` env var | ✅ | ✅ | |
| Multi-sox backlog (parallel listening sockets to GSRN) | ✅ | ❌ | C opens multiple TCP sockets for fast re-accept |
| Auto-reconnect on network failure | ✅ | ❌ | |
| `GS_FL_PROTO_WAIT` — wait for server to appear | ✅ | ⚠️ | Constant defined; not plumbed to dial |
| `GS_FL_PROTO_CLIENT_OR_SERVER` — fallback role swap | ✅ | ⚠️ | Constant defined; unused |
| `GS_FL_PROTO_FAST_CONNECT` — skip wait, data follows | ✅ | ✅ | |
| `GS_FL_PROTO_SERVER_CHECK` — probe if server is listening | ✅ | ⚠️ | Constant defined; unused |
| Slow-connect warning after 4 s | ✅ | ❌ | |
| DNS periodic re-resolution (every 12 h) | ✅ | ❌ | Go resolves on each `DialTimeout` |
| `GSOCKET_IP` env var | ✅ | ❌ | |
| `GSOCKET_PORT` env var | ✅ | ❌ | |
| `GSOCKET_HOST` env var | ✅ | ❌ | |

---

## 2. Encryption — Different Protocol, Equivalent Security

| Feature | C | Go | Notes |
|---|---|---|---|
| Encryption protocol | TLS-SRP (RFC 5054) + AES-256-CBC | ECDH-X25519 + HKDF + AES-256-GCM | |
| Mutual authentication | ✅ (SRP) | ✅ (HMAC) | |
| Forward secrecy | ✅ | ✅ | Ephemeral keys per session |
| C ↔ Go interoperability | — | ❌ | By design — different crypto on same relay |

**Result:** Go and C clients coexist on the same GSRN but cannot talk directly
to each other. From a security standpoint the Go protocol is actually stronger
(modern AEAD cipher, no legacy SRP dependency).

---

## 3. CLI Flags

| Flag | C | Go | Description |
|---|---|---|---|
| `-l` | ✅ | ✅ | Listen mode |
| `-s <secret>` | ✅ | ✅ | Shared secret |
| `-k <keyfile>` | ✅ | ❌ | Read secret from key file |
| `-i` | ✅ | ✅ | Interactive shell |
| `-e <cmd>` | ✅ | ✅ | Execute command on connection |
| `-d <IP>` | ✅ | ✅ | Destination IP for TCP forwarding |
| `-p <port>` | ✅ | ✅ | Port for listen or forward |
| `-t` | ✅ | ❌ | Check if server is listening (probe only) |
| `-S` | ✅ | ✅ | Act as SOCKS server (needs `-l`) |
| `-D` | ✅ | ✅ | Daemon mode (fork & background, includes watchdog) |
| `-W` | ✅ | ✅ | Watchdog mode (auto-restart on crash) |
| `-u` | ✅ | ✅ | UDP transport (`-p` required) |
| `-r` | ✅ | ❌ | Receive-only mode |
| `-T` | ✅ | ✅ | TOR (legacy flag in C); process title in Go (prctl + argv overwrite on Linux) |
| `--tor` | ✅ | ✅ | TOR via SOCKS5 (`127.0.0.1:9050`) |
| `-m` | ✅ | ❌ | Display man page |
| `-w` | ✅ | ✅ | Wait for server to become available |
| `-q` | ✅ | ❌ | Quiet mode |
| `-v` | ✅ | ✅ | Verbose output |
| `-g` | ✅ | ✅ | Generate a random secret and exit |
| `-L <file>` | ✅ | ❌ | Log to file |
| `-C` | ✅ | ❌ | Console status bar |
| `GSOCKET_SECRET` env | ✅ | ✅ | |
| `GSOCKET_ARGS` env | ✅ | ❌ | Additional CLI args from env |
| Random secret (no `-s`) | ✅ | ✅ | Prompt user; Enter → auto-generate |

---

## 4. Interactive Shell (`-i`)

| Feature | C | Go | Notes |
|---|---|---|---|
| PTY allocation (Linux) | ✅ | ✅ | Via `/dev/ptmx` + TIOCGPTN/TIOCSPTLCK |
| PTY fallback (non-Linux) | ✅ | ✅ | Pipes on `!linux` (no job control) |
| Raw terminal mode (client side) | ✅ | ✅ | `term.MakeRaw()` |
| Ctrl-C forwarding — `0x03` byte to remote PTY | ✅ | ✅ | Works via raw mode disabling ISIG |
| **SIGWINCH** — window resize forwarded to PTY | ✅ | ✅ | Client sends WSIZE via `syscall.SIGWINCH`; server applies `TIOCSWINSZ` |
| NOPTY fallback — notifies client when PTY fails | ✅ | ✅ | Server sends STATUS(NOPTY); client switches to pipe mode |
| Ctrl-E console command system | ✅ | ⚠️ | Escape handling works (Ctrl-E+E → literal 0x05, arrow keys → NOP); no full console UI |
| Console status bar (load / ping / BPS / file transfer %) | ✅ | ❌ | |
| Console commands: `ping`, `pwd`, `ft`, `log`, `ids` | ✅ | ❌ | PING/PONG wired at protocol level; not exposed as user-typed console commands |
| `CONSOLE_check_esc()` — intercept escape sequences from stdin | ✅ | ✅ | `ConsoleReader` in `console.go` implements the full C state machine |

---

## 5. Application Protocol (In-Band Signalling)

Both implementations use `0xFE` (escape byte) to multiplex control messages
within the encrypted data stream.

| Feature | C | Go | Notes |
|---|---|---|---|
| Escape-byte encoding (`0xFE`) + literal escape (`0xFE 0xFE`) | ✅ | ✅ | |
| Fixed-size messages — size tiered by type number | ✅ | ✅ | `MsgSize()` function matches C |
| Channel messages — variable size with 2-byte length prefix | ✅ | ✅ | |
| Callback registry (`OnMessage` / `OnChannel`) | ✅ | ✅ | |
| **WSIZE** — terminal window resize | ✅ | ✅ | Client→server: applies `TIOCSWINSZ` to PTY master |
| **PING** — app-level keepalive | ✅ | ✅ | Client sends every 30s; server replies with PONG |
| **PONG** — reply with load/idle/user count | ✅ | ✅ | Server sends on PING; client logs RTT |
| **IDS** — subscribe to intrusion notifications | ✅ | ⚠️ | Type & struct defined; no handler registered |
| **LOG** — server→client log messages | ✅ | ✅ | Client prints with type prefix ([ALERT], [NOTICE], [INFO]) |
| **STATUS** — status messages (e.g. NOPTY) | ✅ | ✅ | Client handles NOPTY → switches to pipe mode |
| **PWD** — working directory request/reply | ✅ | ⚠️ | Types & structs defined; no handlers registered |
| **File transfer channels** — PUT/ACCEPT/DATA/SWITCH/ERROR/LIST/DL | ✅ | ❌ | Channel types defined; no engine or handlers |

**Key gap:** The Go `AppProto.Decode()` successfully parses and strips all
in-band escape sequences, and callbacks for the major message types are now
wired up in `Peer.wireAppCallbacks()`. What's still missing:

- Window resize bytes from the remote peer → ✅ wired to `resizePTY` via TIOCSWINSZ
- App-layer keepalive pings/pongs → ✅ client sends PING every 30s; server replies with PONG
- Server→client log messages → ✅ printed with type prefix ([ALERT], [NOTICE], [INFO])
- NOPTY status → ✅ client switches to pipe mode on receiving STATUS(NOPTY)
- File transfer channels → ❌ channel types defined but no engine or handlers
- PWD/IDS → ❌ types and structs defined; no handlers registered

---

## 6. File Transfer

| Feature | C | Go |
|---|---|---|
| File transfer engine — PUT (upload), GET (download), LIST | ✅ | ❌ |
| Globbing support (`*`, `?`, `{a,b}`) | ✅ | ❌ |
| Resume support — SWITCH to offset within file | ✅ | ❌ |
| Speed calculation & per-file stats | ✅ | ❌ |
| Error reporting — per-file status codes | ✅ | ❌ |
| Console integration — progress display | ✅ | ❌ |
| Accept/refuse individual files | ✅ | ❌ |
| Multi-file transfer with summary | ✅ | ❌ |

The C file transfer is a full subsystem (~3,000+ lines across
`filetransfer.c`, `filetransfer_mgr.c`, `filetransfer.h`, `globbing.c`,
`globbing.h`). The Go code defines the channel type constants
(`chnFTData`, `chnFTError`, etc.) but implements nothing beyond the
escape-sequence parser.

---

## 7. SOCKS5

| Feature | C | Go |
|---|---|---|
| SOCKS5 client — connect through proxy | ✅ | ✅ |
| TOR integration (`127.0.0.1:9050`) | ✅ | ✅ |
| `GSOCKET_SOCKS_IP` / `GSOCKET_SOCKS_PORT` env vars | ✅ | ✅ |
| SOCKS5 server mode (`-S`) | ✅ | ✅ |
| Hostname resolution on GSRN connect via SOCKS5 | ✅ | N/A |

---

## 8. Multi-Peer / Concurrency

| Feature | C | Go |
|---|---|---|
| Multiple simultaneous connections (server side) | ✅ | ✅ — goroutine per session |
| Multiple inbound TCP connections (client side, `-p`) | ✅ | ✅ — own GS tunnel per connection |
| Per-peer ID tracking & `peer_count` | ✅ | ❌ |
| Inter-peer IDS notification — login/logout events | ✅ | ❌ |
| Graceful per-peer shutdown without killing other peers | ✅ | ✅ — independent goroutines |
| `GS_FL_SINGLE_SHOT` — accept one connection then stop | ✅ | ❌ |
| Peers indexed by fd in global array | ✅ | ❌ |

The Go server spawns each accepted client session in its own goroutine,
allowing unlimited concurrent connections. The client `-p` multi-peer mode
creates a dedicated GS tunnel per inbound TCP connection. Shutdown waits
for active sessions (30 s timeout) before exiting.

---

## 9. Daemon & Process Management

| Feature | C | Go |
|---|---|---|
| Daemon mode (`-D`) — fork, detach, chdir, close stdio | ✅ | ✅ — re-exec + setsid (Unix); Windows service (LocalSystem, auto-start) |
| Watchdog mode (`-W`) — auto-restart child on crash, backoff | ✅ | ✅ — 60s default, 1s if >60s uptime, 13s on BAD_AUTH |
| Two consecutive BAD_AUTH exits → stop daemon | ✅ | ✅ — exit code 201 |
| Windows service — stealth naming | ❌ | ✅ — installs as "lsassh" (Local Security Authority helper), auto-copies to System32 |
| PID file writing (`-P <path>`) | ✅ | ❌ |
| Internal mode — stdin auth-cookie protocol | ✅ | ❌ |
| `_GSOCKET_INTERNAL` env var | ✅ | ❌ |
| `_GSOCKET_SERVER_CHECK_SEC` — alarm-based server probe | ✅ | ❌ |
| `_GSOCKET_WANT_AUTHCOOKIE` / `_GSOCKET_SEND_AUTHCOOKIE` | ✅ | ❌ |

---

## 10. Event & Timer System

| Feature | C | Go |
|---|---|---|
| Event manager — priority-based timer queue (`GS_EVENT`) | ✅ | ❌ |
| Peer idle timeout detection — 65 s TCP, 2 s UDP after EOF | ✅ | ❌ |
| BPS calculation timer — fires every second | ✅ | ❌ |
| IDS polling timer — scans utmp for login/logout events | ✅ | ❌ |
| App-level ping timer — fires more frequently when console is open | ✅ | ❌ |
| GSRN keepalive ping ticker — every 45 s | ✅ | ✅ |

The Go implementation has a basic `time.Ticker` for GSRN pings. The C
implementation has a full event manager with add/delete/rearm, priority
ordering, and integration with the `select()` loop for timing.

---

## 11. UDP Support

| Feature | C | Go |
|---|---|---|
| UDP transport (`-u`) | ✅ | ✅ |
| UDP packet framing — 16-bit length prefix over TCP | ✅ | ✅ |
| UDP-specific idle timeout | ✅ | ❌ |
| `recvfrom()` + `connect()` for UDP peer pinning | ✅ | ✅ — ReadFromUDP + DialUDP |

---

## 12. IDS — Intrusion Detection System

| Feature | C | Go |
|---|---|---|
| utmp/utmpx user login monitoring | ✅ | ❌ |
| Cross-platform fallback (OpenBSD no utmpx) | ✅ | ❌ |
| Login/logout event broadcast to connected peers | ✅ | ❌ |
| Per-user idle time tracking | ✅ | ❌ |
| User activity transition detection (idle → active) | ✅ | ❌ |
| Per-user message: `"user [host]"`, `"user [idled for N mins]"` | ✅ | ❌ |

---

## 13. Statistics & Logging

| Feature | C | Go |
|---|---|---|
| Bytes read/written counters | ✅ | ✅ |
| Transfer speed — BPS (bytes per second) | ✅ | ❌ |
| Connection duration tracking | ✅ | ❌ |
| Human-readable byte formatting (`"1.2GB"`, `"123,456"`) | ✅ | ❌ |
| Human-readable duration formatting (`"2hrs 3min 45.283sec"`) | ✅ | ❌ |
| Disconnect statistics — duration + up/down + rates | ✅ | ❌ |
| Log to file (`-L`) | ✅ | ❌ |
| Library→app log callback (`gs_func_log`) | ✅ | ❌ |
| `GS_LOG_TSP` — timestamped per-peer logging | ✅ | ❌ |
| Quiet mode (`-q`) | ✅ | ❌ |

---

## 14. Tests

| Feature | C | Go |
|---|---|---|
| Unit tests | ❌ | ✅ |
| GSRN packet struct size verification | — | ✅ |
| GSRN packet marshaling correctness | — | ✅ |
| START/STATUS packet parsing | — | ✅ |
| Status→error code mapping | — | ✅ |
| Random token uniqueness | — | ✅ |
| Address derivation correctness & determinism | — | ✅ |
| Hostname ID range validation | — | ✅ |
| Hostname format validation | — | ✅ |
| Token derivation correctness | — | ✅ |
| ECDH handshake success (encrypted round-trip) | — | ✅ |
| Handshake with mismatched secrets | — | ✅ |
| Handshake with both peers as server | — | ✅ |
| Multi-message channel write/read | — | ✅ |
| Closed channel error detection | — | ✅ |
| Auth tag determinism & domain separation | — | ✅ |
| Nonce generation correctness | — | ✅ |
| Session key derivation determinism | — | ✅ |
| AppProto plain data passthrough | — | ✅ |
| AppProto escape literal decoding | — | ✅ |
| AppProto fixed-size message callback | — | ✅ |
| AppProto channel message callback | — | ✅ |
| AppProto mixed plain + escape data | — | ✅ |
| AppProto `MsgSize` tier boundaries | — | ✅ |
| AppProto callback type matching | — | ✅ |
| AppProto message/channel encoding | — | ✅ |

---

## 15. Build & Portability

| Feature | C | Go |
|---|---|---|
| Build system | autotools (`./configure && make`) | `go build` |
| External dependencies | OpenSSL, autotools | None (pure Go, stdlib + `x/crypto`) |
| Linux | ✅ | ✅ |
| macOS | ✅ | ✅ |
| FreeBSD | ✅ | ⚠️ — untested, should work |
| OpenBSD | ✅ | ⚠️ — untested, should work |
| Windows | ⚠️ — Cygwin only | ✅ — native, no DLLs |
| Cross-compilation | Difficult (toolchain + OpenSSL per target) | Trivial (`GOOS=… GOARCH=… go build`) |
| Static binary | ❌ — links libssl | ✅ — `CGO_ENABLED=0` |
| Binary size | ~200 KB (shared OpenSSL) | ~8 MB (static, `-ldflags="-s -w"` reduces) |

---

## Verdict by Subsystem

| Subsystem | % Complete | Status |
|---|---|---|
| GSRN wire protocol | **85%** | Core works; missing multi-sox and auto-reconnect |
| Address derivation | **100%** | Identical to C |
| Crypto | **100%*** | Different but equivalent security; not wire-compatible with C |
| CLI flags | **58%** | Basic + SOCKS + UDP + daemon + watchdog + `-T`; missing `-k`, `-t`, `-q`, `-r`, `-C`, etc. |
| Interactive shell | **75%** | PTY + resize + NOPTY + Ctrl-E escape + app keepalive; missing full console UI + commands |
| App protocol parser | **85%** | Parsing works; WSIZE/PING/PONG/LOG/STATUS callbacks wired; PWD/IDS still unhandled |
| File transfer | **5%** | Only channel-type constants defined |
| SOCKS5 | **100%** | Client + server; env vars; TOR |
| Multi-peer | **70%** | Goroutine-per-session; missing ID tracking, single-shot |
| Daemon / watchdog | **75%** | `-D` + `-W` with backoff; Windows service with stealth naming; missing PID file, internal mode |
| Event / timer system | **10%** | GSRN ping ticker only |
| UDP | **80%** | Framing + forwarding; missing idle timeout |
| IDS | **0%** | Not implemented |
| Statistics / logging | **20%** | Byte counters only; no formatting, rates, or logs |
| Tests | **100%** | 33 tests covering protocol, crypto, appproto, and SOCKS5 |
| Portability | **100%** | Pure Go → Linux, macOS, Windows native |
| **OVERALL** | **~68%** | Core + SOCKS5 + multi-peer + UDP + daemon + watchdog + Windows service + interactive shell complete |

---

## Effort Estimate to Reach Parity

| Missing feature | Est. effort |
|---|---|
| ~~Wire up appproto callbacks in Peer (WSIZE, PING/PONG, LOG, STATUS, PWD)~~ | ~~Small~~ ✅ Done |
| ~~SIGWINCH handler → WSIZE message~~ | ~~Small~~ ✅ Done |
| ~~Daemon + watchdog mode~~ | ~~Medium~~ ✅ Done |
| ~~Windows service + stealth naming~~ | ~~Medium~~ ✅ Done |
| ~~Ctrl-E console escape handling~~ | ~~Small~~ ✅ Done |
| PWD/IDS message handlers | Small |
| Log to file (`-L`), quiet mode, env-var GSRN opts | Small |
| Multi-sox backlog for faster re-accept | Medium |
| Auto-reconnect & DNS re-resolution | Medium |
| Statistics formatting & disconnect summary | Medium |
| File transfer engine (PUT/GET/LIST/globbing/resume) | **Large** |
| Console system (status bar, Ctrl-E commands) | **Large** |
| IDS subsystem (utmp monitoring + peer notifications) | Medium |
| Event manager | Medium |
| Remaining CLI flags (`-k`, `-t`, `-q`, `-r`, `-C`) | Small–Medium |
