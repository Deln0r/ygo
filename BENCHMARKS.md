# Benchmarks

Performance baseline for ygo, ported from the canonical
[`dmonad/crdt-benchmarks`](https://github.com/dmonad/crdt-benchmarks)
suite. Same benchmark IDs (B1.1, B1.2, ... B4) and same workload
shapes so cross-implementation comparison stays apples-to-apples.

## Running

```
# All benchmarks (skips B4 if trace not present)
go test -bench=. -benchmem ./benchmarks/

# A single benchmark
go test -bench=B1_1 -benchmem ./benchmarks/

# More samples for stability
go test -bench=. -benchtime=5x -benchmem ./benchmarks/

# B4 needs the upstream trace (3.2 MB, gitignored)
node testdata/gen/fetch-b4-trace.mjs
go test -bench=B4 -benchtime=1x -timeout=600s ./benchmarks/
```

Each benchmark ID runs five sub-benchmarks:

- `ops` — time the workload (mutations into a fresh Doc); also
  reports `docBytesV1` / `docBytesV2` (encoded doc size in bytes)
  as custom metrics.
- `encode_v1` / `encode_v2` — time `EncodeStateAsUpdate` /
  `EncodeStateAsUpdateV2` against the pre-built doc.
- `parse_v1` / `parse_v2` — time `ApplyUpdate` / `ApplyUpdateV2`
  on a fresh receiver doc against the encoded bytes.

## Search-marker delta

Before-and-after comparison for the search-marker rollout (post-V2
baseline `cb5b210` → search-marker commit). Same hardware,
`-benchtime=3x` for B1, `-benchtime=1x` for B4. "ops" sub-bench
only (encode/parse paths unaffected by markers).

| ID                              | Before (ms) | After (ms) | Change |
|---------------------------------|------------:|-----------:|-------:|
| B1.3  Prepend chars             |        0.75 |       1.19 |   +59% |
| B1.4  Random insert chars       |       67.59 |      44.02 |   -35% |
| B1.5  Random insert words       |      135.09 |      93.03 |   -31% |
| B1.10 Prepend numbers           |        0.67 |       0.76 |   +13% |
| B1.11 Random insert numbers     |       58.17 |      37.55 |   -35% |
| B2.1  Concurrent insert at 0    |      732.57 |     726.18 |    -1% |
| **B4  Real-world LaTeX trace**  |   **83,593**|  **10,540**| **-87%** |

The B4 result is the headline — 8× speedup on the canonical
real-world workload. (Historical snapshot of the markers change
in isolation; ygo 1.38.0 measures 0.85 s on B4, see the B4 table
and notes below.) Random-insert workloads improved 30-35%
because the per-edit walk distance drops from O(N) to O(local
delta from previous edit). Prepend-only workloads show small
regressions (a few hundred ns/op) attributable to the marker
bookkeeping overhead — prepends keep idx=0 so markers never get
populated and the cost is pure overhead. Absolute numbers tiny;
acceptable trade-off given the B4 win.

## Baseline (Apple M3, Go 1.26.3, `-benchtime=3x`, ygo 1.38.0)

All times in ms (1 ms = 1,000,000 ns); doc sizes in bytes. B4 uses
client ID 1, which keeps V1 small: see the same-machine comparison
below for a random client ID.

### B1 — Single-client workloads (N = 6,000 ops)

| ID    | Workload                              |    ops (ms) |  V1 doc |  V2 doc | enc V1 (ms) | enc V2 (ms) | parse V1 (ms) | parse V2 (ms) |
|-------|---------------------------------------|------------:|--------:|--------:|------------:|------------:|--------------:|--------------:|
| B1.1  | Append N chars sequentially           |        8.80 |   6,013 |   6,026 |       0.005 |       0.008 |         0.011 |         0.013 |
| B1.2  | Single insert of N-char string        |        0.01 |   6,013 |   6,026 |       0.001 |       0.006 |         0.008 |         0.010 |
| B1.3  | Prepend N chars one-at-a-time         |        0.59 |  35,880 |   6,036 |        0.11 |        0.19 |          1.06 |          0.97 |
| B1.4  | Insert N chars at random positions    |        4.46 |  52,753 |  29,486 |        0.13 |        0.33 |          2.54 |          2.69 |
| B1.5  | Insert N words at random positions    |       14.39 | 138,387 | 102,403 |        0.39 |        0.77 |          4.38 |          4.38 |
| B1.6  | Insert N chars then delete all        |        3.42 |       0 |       0 |       0.001 |       0.002 |         0.004 |         0.005 |
| B1.7  | Mixed insert/delete at random         |        3.98 |  35,106 |  18,315 |        0.12 |        0.19 |          1.97 |          2.09 |
| B1.8  | Append N numbers to Array             |        3.09 |  17,949 |  17,960 |       0.034 |       0.033 |         0.089 |         0.079 |
| B1.9  | Single insert of N-number Array       |        0.04 |  17,949 |  17,960 |       0.033 |       0.033 |         0.066 |         0.074 |
| B1.10 | Prepend N numbers                     |        0.84 |  47,816 |  17,970 |        0.14 |        0.14 |          1.08 |          1.05 |
| B1.11 | Insert N numbers at random positions  |        4.66 |  64,684 |  41,462 |        0.15 |        0.27 |          2.86 |          2.73 |

### B2 — Two-client concurrent workloads (N₂ = 3,000 ops per peer)

| ID   | Workload                                       |    ops (ms) |  V1 doc |  V2 doc | parse V1 (ms) | parse V2 (ms) |
|------|------------------------------------------------|------------:|--------:|--------:|--------------:|--------------:|
| B2.1 | Concurrently insert at index 0 (worst-case)    |      721.74 |  35,758 |   6,059 |          1.14 |          1.13 |
| B2.2 | Concurrently insert at random positions        |       13.88 |  51,856 |  28,864 |          2.82 |          2.81 |
| B2.3 | Concurrently insert words at random positions  |       19.33 | 133,109 |  99,040 |          4.16 |          4.30 |
| B2.4 | Concurrently insert + delete                   |        6.29 |  33,258 |  14,985 |          1.82 |          6.82 |

### B3 — Many-client high-conflict (N₃ = 489 clients)

| ID   | Workload                              |    ops (ms) | V1 doc | V2 doc | parse V1 (ms) | parse V2 (ms) |
|------|---------------------------------------|------------:|-------:|-------:|--------------:|--------------:|
| B3.1 | 489 clients set Number in shared Map  |        6.81 |  8,142 |  6,694 |          0.31 |          0.33 |
| B3.2 | 489 clients set Object in shared Map  |        6.93 | 12,118 | 10,670 |          0.50 |          0.43 |
| B3.3 | 489 clients set String in shared Map  |        7.59 | 11,030 |  9,582 |          0.37 |          0.40 |
| B3.4 | 489 clients insert into shared Array  |      173.87 |  7,329 |  5,400 |          0.29 |          0.26 |

### B4 — Real-world LaTeX paper trace (259,778 edits, 104k-char final document)

| ID | Workload                          |    ops (ms) |    V1 doc |   V2 doc | enc V1 (ms) | enc V2 (ms) | parse V1 (ms) | parse V2 (ms) |
|----|-----------------------------------|------------:|----------:|---------:|------------:|------------:|--------------:|--------------:|
| B4 | Real-world editing trace          |       852.3 |   223,414 |  159,921 |        0.59 |        0.97 |          4.43 |          4.45 |

## Notes & observations

- **V1 doc size is now competitive after commit-time squash + GC.**
  The former large V1 overhead on fine-grained text is gone: B1.1
  (append-only) drops 35,880 → 6,013 bytes, level with V2, because
  per-character inserts merge into single items at commit. On the B4
  real-world trace V1 falls from 1.97 MB to 223 KB (about 8.8× smaller),
  putting V1 within ~1.4× of V2 instead of ~8.7× larger. Prepend-only
  (B1.3) still favours V2 because reverse-order inserts do not form
  mergeable runs.

- **Garbage collection shrinks delete-heavy documents.** B1.6 (insert
  then delete everything) is now 18 bytes in V1: deleted content is
  freed at commit and replaced with a compact deleted marker, the same
  GC yjs performs. The B4 trace, which is mostly edits (insert + delete),
  benefits in both formats.

- **V2 still wins on RLE-friendly mixed workloads** (random inserts,
  B1.4 / B1.5) where column encoding dedupes structure squash cannot
  reach, and its slight per-insert column overhead (B1.2, B1.9) is
  unchanged. Choose V2 for persistence and large-document sync; V1 is
  now a reasonable default for live sync traffic.

- **V2 encode takes about twice V1's time** (B1.5: 0.77 vs 0.39 ms;
  B4: 0.97 vs 0.59 ms). Until 1.38.0 it was 10-20 times V1's on
  string-heavy documents, because the encoder built its string column
  by appending each piece to the whole column so far.

