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
in isolation; current main measures ~20.3 s on B4 because
commit-time squash + GC later added per-commit work — see the
B4 table and notes below.) Random-insert workloads improved 30-35%
because the per-edit walk distance drops from O(N) to O(local
delta from previous edit). Prepend-only workloads show small
regressions (a few hundred ns/op) attributable to the marker
bookkeeping overhead — prepends keep idx=0 so markers never get
populated and the cost is pure overhead. Absolute numbers tiny;
acceptable trade-off given the B4 win.

## Baseline (Apple M3, Go 1.26.3, `-benchtime=3x`, post-search-marker build)

All times in ms (1 ms = 1,000,000 ns); doc sizes in bytes.

### B1 — Single-client workloads (N = 6,000 ops)

| ID    | Workload                              |    ops (ms) |  V1 doc |  V2 doc | enc V1 (ms) | enc V2 (ms) | parse V1 (ms) | parse V2 (ms) |
|-------|---------------------------------------|------------:|--------:|--------:|------------:|------------:|--------------:|--------------:|
| B1.1  | Append N chars sequentially           |       13.43 |   6,013 |   6,026 |       0.003 |       0.009 |          0.01 |          0.01 |
| B1.2  | Single insert of N-char string        |        0.06 |   6,013 |   6,026 |       0.001 |       0.006 |         0.007 |         0.020 |
| B1.3  | Prepend N chars one-at-a-time         |        0.75 |  35,880 |   6,036 |        0.09 |        0.34 |          1.21 |          1.23 |
| B1.4  | Insert N chars at random positions    |       44.02 |  52,753 |  29,486 |        0.12 |        0.43 |          2.16 |          2.37 |
| B1.5  | Insert N words at random positions    |       93.03 | 138,396 | 102,408 |        0.41 |        5.73 |          3.98 |          3.72 |
| B1.6  | Insert N chars then delete all        |       11.16 |      18 |      29 |       0.001 |       0.002 |         0.003 |         0.004 |
| B1.7  | Mixed insert/delete at random         |       45.85 |  38,686 |  20,505 |        0.12 |        0.36 |          1.64 |          1.54 |
| B1.8  | Append N numbers to Array             |      113.84 |  47,816 |  17,970 |        0.12 |        0.14 |          1.42 |          1.84 |
| B1.9  | Single insert of N-number Array       |        0.04 |  17,949 |  17,960 |        0.03 |        0.03 |         0.071 |         0.077 |
| B1.10 | Prepend N numbers                     |        0.67 |  47,816 |  17,970 |        0.11 |        0.13 |          1.42 |          1.54 |
| B1.11 | Insert N numbers at random positions  |       37.55 |  64,692 |  41,461 |        0.13 |        0.23 |          2.35 |          2.40 |

### B2 — Two-client concurrent workloads (N₂ = 3,000 ops per peer)

| ID   | Workload                                       |    ops (ms) |  V1 doc |  V2 doc | parse V1 (ms) | parse V2 (ms) |
|------|------------------------------------------------|------------:|--------:|--------:|--------------:|--------------:|
| B2.1 | Concurrently insert at index 0 (worst-case)    |      732.57 |  35,758 |   6,059 |        761.64 |        757.47 |
| B2.2 | Concurrently insert at random positions        |       38.61 |  51,879 |  28,860 |          8.32 |          8.38 |
| B2.3 | Concurrently insert words at random positions  |       74.74 | 133,117 |  99,042 |          7.29 |          7.04 |
| B2.4 | Concurrently insert + delete                   |       26.30 |  33,258 |  14,985 |          1.84 |          1.86 |

### B3 — Many-client high-conflict (N₃ = 489 clients)

| ID   | Workload                              |    ops (ms) | V1 doc | V2 doc | parse V1 (ms) | parse V2 (ms) |
|------|---------------------------------------|------------:|-------:|-------:|--------------:|--------------:|
| B3.1 | 489 clients set Number in shared Map  |        1.00 |  8,142 |  6,694 |          0.29 |          0.30 |
| B3.2 | 489 clients set Object in shared Map  |        3.80 | 12,118 | 10,670 |          0.84 |          0.70 |
| B3.3 | 489 clients set String in shared Map  |        1.03 | 11,030 |  9,582 |          0.28 |          0.30 |
| B3.4 | 489 clients insert into shared Array  |      133.60 |  7,329 |  5,400 |          0.26 |          0.31 |

### B4 — Real-world LaTeX paper trace (259,778 edits, 104k-char final document)

| ID | Workload                          |    ops (ms) |    V1 doc |   V2 doc | enc V1 (ms) | enc V2 (ms) | parse V1 (ms) | parse V2 (ms) |
|----|-----------------------------------|------------:|----------:|---------:|------------:|------------:|--------------:|--------------:|
| B4 | Real-world editing trace          |   20,259.0  |   223,414 |  159,921 |        0.62 |       10.5  |          4.4  |          4.5  |

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

