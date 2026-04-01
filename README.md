# 🗄️ slotdb

> A **zero-dependency**, arena-based in-memory slot store written in Go.  
> Dual transport (TCP text protocol + HTTP REST), striped-mutex concurrency, per-connection token-bucket rate limiting, and automatic dead-slot garbage collection — all in a single `main.go`.

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](#license)
[![Single File](https://img.shields.io/badge/source-single%20file-blueviolet)](#)
[![TCP Port](https://img.shields.io/badge/TCP-%3A9000-orange)](#tcp-protocol)
[![HTTP Port](https://img.shields.io/badge/HTTP-%3A8080-blue)](#http-api)

---

## Table of Contents

- [Why slotdb?](#why-slotdb)
- [Slot Layout](#slot-layout)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [TCP Protocol](#tcp-protocol)
- [HTTP API](#http-api)
- [Dead-slot Garbage Collection (DFG)](#dead-slot-garbage-collection-dfg)
- [Concurrency Model](#concurrency-model)
- [Rate Limiting](#rate-limiting)
- [Performance Notes](#performance-notes)
- [Known Limitations](#known-limitations)
- [Changelog](#changelog)
- [License](#license)

---

## Why slotdb?

slotdb is designed for environments where **simplicity and predictability** matter more than raw feature count:

- **Fixed-size arena** — memory is allocated once at startup, no GC pressure from the store itself.
- **Single binary, zero config files** — everything is driven by environment variables.
- **Dual transport** — reach it from any TCP client (netcat, Redis-compatible tooling) *or* from any HTTP client (curl, browsers, SDKs).
- **Checksum per slot** — every 8-byte slot carries an XOR checksum; corruption is detected on every read.
- **Saturating arithmetic** — `INC`/`DSC` clamp to `[0, 2³²−1]` instead of wrapping silently.
- **Pooled buffers** — `bufio.Writer` and scanner backing buffers are pooled per-connection to minimise heap pressure.
- **Hot-path allocation-free responses** — `INC`/`DSC`/`VIEW` responses use stack-allocated byte buffers; no `fmt.Sprintf` on the critical path.

---

## Slot Layout

Every slot is exactly **8 bytes**, stored in a contiguous byte array (the *arena*):

```
 Byte │  0   1   2   3  │  4   │  5   6  │  7
──────┼─────────────────┼──────┼─────────┼──────────
Field │    value u32    │ flag │ ver u16 │ checksum
──────┼─────────────────┼──────┼─────────┼──────────
 LE   │   little-endian │      │  LE     │ XOR(0..6)
```

| Flag value | Meaning   | Notes                              |
|:----------:|:----------|------------------------------------|
| `0`        | Free/Del  | All-zero → **free**; non-zero → **tombstone** |
| `1`        | FlagStart | Normal active slot                 |
| `2`        | FlagNext  | Reserved for future multi-slot use |
| `3`        | FlagEnd   | Reserved for future multi-slot use |

> **Checksum** = XOR of all 7 preceding bytes. Detected on every `VIEW`, `INC`, `DSC`, and `RST`.

> **Active check** — `VIEW` and related commands now explicitly verify `flag == FlagStart` before returning data. Free slots (flag `0`) and tombstones return `ERR not active`, even when their checksum would otherwise pass.

---

## Quick Start

### Build & run locally

```bash
git clone https://github.com/your-username/slotdb.git
cd slotdb
go run main.go
```

Both transports start immediately:

```
listen=:9000 arena_bytes=2097152 slots=262144 max_conn=32 gomaxprocs=1
http_listen=:8080
```

### Docker (single-stage)

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY main.go .
RUN go build -o slotdb main.go

FROM alpine:3.19
COPY --from=builder /app/slotdb /usr/local/bin/slotdb
EXPOSE 9000 8080
CMD ["slotdb"]
```

```bash
docker build -t slotdb .
docker run -p 9000:9000 -p 8080:8080 \
  -e ARENA_BYTES=10485760 \
  slotdb
```

### Quick smoke test

```bash
# TCP
echo "UPSERT 0 42" | nc localhost 9000
echo "VIEW 0"      | nc localhost 9000

# HTTP
curl -s localhost:8080/ping
curl -s -X PUT localhost:8080/slot/0 -d '{"value":42}'
curl -s localhost:8080/slot/0
```

---

## Configuration

All tunables are set via **environment variables**. The binary needs no config file.

| Variable            | Default        | Description                                                    |
|---------------------|----------------|----------------------------------------------------------------|
| `LISTEN_ADDR`       | `:9000`        | TCP listener address                                           |
| `HTTP_LISTEN_ADDR`  | `:8080`        | HTTP listener address                                          |
| `ENABLE_TCP`        | `true`         | Enable TCP transport (`true/false/1/0/yes/no`)                 |
| `ENABLE_HTTP`       | `true`         | Enable HTTP transport                                          |
| `ARENA_BYTES`       | `2097152`      | Arena size in bytes (rounded down to nearest 8). Default = 2 MiB, giving **262 144 slots** |
| `MAX_CONN`          | `32`           | Maximum concurrent TCP connections *(reduced from 64 — see [Performance Notes](#performance-notes))* |
| `MAX_LINE_BYTES`    | `96`           | Maximum bytes per TCP command line                             |
| `READ_TIMEOUT`      | `10s`          | TCP read deadline per line                                     |
| `WRITE_TIMEOUT`     | `5s`           | TCP/HTTP write deadline                                        |
| `IDLE_TIMEOUT`      | `30s`          | TCP idle connection timeout                                    |
| `CMDS_PER_SEC`      | `128`          | Token-bucket rate limit per TCP connection                     |
| `DFG_COOLDOWN`      | `5m`           | Minimum time between DFG sweeps                               |
| `AUTO_DFG_INTERVAL` | `5m`           | Background DFG check interval *(increased from 1m to reduce idle wakeups)* |
| `AUTO_DFG_RATIO_PCT`| `25`           | Tombstone % threshold that triggers auto-DFG                   |
| `AUTO_DFG_MIN_SLOTS`| `1024`         | Minimum tombstone count before auto-DFG fires                  |
| `GOMAXPROCS`        | `1`            | Go runtime GOMAXPROCS (safe default for shared 1-core hosts)   |

### Recommended settings for constrained environments (32 MB RAM / 1 shared core)

```bash
MAX_CONN=32
AUTO_DFG_INTERVAL=5m
GOMAXPROCS=1
# HTTP body limit is hardcoded to 512 bytes (sufficient for all endpoints)
```

---

## TCP Protocol

Connect with any TCP client. Commands are newline-delimited, ASCII text.

```
nc localhost 9000
```

### Commands

| Command                  | Response                        | Description                               |
|--------------------------|---------------------------------|-------------------------------------------|
| `PING`                   | `OK PONG`                       | Liveness check (no timing overhead)       |
| `STATS`                  | `OK slots=… active=… tombstones=… deleted_ratio=… ops=…` | Server statistics |
| `VIEW <idx>`             | `OK value=… flag=… version=…`   | Read slot at index (must be active)       |
| `UPSERT <idx> <value>`   | `OK`                            | Write uint32 value to slot                |
| `DELETE <idx>`           | `OK`                            | Mark slot as tombstone                    |
| `INC <idx> [delta]`      | `OK value=…`                    | Saturating increment (default delta=1)    |
| `DSC <idx> [delta]`      | `OK value=…`                    | Saturating decrement (default delta=1)    |
| `RST <idx>`              | `OK`                            | Reset slot value to 0 (keeps it active)   |
| `DFG [AUTO\|NOW\|DRY]`   | `OK swept=… freed=…`            | Run dead-slot garbage collection          |

All responses from timed commands include a `backend_processing_us=N` field.

### Error responses

```
ERR bad index
ERR bad value
ERR not active
ERR checksum
ERR rate limit
ERR server busy
ERR dfg busy
ERR dfg cooldown
ERR dfg threshold
ERR unknown command
```

### Session example

```
> PING
OK PONG
> UPSERT 5 100
OK backend_processing_us=1
> INC 5 10
OK value=110 backend_processing_us=1
> DSC 5 200
OK value=0 backend_processing_us=1
> VIEW 5
OK value=0 flag=1 version=3 backend_processing_us=1
> DELETE 5
OK backend_processing_us=1
> VIEW 5
ERR not active backend_processing_us=1
> VIEW 99999
ERR not active backend_processing_us=1
> STATS
OK slots=262144 active=0 tombstones=1 deleted_ratio=100 ops=8
> DFG NOW
OK swept=1 freed=1 backend_processing_us=42
```

---

## HTTP API

Base URL: `http://localhost:8080`

All responses are JSON. All write endpoints accept `Content-Type: application/json`.

> **Body size limit**: HTTP request bodies are capped at **512 bytes**. This is sufficient for all valid JSON payloads on every endpoint (maximum meaningful payload is ~50 bytes).

### Response envelope

```jsonc
// Success
{
  "ok": true,
  "operation": "VIEW",
  "command": "VIEW 0",
  "backend_processing_us": 2,
  "raw": "OK value=42 flag=1 version=1 backend_processing_us=2",
  "data": { "value": 42, "flag": 1, "version": 1 }
}

// Error
{
  "ok": false,
  "error": "not active",
  "details": null
}
```

### Endpoints

| Method   | Path                    | Body                       | Description              |
|----------|-------------------------|----------------------------|--------------------------|
| `GET`    | `/`                     | —                          | Route map / service info |
| `GET`    | `/ping`                 | —                          | Liveness probe           |
| `GET`    | `/stats`                | —                          | Server statistics        |
| `GET`    | `/slot/:index`          | —                          | Read a slot              |
| `PUT`    | `/slot/:index`          | `{"value": uint32}`        | Upsert a slot            |
| `DELETE` | `/slot/:index`          | —                          | Delete (tombstone) a slot|
| `POST`   | `/slot/:index/inc`      | `{"delta": uint32}` (opt.) | Increment a slot         |
| `POST`   | `/slot/:index/dsc`      | `{"delta": uint32}` (opt.) | Decrement a slot         |
| `POST`   | `/slot/:index/reset`    | —                          | Reset slot value to 0    |
| `POST`   | `/dfg`                  | `{"mode": "AUTO\|NOW\|DRY"}` | Trigger DFG            |
| `POST`   | `/command`              | `{"command": string}`      | Raw TCP command over HTTP|

> See [`curl-example.md`](./curl-example.md) for ready-to-run examples of every endpoint.

---

## Dead-slot Garbage Collection (DFG)

Deleted slots leave **tombstones** — the slot's bytes are non-zero but flagged as deleted. Tombstones preserve version history but consume arena space. DFG reclaims them.

### Modes

| Mode   | Behaviour                                                         |
|--------|-------------------------------------------------------------------|
| `AUTO` | Runs only if tombstones ≥ `AUTO_DFG_MIN_SLOTS` **and** tombstone % ≥ `AUTO_DFG_RATIO_PCT` |
| `NOW`  | Runs unconditionally (still respects `DFG_COOLDOWN`)             |
| `DRY`  | Reports what *would* be freed without modifying anything         |

### How it works

1. DFG acquires an **exclusive write lock** on the arena barrier — all reads and writes pause.
2. Every slot is scanned; tombstones are zero-filled with a single `copy()`.
3. The tombstone counter is decremented atomically.
4. The lock is released and operations resume.

> 💡 Use `DFG DRY` first to preview the impact before committing a sweep.

> ⚠️ At very large arena sizes (8 MB+, ~1M slots), a full DFG sweep can hold the barrier lock for several hundred microseconds. A warning comment is present in the source at the relevant code site.

---

## Concurrency Model

```
Normal ops (VIEW/UPSERT/INC/…)
  └─ barrier.RLock()           ← shared; many concurrent readers/writers
       └─ stripes[idx & 0xFF].Lock()  ← 256 fine-grained slot-level locks

DFG sweep
  └─ barrier.Lock()            ← exclusive; all other ops pause
       └─ (no stripe locks needed; no readers can enter)
```

- **256 stripe mutexes** keep slot-level contention minimal without allocating a mutex per slot.
- **`sync/atomic`** for `activeSlots`, `tombstones`, `opsTotal`, `lastDFGUnix`, and `dfgRunning` — read/update without holding any mutex.
- **`sync.Pool`** recycles per-connection scanner buffers **and** `bufio.Writer` instances to avoid heap pressure.
- **`opsTotal` is incremented exactly once per command** in `handleConn`. The `PING` short-circuit path no longer double-counts.

---

## Rate Limiting

Each TCP connection gets its own **token-bucket** rate limiter (never shared, no mutex needed):

- **Capacity**: `burst = max(4, limit × 2)` tokens
- **Refill**: `CmdsPerSec` tokens per second
- **On exhaustion**: connection receives `ERR rate limit` and is **disconnected immediately**

The HTTP transport inherits OS-level connection limits instead.

---

## Performance Notes

- `PING` is short-circuited before any timing or lock acquisition — pure throughput path.
- `appendProcessingTime` uses a stack-allocated `[160]byte` array; no heap allocation per response.
- `INC`, `DSC`, and `VIEW` responses use a stack-allocated `[32]byte` buffer with `strconv.AppendUint`; no `fmt.Sprintf` on the hot path.
- Commands are uppercased **once** in `exec` before dispatch; no redundant `strings.ToUpper` calls inside the dispatch switch.
- `SetReadDeadline` is called **once per scan iteration**, not repeatedly inside the command loop, reducing syscall overhead on a shared core.
- The arena is a single `[]byte` slice — all slot data stays in one contiguous memory region, maximising cache locality.
- `bufio.Writer` instances are pooled alongside scanner buffers; per-connection heap overhead is minimised.
- `GOMAXPROCS=1` is the safe default for shared single-core hosts; raise it if you have dedicated cores.

### Memory budget (32 MB host)

| Component | Estimate |
|-----------|---------|
| Go runtime baseline | ~3–4 MB |
| Arena (default 2 MiB) | 2 MB |
| HTTP + TCP goroutine stacks (66 goroutines × ~8 KB) | ~528 KB |
| Per-connection heap (pooled Scanner + Writer + rateLimiter) × 32 | ~160 KB |
| Stripe mutexes (256 × 8 bytes) | 2 KB |
| **Total (approx.)** | **~6–7 MB** |

With `MAX_CONN=32` the server comfortably fits in 32 MB with headroom for application-level spikes.

---

## Known Limitations

- **Single-file design** — all logic lives in `main.go`. This makes the codebase easy to audit but intentionally limits extensibility.
- **No persistence** — the arena is in-memory only. A process restart resets all slots.
- **No replication** — slotdb is a single-node store; there is no built-in clustering or failover.
- **DFG pauses writes** — while DFG holds the barrier write lock, all slot operations are blocked. At the default arena size (~262 K slots), this pause is typically under 200 µs. At larger arenas (8 MB+) it becomes more noticeable.
- **HTTP body limit** — capped at 512 bytes. Sending a larger body returns an HTTP 413-equivalent error response.
- **No TLS** — both transports are plain text. Run behind a TLS-terminating proxy for production use.

---

## Changelog

### v0.0.2 — stability, correctness, and memory improvements

#### Correctness fixes

- **`VIEW` now checks slot active flag** — previously, a free or tombstone slot whose XOR checksum happened to be valid would silently return `OK value=0`. All commands that read slot data now verify `flag == FlagStart` and return `ERR not active` for free and deleted slots.
- **`PING` no longer double-counts `opsTotal`** — the counter was incremented once inside `exec` and again in `handleConn`. It is now incremented exactly once per command in `handleConn` only.
- **HTTP nil body panic fixed** — `readJSONBody` previously set a `defer r.Body.Close()` before checking whether `r.Body == nil`, causing a panic on requests with no body. The nil check now happens first.
- **`Run()` listener leak on partial startup fixed** — if one transport listener started successfully and the other failed, the first goroutine was leaked with no way to stop it. Both listeners are now tracked and closed explicitly on any startup error.

#### Memory improvements

- **`bufio.Writer` is now pooled** — previously a new `bufio.Writer` was heap-allocated for every accepted TCP connection. Writers are now borrowed from a `sync.Pool` and returned on connection close, eliminating one heap allocation per connection.
- **HTTP body limit reduced: 1 MiB → 512 bytes** — the previous limit was far larger than any valid request payload (max ~50 bytes). The tighter limit reduces risk from oversized or malformed HTTP bodies.

#### CPU / latency improvements

- **Command uppercasing is done once in `exec`** — previously `strings.ToUpper(cmd)` was called inside the dispatch switch for every non-PING command. The string is now uppercased once before dispatch.
- **Hot-path responses use stack-allocated buffers** — `INC`, `DSC`, and `VIEW` responses no longer call `fmt.Sprintf`. Instead they use a `[32]byte` stack array with `strconv.AppendUint`, eliminating per-response heap allocations on the most frequently called commands.
- **`SetReadDeadline` is set once per scan iteration** — the previous implementation called this syscall on every loop iteration including after rate-limit checks. It is now called exactly once, just before the blocking `reader.Scan()`.

#### Configuration defaults updated

- `MAX_CONN` default: `64` → `32` — better fits a 32 MB / 1-core host without sacrificing typical workloads.
- `AUTO_DFG_INTERVAL` default: `1m` → `5m` — reduces background goroutine wakeup frequency at idle.

---

### v0.0.1 — initial release

- Arena-based in-memory slot store with fixed 8-byte slot layout.
- Dual transport: TCP text protocol (`:9000`) and HTTP REST API (`:8080`).
- Striped mutex concurrency model (256 stripes).
- Per-connection token-bucket rate limiting.
- Automatic dead-slot garbage collection (DFG) with AUTO / NOW / DRY modes.
- XOR checksum per slot, detected on every read.
- Saturating arithmetic for `INC` / `DSC`.
- Single `main.go`, zero external dependencies.

---

## License

MIT © your-username