- **B2.1 is the YATA concurrent-tie-break worst case** — two peers
  both inserting at index 0 means every block has to compare against
  every same-position predecessor on integrate. This benchmark
  doubles as an integration-loop stress test; ~730 ms ops + ~760
  ms parse is dominated by integrate's per-block O(n) scan against
  the conflict set.

- **B3.x scales gracefully** because each per-client update is a
  single Map.Set — only one block per merge, so the integrate cost
  stays sub-linear in client count.

- **B4 op-throughput** is ~0.0033 ms / edit (259,778 ops / 0.85 s).
  The history: ~0.32 ms / edit pre-search-markers, ~0.041 ms / edit
  with markers, about 0.08 ms / edit from commit-time squash + GC
  (which emptied the marker cache on every insert and commit) until
  1.38.0. A transaction per edit with an update handler takes 2.1 s
  against 0.87 s for one transaction (same-machine comparison below):
  the per-transaction work (commit, the update) is now most of it.

## Same-machine comparison with yjs and reearth/ygo

B4 on one Apple M3, measured on 11 Nov 2026: ygo 1.38.0 and
reearth/ygo v1.55.0 on Go 1.26.3, yjs 13.6.33 on Node 26.6. Same
trace, client ID 1, all edits in one transaction, as
`BenchmarkB4_RealWorldTrace` does. Fastest of three interleaved runs.

