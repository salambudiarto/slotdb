# eval.md — slotdb 0.0.1 audit (32 MB RAM, 1 shared core)

## CRITICAL — memory

1. **`bufio.NewWriterSize(conn, 256)` allocated per-connection, not pooled.**
   256 bytes is fine but the Writer struct itself + its internal buffer escapes to heap on every Accept.
   Fix: pool `*bufio.Writer` alongside the scanner buffer, or write directly via a pooled `[]byte` + `conn.Write`.

2. **`bufio.NewScanner(conn)` always allocates a new Scanner struct** even though we pool the backing buffer.
   The Scanner itself (with its internal state) is a separate heap alloc. Pool the whole scanner or switch to manual `io.ReadFull` + byte-scan loop.

3. **`rateLimiter` is heap-allocated per connection** via `newRateLimiter` → `&rateLimiter{...}`.
   On a busy server with 64 conns this is 64 structs. Minor but fixable: embed in a pooled connection-state struct.

4. **`appendProcessingTime` uses `[160]byte` on stack — CORRECT. Keep.**

5. **`readJSONBody` uses `io.LimitReader(r.Body, 1<<20)` → 1 MiB limit is too large for this env.**
   A valid JSON body for any endpoint is at most ~50 bytes. Limit to 512 bytes.

6. **`json.NewEncoder(w).Encode(body)` in `writeJSON` allocates a new Encoder every call.**
   For a 32 MB env under load this adds GC pressure. Use a pooled `bytes.Buffer` + single `json.Marshal` + `w.Write`.

7. **`make(map[string]any)` in `parseCommandResult` on every HTTP call.**
   HTTP path is already higher-cost than TCP, but every VIEW creates 1–2 maps. Acceptable for HTTP, but noted.

8. **`sync.Pool` for scanner buffers: correct pattern. Keep.**

9. **`[256]sync.Mutex` = 256 × 8 bytes = 2 KB. Fine.**

10. **Arena default 2 MiB. With 32 MB total RAM, Go runtime baseline is ~3–4 MB, HTTP+TCP stacks ~0.5 MB each goroutine × (2+64) = ~33 MB worst case if all conns active.**
    → Reduce `MAX_CONN` default to **32** and reduce goroutine stack pressure by capping writer buffer.
    → Goroutine stacks start at 2–8 KB each; 66 goroutines (64 conn + autoDFG + TCP accept + HTTP) ≈ 528 KB stack minimum — fine.
    → Actual concern: heap per-connection (Scanner + Writer + rateLimiter + bufio internals) ≈ 4–6 KB × 64 = 384 KB. Tolerable but pool aggressively.

## CRITICAL — CPU / latency

11. **`strings.TrimSpace` + `strings.EqualFold` + `strings.ToUpper` in hot path.**
    `EqualFold` for PING is fine. `ToUpper` in dispatch is called for every non-PING command.
    Fix: uppercase once in `exec` before passing to dispatch.

12. **`exec` calls `splitFirst` then `dispatch` calls `strings.ToUpper(cmd)` — cmd is already split but uppercasing happens again.**
    Minor double-work. Uppercase once in `exec` before passing to dispatch.

13. **`conn.SetReadDeadline(time.Now().Add(...))` called on EVERY line** including after rate-limit check.
    Syscall per line is expensive on a shared core. Use a coarser strategy: set deadline once before scan.
    Fix: set read deadline once before `reader.Scan()` only; remove from inside the loop body.

14. **`fmt.Sprintf` in dispatch** for every STATS/VIEW/INC/DSC/UPSERT response = format + alloc each time.
    Hot path (INC/DSC/VIEW under load) should use `strconv.AppendUint` into a stack buffer.
    Priority: INC and DSC are most likely to be called in tight loops.

15. **`strings.Fields(raw)` in `parseCommandResult`** — allocates a `[]string`. Only called on HTTP path. Acceptable.

## MEDIUM — correctness / stability

16. **`view` returns checksum-ok but does NOT check if slot is active.**
    A free slot (all zeros) passes checksum (XOR of zeros = 0) and returns `OK value=0 flag=0 version=0`.
    This silently reads free/deleted slots without returning "not active".
    Fix: check `flag != FlagDel` inside view or in dispatch after OK.

17. **`PING` double-counts `opsTotal`**: `exec` short-circuits and calls `s.opsTotal.Add(1)`, then `handleConn` calls `s.opsTotal.Add(1)` again.
    Fix: move `opsTotal.Add(1)` to `handleConn` only, remove from inside `exec`.

18. **HTTP `readJSONBody` does `defer r.Body.Close()` but checks `r.Body == nil` AFTER the defer.**
    If body is nil, defer will call `nil.Close()` → panic.
    Fix: check nil before setting defer.

19. **`Run()` error handling: if one transport starts then the other fails, the first goroutine is leaked** (its listener is never closed).
    Fix: track listeners and close on first error.

20. **`dfg` barrier.Lock() held for full arena scan** — at 2 MiB = 262K iterations ≈ 200 µs. Acceptable.
    At larger arenas (8 MB+) this becomes noticeable. Add a warning comment in code.

## IDLE FOOTPRINT

21. **Idle: 1 autoDFG ticker goroutine + 1 TCP accept goroutine + net/http idle pool. Negligible. Good.**
22. **`time.NewTicker` 1-minute interval wakes goroutine once/min. Fine.**
23. **No goroutine leaks if `done` channel is properly closed. Correct.**

## CONFIG TUNING for 32 MB / 1 shared core

24. Recommended defaults:
    - `MAX_CONN=32` (halve goroutine/heap pressure)
    - `AUTO_DFG_INTERVAL=5m` (less wakeup churn at idle)
    - HTTP body limit: 512 bytes (not 1 MiB)
    - `GOMAXPROCS=1` (already correct)

## EXECUTION PLAN (priority order)

A. Fix PING double-count (correctness, trivial).
B. Fix `view` not checking active flag (correctness).
C. Fix `r.Body == nil` + defer panic (correctness).
D. Fix `Run()` listener leak on partial startup failure.
E. Pool `bufio.Writer` to reduce per-connection heap.
F. Reduce HTTP body read limit to 512 bytes.
G. Remove redundant `SetReadDeadline` syscall per line (set once per scan iteration).
H. Uppercase cmd once in exec, pass pre-uppercased to dispatch.
I. Fast-path INC/DSC/VIEW response with stack buffer (avoid fmt.Sprintf hot path).
J. Update default MAX_CONN=32, AUTO_DFG_INTERVAL=5m.