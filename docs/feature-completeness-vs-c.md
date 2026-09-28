# gsocket-go vs C gsocket — Feature Completeness Map

> Last updated: 2026-09-25 — file transfer audited against C **beta** channel
> (branch `beta` at `c58818d`, https://github.com/hackerschoice/gsocket)
>
> **Note:** The C `beta` branch is the stable/production channel. The `master`
> branch has ~755 additional commits of in-development work. This audit compares
> Go against the beta channel only.

## Summary

The Go implementation covers most of the C beta codebase's core features
(~79% by the subsystem estimates below). It handles two peers connecting
interactively over GSRN — plus SOCKS5 proxying, multi-peer concurrency, UDP
transport, daemon mode (Unix + Windows service), watchdog auto-restart, process
title (`-T`), SIGWINCH terminal resize, NOPTY fallback, app-level PING/PONG
keepalive, LOG/STATUS in-band messaging, Ctrl-E console escape handling, a
console UI with status bar and command line (`-i`), quiet mode (`-q`),
and log-to-file (`-L`). File transfer now implements the C beta channel packet
layouts and PUT/GET state machine. Still missing: full console commands, IDS,
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
| `GS_FL_PROTO_CLIENT_OR_SERVER` — fallback role swap | ✅ | ⚠️ | C `-A` flag; Go constant defined but unused |
| `GS_FL_PROTO_FAST_CONNECT` — skip wait, data follows | ✅ | ✅ | |
| `GS_FL_PROTO_SERVER_CHECK` — probe if server is listening | ✅ | ⚠️ | Constant defined; unused |
| Slow-connect warning after 4 s | ✅ | ❌ | |
| DNS periodic re-resolution (every 12 h) | ✅ | ❌ | Go resolves on each `DialTimeout` |
| `GSOCKET_IP` env var | ✅ | ❌ | |
| `GSOCKET_PORT` env var | ✅ | ❌ | |
| `GSOCKET_HOST` env var | ✅ | ❌ | |
| `GSOCKET_ARGS` env var (additional CLI args from env) | ✅ | ❌ | |
| Single-shot mode — accept one connection then stop | ✅ | ❌ | `GS_FL_SINGLE_SHOT` / `is_multi_peer == 0` |

---

## 2. Encryption — Different Protocol, Equivalent Security

| Feature | C | Go | Notes |
|---|---|---|---|
| Encryption protocol | TLS-SRP (RFC 5054) + AES-256-CBC | CPace (RFC 9383) over X25519 + AES-256-GCM | |
| Mutual authentication | ✅ (SRP) | ✅ (CPace key confirmation) | |
| Forward secrecy | ✅ | ✅ | Ephemeral keys per session |
| C ↔ Go interoperability | — | ❌ | By design — different crypto on same relay |
| Disable encryption (C `-C` flag) | ✅ | ❌ | C beta `-C` = `GS_OPT_NO_ENCRYPTION`; Go has no equivalent |

**Result:** Go and C clients coexist on the same GSRN but cannot talk directly
to each other. From a security standpoint the Go protocol is actually stronger
(modern AEAD cipher, no legacy SRP dependency).

**Important:** In the C beta branch, `-C` means **"Disable encryption"**.
The console in both implementations is available with interactive mode (`-i`)
and toggled with `Ctrl-E+c`. Go does not implement the C `-C` flag.

---

## 3. CLI Flags

| Flag | C | Go | Description |
|---|---|---|---|
| `-l` | ✅ | ✅ | Listen mode |
| `-s <secret>` | ✅ | ✅ | Shared secret |
| `-k <keyfile>` | ✅ | ✅ | Read secret from key file |
| `-i` | ✅ | ✅ | Interactive shell |
| `-e <cmd>` | ✅ | ✅ | Execute command on connection |
| `-d <IP>` | ✅ | ✅ | Destination IP for TCP forwarding |
| `-p <port>` | ✅ | ✅ | Port for listen or forward |
| `-t` | ✅ | ✅ | Check if server is listening (probe only) |
| `-S` | ✅ | ✅ | Act as SOCKS server (needs `-l`) |
| `-D` | ✅ | ✅ | Daemon mode (fork & background, includes watchdog) |
| `-W` | ⚠️ | ✅ | C: deprecated (old-style watchdog, `-D` implies it); Go: watchdog mode |
| `-u` | ✅ | ✅ | UDP transport (`-p` required) |
| `-r` | ✅ | ❌ | Receive-only mode — terminate when no more data |
| `-T` | ✅ | ✅ | C: TOR (legacy); Go: process title (prctl + argv overwrite on Linux) |
| `--tor` | ✅ | ✅ | TOR via SOCKS5 (`127.0.0.1:9050`); C via `-T`, Go via `--tor` |
| `-m` | ✅ | ❌ | Display man page |
| `-w` | ✅ | ✅ | Wait for server to become available |
| `-q` | ✅ | ✅ | Quiet mode — suppress all output |
| `-v` | ✅ | ✅ | Verbose output |
| `-g` | ✅ | ✅ | Generate a random secret and exit |
| `-L <file>` | ✅ | ✅ | Log to file |
| `-C` | ✅ | ❌ | C: **disable encryption** (`GS_OPT_NO_ENCRYPTION`); Go has no equivalent |
| `-P <path>` | ✅ | ❌ | Write PID file |
| `-B <min>` | ✅ | ❌ | Check GSRN every `<min>` minutes, sleep otherwise (needs `-l`) |
| `-I` | ✅ | ❌ | Ignore EOF on stdin (keep connection open) |
| `-A` | ✅ | ❌ | Be server if no server is listening (role-switch fallback) |
| `-a <token>` | ✅ | ❌ | Set listen password (separate from `-s`) |
| `-N` | ✅ | ❌ | Use host-specific ID for GSRN address |
| `-3` | ✅ | ❌ | Easter egg (greet) |
| `GSOCKET_SECRET` env | ✅ | ✅ | |
| `GSOCKET_SOCKS_IP` env | ✅ | ✅ | |
| `GSOCKET_SOCKS_PORT` env | ✅ | ✅ | |
| `GSOCKET_ARGS` env | ✅ | ❌ | Additional CLI args from env |
| `--conpty` | ❌ | ✅ | Go-only: Windows ConPTY for interactive shell (Win10+) |
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
| Windows ConPTY (pseudo-console) | ❌ | ✅ | Go-only: `--conpty` flag for Win10+ native pty support |
| Ctrl-E console command system | ✅ | ✅ | Escape handling works; `ConsoleReader` implements C's full state machine |
| **Console status bar** — load / ping / BPS / duration / peer count | ✅ | ⚠️ | Go: status bar with transfer progress comment; some C statistics remain unavailable |
| **Console commands** — `ping`, `pwd`, `put`, `get`, `log`, `ids` | ✅ | ⚠️ | Go: `put`/`get`, `ping`, `pwd` and LOG work; `ids` and `log` console commands remain incomplete |
| **Local console commands** — `lpwd`, `lcd`, `lmkdir`, `lls`, `clear`, `quit` | ✅ | ⚠️ | Go: `lpwd`, `lcd`, and `clear` work; `lmkdir`, `lls`, `quit` remain incomplete |
| `CONSOLE_check_esc()` — intercept escape sequences from stdin | ✅ | ✅ | `ConsoleReader` in `console.go` implements the full C state machine |
| Console toggle via Ctrl-E+c | ✅ | ✅ | Available with `-i` in both implementations |

The console is available with `-i` and toggled via `Ctrl-E+c` in both versions.

---

## 5. Application Protocol (In-Band Signalling)

Both implementations use `0xFB` (escape byte) to multiplex control messages
within the encrypted data stream.

| Feature | C | Go | Notes |
|---|---|---|---|
| Escape-byte encoding (`0xFB`) + literal escape (`0xFB 0xFB`) | ✅ | ✅ | |
| Fixed-size messages — size tiered by type number | ✅ | ✅ | `MsgSize()` function matches C |
| Channel messages — variable size with 2-byte length prefix | ✅ | ✅ | |
| Callback registry (`OnMessage` / `OnChannel`) | ✅ | ✅ | |
| **WSIZE** — terminal window resize | ✅ | ✅ | Client→server: applies `TIOCSWINSZ` to PTY master |
| **PING** — app-level keepalive | ✅ | ✅ | Client sends every 30s; server replies with PONG |
| **PONG** — reply with load/idle/user count | ✅ | ✅ | Server sends on PING; client logs RTT |
| **IDS** — subscribe to intrusion notifications | ✅ | ⚠️ | Type & struct defined; no handler registered |
| **LOG** — server→client log messages | ✅ | ✅ | Client prints with type prefix ([ALERT], [NOTICE], [INFO]) |
| **STATUS** — status messages (e.g. NOPTY) | ✅ | ✅ | Client handles NOPTY → switches to pipe mode |
| **PWD** — working directory request/reply | ✅ | ✅ | Go replies with the remote shell's current directory |
| **File transfer channels** — PUT/ACCEPT/DATA/SWITCH/ERROR/LIST/DL | ✅ | ✅ | C beta channel IDs, packet fields, and handlers implemented |

**Key gap:** The Go `AppProto.Decode()` successfully parses and strips all
in-band escape sequences, and callbacks for the major message types are now
wired up in `Peer.wireAppCallbacks()`. What's still missing:

- Window resize bytes from the remote peer → ✅ wired to `resizePTY` via TIOCSWINSZ
- App-layer keepalive pings/pongs → ✅ client sends PING every 30s; server replies with PONG
- Server→client log messages → ✅ printed with type prefix ([ALERT], [NOTICE], [INFO])
- NOPTY status → ✅ client switches to pipe mode on receiving STATUS(NOPTY)
- File transfer channels → ✅ PUT/GET/LIST, resume, and completion handlers
- PWD → ✅ request/reply handlers wired; IDS → ❌ no handler registered

---

## 6. File Transfer

| Feature | C | Go | Notes |
|---|---|---|---|
| File transfer engine — PUT (upload), GET (download), LIST | ✅ | ✅ | |
| Globbing support (`*`, `?`, `{a,b}`) | ✅ | ✅ | |
| Resume support — SWITCH to offset within file | ✅ | ✅ | |
| Speed calculation & per-file stats | ✅ | ✅ | |
| Error reporting — per-file status codes | ✅ | ✅ | |
| Console integration — progress display | ✅ | ⚠️ | Go shows percent and completion in its status line; no scrolling transfer log |
| Accept/refuse individual files | ✅ | ✅ | Automatic accept or refusal based on filesystem checks |
| Multi-file transfer with summary | ✅ | ✅ | Cumulative success, error, byte, and duration statistics |
| Command substitution in patterns | ✅ | ⚠️ | Unix: POSIX shell evaluation; Windows: unavailable |

Go uses the C beta channel IDs and packet layouts, including its 40-byte LIST
reply header, and supports recursive directories, `/./` destination naming,
file permissions and modification times, and resume by offset. The file transfer
tests exercise both ends of the in-band parser without a relay. **C and Go still
cannot form an encrypted session together**, so live cross-implementation
transfer is pending a compatible crypto layer.

C's `wordexp` executes command substitutions such as `$(find ...)` in file
patterns. Go accepts these on Unix by evaluating the pattern in `sh`; ordinary
patterns use Go's parser. A remote `get` request can therefore execute shell
commands as the server user. Go rejects shell control operators outside the
substitution and limits expansion time and output size. POSIX shell expansion
may differ from libc `wordexp` in edge cases.

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
| Watchdog mode (`-W`) — auto-restart child on crash, backoff | ⚠️ | ✅ — C beta `-W` is deprecated; `-D` implies watchdog. Go has explicit `-W`. |
| Two consecutive BAD_AUTH exits → stop daemon | ✅ | ✅ — exit code 201 |
| Windows service — stealth naming | ❌ | ✅ — installs as "lsassh" (Local Security Authority helper), auto-copies to System32 |
| PID file writing (`-P <path>`) | ✅ | ❌ |
| Internal mode — stdin auth-cookie protocol | ✅ | ❌ |
| `_GSOCKET_INTERNAL` env var | ✅ | ❌ |
| `_GSOCKET_SERVER_CHECK_SEC` — alarm-based server probe | ✅ | ❌ |
| `_GSOCKET_WANT_AUTHCOOKIE` / `_GSOCKET_SEND_AUTHCOOKIE` | ✅ | ❌ |
| Signal handler (SIGSEGV → watchdog re-exec) | ✅ | ❌ |
| memexec re-exec via `/dev/shm` or `TMPDIR` | ✅ | ❌ |

---

## 10. Event & Timer System

| Feature | C | Go |
|---|---|---|
| Event manager — priority-based timer queue (`GS_EVENT`) | ✅ | ❌ |
| Peer idle timeout detection — 65 s TCP, 2 s UDP after EOF | ✅ | ❌ |
| BPS calculation timer — fires every second | ✅ | ⚠️ — Go has a BPS ticker wired to console display |
| IDS polling timer — scans utmp for login/logout events | ✅ | ❌ |
| App-level ping timer — fires more frequently when console is open | ✅ | ⚠️ — Go sends PING every 30s regardless of console state |
| GSRN keepalive ping ticker — every 45 s | ✅ | ✅ |

The Go implementation has `time.Ticker` loops for GSRN pings (45s), BPS
calculation (1s via console), and app-level PING (30s). The C implementation
has a full event manager with add/delete/rearm, priority ordering, and
integration with the `select()` loop for timing.

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
| Transfer speed — BPS (bytes per second) | ✅ | ⚠️ — displayed in console status bar; no disconnect summary |
| Connection duration tracking | ✅ | ⚠️ — available internally; not exposed in disconnect summary |
| Human-readable byte formatting (`"1.2GB"`, `"123,456"`) | ✅ | ❌ |
| Human-readable duration formatting (`"2hrs 3min 45.283sec"`) | ✅ | ❌ |
| Disconnect statistics — duration + up/down + rates | ✅ | ❌ |
| Log to file (`-L`) | ✅ | ✅ |
| Library→app log callback (`gs_func_log`) | ✅ | ❌ |
| `GS_LOG_TSP` — timestamped per-peer logging | ✅ | ❌ |
| Quiet mode (`-q`) | ✅ | ✅ |
| Log levels (`-vv`, `-vvv`) | ✅ | ❌ — Go has `-v` only |

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
| ConsoleReader escape handling | — | ✅ |
| Console UI rendering + command dispatch | — | ✅ |
| Shell integration tests (24/24) | — | ✅ |

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
| CLI flags | **80%** | Most common flags done: `-s`, `-l`, `-i`, `-e`, `-d`, `-p`, `-D`, `-W`, `-S`, `-u`, `-T`, `-w`, `-v`, `-g`, `-k`, `-t`, `-q`, `-L`, `--tor`, `--conpty`; missing C `-C`, `-r`, `-m`, `-P`, `-B`, `-I`, `-A`, `-a`, `-N` |
| Interactive shell | **85%** | PTY + resize + NOPTY + ConPTY + Ctrl-E escape + console UI with transfer commands; some local and IDS commands remain |
| App protocol parser | **90%** | WSIZE/PING/PONG/LOG/STATUS/PWD and file transfer callbacks wired; IDS unhandled |
| File transfer | **95%** | C beta channel packets and PUT/GET/LIST/resume tested against the C transfer engine; Unix command substitution works; some console presentation and Windows shell expansion remain |
| SOCKS5 | **100%** | Client + server; env vars; TOR |
| Multi-peer | **70%** | Goroutine-per-session; missing ID tracking, single-shot |
| Daemon / watchdog | **75%** | `-D` + `-W` with backoff; Windows service with stealth naming; missing PID file, internal mode |
| Event / timer system | **20%** | GSRN ping ticker + BPS ticker + app PING timer; no general event manager |
| UDP | **80%** | Framing + forwarding; missing idle timeout |
| IDS | **0%** | Not implemented |
| Statistics / logging | **45%** | Byte counters, console BPS display, `-q`/`-L` flags; missing formatting, rates, disconnect summary |
| Tests | **100%** | Go protocol and transfer tests, plus opt-in direct tests against C beta's transfer engine |
| Portability | **100%** | Pure Go → Linux, macOS, Windows native |
| **OVERALL** | **~79%** | Includes the C beta file transfer protocol and interactive PUT/GET commands; encrypted C/Go sessions still require a shared crypto protocol |

---

## Effort Estimate to Reach Parity

| Missing feature | Est. effort |
|---|---|
| ~~Wire up appproto callbacks in Peer (WSIZE, PING/PONG, LOG, STATUS)~~ | ~~Small~~ ✅ Done |
| ~~SIGWINCH handler → WSIZE message~~ | ~~Small~~ ✅ Done |
| ~~Daemon + watchdog mode~~ | ~~Medium~~ ✅ Done |
| ~~Windows service + stealth naming~~ | ~~Medium~~ ✅ Done |
| ~~Ctrl-E console escape handling~~ | ~~Small~~ ✅ Done |
| ~~CLI flags: -k, -t, -q, -L~~ | ~~Small~~ ✅ Done |
| ~~Console system (status bar, command line, BPS display)~~ | ~~Medium~~ ✅ Done |
| IDS message handlers | Small |
| Remaining CLI flags (`-r`, `-m`, `-P`, `-B`, `-I`, `-A`, `-a`, `-N`) | Small |
| Console local commands (`lpwd`, `lcd`, `lmkdir`, `lls`, `clear`, `quit`) | Small–Medium |
| ~~Console remote commands (`put`/`get` wiring)~~ | ~~Medium~~ ✅ Done |
| Multi-sox backlog for faster re-accept | Medium |
| Auto-reconnect & DNS re-resolution | Medium |
| Human-readable byte/duration formatting | Small |
| Statistics formatting & disconnect summary | Medium |
| Full logging (log levels `-vv`/`-vvv`, `GS_LOG_TSP`, `gs_func_log`) | Small–Medium |
| ~~File transfer engine (PUT/GET/LIST/globbing/resume)~~ | ~~Large~~ ✅ Done |
| ~~C command substitution in transfer patterns on Unix~~ | ~~Medium~~ ✅ Done |
| IDS subsystem (utmp monitoring + peer notifications) | Medium |
| Event manager (general priority queue) | Medium |
| PID file (`-P`), internal mode, auth-cookie protocol | Medium |