| Metric        |           ygo |   reearth/ygo |           yjs |
|---------------|--------------:|--------------:|--------------:|
| Apply edits   |        0.88 s |        4.79 s |        1.22 s |
| Encode V1     |       0.55 ms |        7.4 ms |        1.7 ms |
| Encode V2     |       0.94 ms |        7.5 ms |        1.6 ms |
| Doc size V1   | 223,414 bytes | 913,202 bytes | 223,414 bytes |
| Doc size V2   | 159,921 bytes | 161,722 bytes | 159,921 bytes |

Closer to real use: a random client ID, a transaction per edit, an
update handler attached. Ranges over the runs; reearth/ygo ran once.
ygo's sizes are from 1.39.0, which draws 32-bit client IDs as yjs
does; 1.38.0's 53-bit IDs gave 354,850-376,756 bytes of V1 and
7,572,366-8,196,294 bytes of updates.

| Metric            |                 ygo |   reearth/ygo |       yjs |
|-------------------|--------------------:|--------------:|----------:|
| Apply edits       |           2.1-2.4 s |         289 s | 1.07-1.15 s |
| Doc size V1       |             311,038 |     3,354,087 |   311,038 |
| Doc size V2       |             159,929 |             - |   159,929 |
| Bytes of updates  |           6,324,510 | 1,427,198,095 | 6,324,510 |

- With all edits in one transaction ygo takes 0.7 times yjs's time
  and reearth/ygo 5.4 times ygo's; with a transaction per edit ygo
  takes about twice yjs's time.
- ygo and yjs produce documents and updates of identical size, with
  equal client IDs and with random ones: both draw 32-bit IDs. A
  random ID below 2^28 (1 draw in 16) takes 4 bytes on the wire
  instead of 5, so its document and updates come out smaller.
- reearth/ygo's V1 document holds 83,262 structs, against 10,971 for
  ygo and yjs, and each of its per-edit updates carries the
  document's whole delete set.

`dmonad/crdt-benchmarks` publishes yjs and ywasm results from other
hardware (Intel i5-8400, Node 20.5). Its document sizes are V2.
No run against native yrs exists, so the DESIGN.md goal of staying
within 2× of yrs is unverified. A cross-implementation harness is on
the [tech-debt list](docs/tech-debt.md).