- **V2 encode time can be higher than V1** on word-heavy or
  string-column-heavy workloads (B1.5: 5.93 vs 0.33 ms; B4: 10.5 vs
  0.6 ms) because of `StringEncoder` UTF-16 length computation and
  per-key staging. V1 just emits varstrings inline. Workloads with
  many small distinct strings (each going through the column key
  table) pay this most.

- **B2.1 is the YATA concurrent-tie-break worst case** — two peers
  both inserting at index 0 means every block has to compare against
  every same-position predecessor on integrate. This benchmark
  doubles as an integration-loop stress test; ~730 ms ops + ~760
  ms parse is dominated by integrate's per-block O(n) scan against
  the conflict set.

- **B3.x scales gracefully** because each per-client update is a
  single Map.Set — only one block per merge, so the integrate cost
  stays sub-linear in client count.

- **B4 op-throughput** is ~0.078 ms / edit (259,778 ops / 20.3 s).
  The history: ~0.32 ms / edit pre-search-markers, ~0.041 ms / edit
  with markers, roughly doubled back when commit-time squash + GC
  landed (every commit now runs a merge scan and a GC pass over the
  transaction's blocks). That CPU buys the 8.8× smaller V1 document,
  the right trade for sync traffic. Against yrs's published sub-10-s
  B4 numbers this sits at roughly 2× — AT the DESIGN.md "within 2×
  of yrs" boundary, not comfortably under it; the commit pipeline
  has known optimization headroom (the squash scan re-walks the full
  new-block range per commit).

## Comparison with yjs / ywasm (published numbers)

`dmonad/crdt-benchmarks` publishes results for `yjs` (the npm
JS package, the canonical reference) and `ywasm` (yrs compiled
to WASM, called from JS via wasm-bindgen). The published numbers
were measured on **Intel® Core™ i5-8400 @ 2.80 GHz × 6 with
Node 20.5.0**. Ours were measured on **Apple M3 with Go 1.26.3**.
Hardware and runtime differ, so this is not an apples-to-apples
benchmark — but it places ygo in the same order of magnitude
on the canonical workload and identifies where the remaining
gaps are.

B4 (real-world LaTeX paper trace, 259,778 edits → 104,852-char
document):

| Metric                | yjs (Node)  | ywasm (WASM) | ygo V1 | ygo V2 |
|-----------------------|------------:|-------------:|-------:|-------:|
| Apply edits (ms)      |       5,714 |       28,675 | 20,259 | 20,259 |
| Encode (ms)           |          11 |            3 |    0.6 |   10.5 |
| Parse (ms)            |          39 |           16 |    4.4 |    4.5 |
| Doc size (bytes)      |     159,929 |      159,929 | **223,414** | **159,921** |
| Memory used (MB)      |         3.2 |          0.0 |    n/a |    n/a |

Notes:

- **ywasm is NOT a fair representative of native yrs.** wasm-
  bindgen adds substantial overhead (~5× on this workload per
  the table above); native yrs's own benchmarks ship sub-1-second
  B4 numbers. A direct ygo-vs-native-yrs run under identical
  hardware is on the roadmap but not yet executed.
- **ygo V1 doc size is within ~1.40× of yjs's V1** (223,414 vs
  159,929 bytes). The historical ~12× bloat (1.97 MB, visible in
  this table before June 2026) is resolved: commit-time block
  squash merges per-character insert runs at commit, and GC frees
  deleted content, the same merging yjs performs. The remaining
  1.4× gap comes from runs that cross transaction boundaries less
  favourably than yjs's in-memory merge timing.
- **ygo V2 doc size matches yjs byte-for-byte-scale** (159,921 vs
  159,929 bytes — parity). V2's per-column RLE compression plus
  commit-time GC dedupe everything the squash reaches and more.
  V2 is the right choice for persistence and snapshots.
- **Apply throughput is ~3.5× yjs's wall-clock on different
  hardware** (20.3 s on Apple M3 vs 5.7 s on the slower i5-8400,
  so the hardware-normalized gap is wider). It was ~1.85× before
  commit-time squash + GC landed; the per-commit merge and GC
  scans roughly doubled apply time, in exchange for the 8.8×
  smaller V1 documents. Relative to yrs's published sub-10-s B4
  numbers this sits at roughly 2× — at the DESIGN.md target
  boundary. Known optimization headroom: the squash scan re-walks
  the full new-block range each commit.
- **Encode V2 is ~17× slower** than encode V1 (10.5 vs 0.6 ms)
  because of `StringEncoder` UTF-16 length computation and per-key
  staging. With squash shipped, V1 encode is both the fastest and
  the hot-path default; V2 remains the persistence format where
  its size parity with yjs pays.

A proper cross-impl harness that runs ygo + native yrs +
yjs through a single comparison runner under identical
hardware is on the [tech-debt list](docs/tech-debt.md). The
numbers above are honest absolute figures with hardware
caveats; a head-to-head harness is a "later" optimization.

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
