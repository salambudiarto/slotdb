# curl Examples — slotdb HTTP API

> Base URL: `http://localhost:8080`  
> Requires: `curl` + [`jq`](https://jqlang.github.io/jq/)  
> Every block below is **one command** — copy, paste, Enter.

---

## Table of Contents

- [Service Info](#service-info)
- [Liveness Probe](#liveness-probe)
- [Statistics](#statistics)
- [Slot Operations](#slot-operations)
  - [Read a slot](#read-a-slot)
  - [Upsert a slot](#upsert-a-slot)
  - [Delete a slot](#delete-a-slot)
  - [Increment](#increment)
  - [Decrement](#decrement)
  - [Reset to zero](#reset-to-zero)
- [Dead-slot Garbage Collection (DFG)](#dead-slot-garbage-collection-dfg)
- [Raw Command (TCP over HTTP)](#raw-command-tcp-over-http)
- [Error Responses](#error-responses)
- [End-to-End Script](#end-to-end-script)

---

## Service Info

```bash
curl -s http://localhost:8080/ | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "INFO",
  "command": "",
  "backend_processing_us": null,
  "raw": "OK",
  "data": {
    "service": "slotdb-http",
    "routes": {
      "ping":          "GET /ping",
      "stats":         "GET /stats",
      "viewSlot":      "GET /slot/:index",
      "upsertSlot":    "PUT /slot/:index {\"value\": uint32}",
      "incrementSlot": "POST /slot/:index/inc {\"delta\": uint32}",
      "decrementSlot": "POST /slot/:index/dsc {\"delta\": uint32}",
      "resetSlot":     "POST /slot/:index/reset",
      "deleteSlot":    "DELETE /slot/:index",
      "dfg":           "POST /dfg {\"mode\": \"AUTO|NOW|DRY\"}",
      "rawCommand":    "POST /command {\"command\": string}"
    }
  }
}
```

</details>

---

## Liveness Probe

```bash
curl -s http://localhost:8080/ping | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "PING",
  "command": "PING",
  "backend_processing_us": null,
  "raw": "OK PONG",
  "data": { "message": "PONG" }
}
```

</details>

---

## Statistics

```bash
curl -s http://localhost:8080/stats | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "STATS",
  "command": "STATS",
  "backend_processing_us": 2,
  "raw": "OK slots=262144 active=3 tombstones=1 deleted_ratio=25 ops=42",
  "data": {
    "slots": 262144,
    "active": 3,
    "tombstones": 1,
    "deleted_ratio": 25,
    "ops": 42
  }
}
```

</details>

---

## Slot Operations

### Read a slot

```bash
curl -s http://localhost:8080/slot/0 | jq .
```

<details>
<summary>Response (active)</summary>

```json
{
  "ok": true,
  "operation": "VIEW",
  "command": "VIEW 0",
  "backend_processing_us": 1,
  "raw": "OK value=42 flag=1 version=3 backend_processing_us=1",
  "data": { "value": 42, "flag": 1, "version": 3 }
}
```

</details>

<details>
<summary>Response (free / deleted slot)</summary>

```json
{
  "ok": false,
  "error": "not active",
  "details": null
}
```

</details>

---

### Upsert a slot

Write `42` to slot `0`:

```bash
curl -s -X PUT http://localhost:8080/slot/0 \
  -H "Content-Type: application/json" \
  -d '{"value": 42}' | jq .
```

Write maximum uint32 to slot `1`:

```bash
curl -s -X PUT http://localhost:8080/slot/1 \
  -H "Content-Type: application/json" \
  -d '{"value": 4294967295}' | jq .
```

Write `0` to slot `2` (valid — slot becomes active with value 0):

```bash
curl -s -X PUT http://localhost:8080/slot/2 \
  -H "Content-Type: application/json" \
  -d '{"value": 0}' | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "UPSERT",
  "command": "UPSERT 0 42",
  "backend_processing_us": 1,
  "raw": "OK backend_processing_us=1",
  "data": {}
}
```

</details>

---

### Delete a slot

Marks slot as tombstone (value preserved until DFG sweeps it):

```bash
curl -s -X DELETE http://localhost:8080/slot/0 | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "DELETE",
  "command": "DELETE 0",
  "backend_processing_us": 1,
  "raw": "OK backend_processing_us=1",
  "data": {}
}
```

</details>

<details>
<summary>Error — slot already deleted</summary>

```json
{
  "ok": false,
  "error": "not active",
  "details": null
}
```

</details>

---

### Increment

Saturating — clamps to `4294967295`, never wraps.

Increment slot `0` by `1` (default delta):

```bash
curl -s -X POST http://localhost:8080/slot/0/inc | jq .
```

Increment slot `0` by `10`:

```bash
curl -s -X POST http://localhost:8080/slot/0/inc \
  -H "Content-Type: application/json" \
  -d '{"delta": 10}' | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "INC",
  "command": "INC 0 10",
  "backend_processing_us": 1,
  "raw": "OK value=52 backend_processing_us=1",
  "data": { "value": 52 }
}
```

</details>

---

### Decrement

Saturating — clamps to `0`, never underflows.

Decrement slot `0` by `1` (default delta):

```bash
curl -s -X POST http://localhost:8080/slot/0/dsc | jq .
```

Decrement slot `0` by `100`:

```bash
curl -s -X POST http://localhost:8080/slot/0/dsc \
  -H "Content-Type: application/json" \
  -d '{"delta": 100}' | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "DSC",
  "command": "DSC 0 100",
  "backend_processing_us": 1,
  "raw": "OK value=0 backend_processing_us=1",
  "data": { "value": 0 }
}
```

</details>

---

### Reset to zero

Zeroes the value while keeping the slot **active** (flag and version are bumped, slot is not deleted):

```bash
curl -s -X POST http://localhost:8080/slot/0/reset | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "RST",
  "command": "RST 0",
  "backend_processing_us": 1,
  "raw": "OK backend_processing_us=1",
  "data": {}
}
```

</details>

---

## Dead-slot Garbage Collection (DFG)

| Mode   | Behaviour                                                              |
|--------|------------------------------------------------------------------------|
| `DRY`  | Preview only — counts tombstones, modifies nothing                    |
| `AUTO` | Runs only if tombstone % ≥ `AUTO_DFG_RATIO_PCT` and count ≥ `AUTO_DFG_MIN_SLOTS` |
| `NOW`  | Forces a sweep (still respects `DFG_COOLDOWN`)                        |

**Dry run** — preview without modifying anything:

```bash
curl -s -X POST http://localhost:8080/dfg \
  -H "Content-Type: application/json" \
  -d '{"mode": "DRY"}' | jq .
```

**Force immediate sweep:**

```bash
curl -s -X POST http://localhost:8080/dfg \
  -H "Content-Type: application/json" \
  -d '{"mode": "NOW"}' | jq .
```

**Auto mode** — runs only if thresholds are met:

```bash
curl -s -X POST http://localhost:8080/dfg \
  -H "Content-Type: application/json" \
  -d '{"mode": "AUTO"}' | jq .
```

<details>
<summary>Response (DRY)</summary>

```json
{
  "ok": true,
  "operation": "DFG",
  "command": "DFG DRY",
  "backend_processing_us": 38,
  "raw": "OK swept=5 freed=0 backend_processing_us=38",
  "data": { "swept": 5, "freed": 0 }
}
```

</details>

<details>
<summary>Response (NOW)</summary>

```json
{
  "ok": true,
  "operation": "DFG",
  "command": "DFG NOW",
  "backend_processing_us": 44,
  "raw": "OK swept=5 freed=5 backend_processing_us=44",
  "data": { "swept": 5, "freed": 5 }
}
```

</details>

<details>
<summary>Error — cooldown active</summary>

```json
{
  "ok": false,
  "error": "dfg cooldown",
  "details": null
}
```

</details>

<details>
<summary>Error — threshold not met (AUTO)</summary>

```json
{
  "ok": false,
  "error": "dfg threshold",
  "details": null
}
```

</details>

---

## Raw Command (TCP over HTTP)

Send any TCP protocol command directly via HTTP — useful for debugging without a TCP client.

```bash
curl -s -X POST http://localhost:8080/command \
  -H "Content-Type: application/json" \
  -d '{"command": "PING"}' | jq .
```

```bash
curl -s -X POST http://localhost:8080/command \
  -H "Content-Type: application/json" \
  -d '{"command": "STATS"}' | jq .
```

```bash
curl -s -X POST http://localhost:8080/command \
  -H "Content-Type: application/json" \
  -d '{"command": "UPSERT 7 999"}' | jq .
```

```bash
curl -s -X POST http://localhost:8080/command \
  -H "Content-Type: application/json" \
  -d '{"command": "INC 7 1"}' | jq .
```

```bash
curl -s -X POST http://localhost:8080/command \
  -H "Content-Type: application/json" \
  -d '{"command": "DFG DRY"}' | jq .
```

<details>
<summary>Response</summary>

```json
{
  "ok": true,
  "operation": "COMMAND",
  "command": "UPSERT 7 999",
  "backend_processing_us": 2,
  "raw": "OK backend_processing_us=2",
  "data": {}
}
```

</details>

---

## Error Responses

| HTTP Status | `error` field       | Cause                                         |
|:-----------:|---------------------|-----------------------------------------------|
| `400`       | `bad index`         | Index out of range or non-numeric             |
| `400`       | `bad value`         | Value > uint32 max or non-numeric             |
| `400`       | `invalid json body` | Malformed JSON in request body                |
| `404`       | `not active`        | Slot is free or a tombstone                   |
| `404`       | `unknown command`   | Unrecognised TCP command via `/command`        |
| `404`       | `route not found`   | No route matched path + method                |
| `409`       | `dfg busy`          | Another DFG sweep is already running          |
| `409`       | `checksum`          | Slot failed XOR integrity check               |
| `429`       | `rate limit`        | TCP connection exceeded `CMDS_PER_SEC`        |
| `429`       | `server busy`       | Concurrent TCP connections hit `MAX_CONN`     |
| `500`       | `dfg cooldown`      | DFG ran too recently (see `DFG_COOLDOWN`)     |
| `500`       | `dfg threshold`     | Tombstone ratio too low for AUTO mode         |

---

## End-to-End Script

Save as `smoke.sh`, then run `bash smoke.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
BASE="http://localhost:8080"

echo "--- ping"
curl -sf "$BASE/ping" | jq -r '.data.message'

echo "--- upsert slot 10 = 500"
curl -sf -X PUT "$BASE/slot/10" \
  -H "Content-Type: application/json" \
  -d '{"value": 500}' | jq -r '.ok'

echo "--- increment slot 10 by 50"
curl -sf -X POST "$BASE/slot/10/inc" \
  -H "Content-Type: application/json" \
  -d '{"delta": 50}' | jq '.data.value'

echo "--- read slot 10"
curl -sf "$BASE/slot/10" | jq '{value: .data.value, version: .data.version}'

echo "--- reset slot 10 to zero"
curl -sf -X POST "$BASE/slot/10/reset" | jq -r '.ok'

echo "--- delete slot 10"
curl -sf -X DELETE "$BASE/slot/10" | jq -r '.ok'

echo "--- dfg dry run"
curl -sf -X POST "$BASE/dfg" \
  -H "Content-Type: application/json" \
  -d '{"mode": "DRY"}' | jq '.data'

echo "--- stats"
curl -sf "$BASE/stats" | jq '.data | {active, tombstones, ops}'
```