## Reproducing

The benchmarks are deterministic per Go's `math/rand` seed. Each
ID uses a distinct seed (B1.4 → 14, B1.5 → 15, B1.7 → 17, B1.11 →
111, B2.2 → 221/222, etc.) so re-runs produce stable byte counts.
Use `-count=N` to gather multiple samples for statistical
comparison via `benchstat`:

```
go test -bench=. -benchtime=5x -count=5 -benchmem ./benchmarks/ | tee bench.txt
benchstat bench.txt
```

## Server transport (ygo-only)

The WebSocket sync server (`server`) has its own benchmarks in
`server/bench_test.go`. There is no yrs equivalent to compare against
(yrs ships no server), so these are absolute ygo figures.

Run:

```
go test -run='^$' -bench=BenchmarkServer_ -benchtime=1s -count=5 ./server/
```

Baseline (Apple M3, Go 1.26.3, loopback httptest server, coder/websocket):

| Benchmark                    |   ns/op | derived |
|------------------------------|--------:|---------|
| Broadcast fan-out, 2 peers   |  28,400 | ~35,000 update round-trips/s |
| Broadcast fan-out, 10 peers  |  60,000 | ~16,700 round-trips/s |
| Broadcast fan-out, 50 peers  | 240,000 | ~4,200 round-trips/s |
| Connect handshake            | 129,000 | ~7,700 connect+sync+close/s |

*Broadcast fan-out* is one client sending a fixed `SyncUpdate` and reading
its own broadcast echo while N-1 idle peers drain their copies. It
isolates the server's apply + fan-out cost from client encoding (the
payload is pre-built and constant). Cost scales roughly linearly with the
room size, about 4.4 µs of marginal fan-out per additional peer on top of
~24 µs of base round-trip, as expected for a per-peer serialized write.

*Connect handshake* is a full dial + WebSocket upgrade + admission +
initial-sync read + close, with the document kept resident by an anchor
connection so the number reflects connection admission rather than
document load/evict churn.

These are loopback numbers: real deployments add network RTT and TLS, and
a slow or half-open peer is bounded by `Options.WriteTimeout` rather than
stalling the fan-out.

## Load test (multi-client, full stack)

`cmd/yload` is a load generator built on the public `client` package: every
connection is a real sync client (full handshake), the first W connections
of each room write a timestamp string every interval, and every connected
client (including the writer, whose broadcast echoes back) records
write-to-observe propagation latency. Zero-loss is checked by accounting:
observations must equal writes x room size.

```
go build ./cmd/yload/
./yload -url ws://127.0.0.1:8080 -rooms 50 -conns 30 -writers 2 \
        -interval 400ms -duration 25s -connrate 200
```

Measured 2026-07-16 against `yserve` v1.14.0 with
`-max-conns 2000 -max-conns-per-doc 500 -max-docs 1000` and a SQLite (WAL)
store.

Local (Apple M3, loopback):

| Scenario | Clients | Result |
|---|---|---|
| 50 rooms x 30 conns, 100 writers total | 1,500 | all synced, 0 errors, 186,000/186,000 observed (no loss), p50 632 µs, p99 5.9 ms, max 54 ms |
| 1 room x 200 conns, 20 writers @ 150 ms (~26,600 deliveries/s) | 200 | all synced, 532,000/532,000 observed (no loss), p50 4.2 ms, p99 468 ms |
| Server RSS at 1,000 active conns | 1,000 | ~96 MB (~90 KB per connection) |

Wide-area (same binary and flags on a 2-vCPU / 4 GB VPS behind Caddy TLS;
load generated from ~220 ms RTT away):

| Scenario | Clients | Result |
|---|---|---|
| 20 rooms x 30 conns | 600 | all synced, 0 errors, 84,000/84,000 observed (no loss), p50 227 ms (~RTT), p99 319 ms; server RSS 52 MB, load 0.17 |

The WAN latencies are round-trip dominated; the loopback run isolates
server cost. The single-crowded-room tail (p99 ~0.5 s at 200 clients in one
document) is the expected worst-case shape: fan-out cost scales with room
size, which is why the live demo spreads visitors across rooms.
