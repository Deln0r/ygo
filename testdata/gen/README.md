# testdata/gen — fixture generator

Node.js scripts that load the official `lib0` and `yjs` npm packages and emit
fixtures consumed by Go tests.

## Setup

```bash
# from this directory
npm install
```

## Run

Each script writes one file in `testdata/` at the repo root. CI's fixtures job
runs them all and fails if any committed fixture would change.

| Script | Writes | What it captures |
|---|---|---|
| `gen-lib0.mjs` | `lib0.json` | lib0 primitive encodings (values and the exact bytes) |
| `gen-rle.mjs` | `rle-fixtures.json` | lib0's stateful RLE encoders, which V2 uses |
| `gen-yjs-update.mjs` | `yjs-updates.json` | V1 updates, JS to Go |
| `gen-yjs-update-v2.mjs` | `yjs-update-v2-fixtures.json` | V2 updates, JS to Go |
| `gen-xml.mjs` | `yjs-xml-fixtures.json` | XML type updates |
| `gen-awareness.mjs` | `awareness-fixtures.json` | awareness updates |
| `gen-sync.mjs` | `sync-fixtures.json` | y-protocols sync envelopes |
| `gen-undo.mjs` | `undo-fixtures.json` | UndoManager: final state after the same edits and undo / redo |
| `gen-snapshot.mjs` | `snapshot-fixtures.json` | V1 snapshots |
| `gen-subdoc.mjs` | `subdoc-fixtures.json` | subdocument references |
| `gen-wire-edge.mjs` | `wire-edge-fixtures.json` | multi-client deletes (descending client order), client IDs above 2^32 |
| `gen-nested-gc.mjs` | `nested-gc-fixtures.json` | GC of deleted nested types |
| `gen-relpos.mjs` | `relpos-fixtures.json` | relative positions |
| `gen-rich-text.mjs` | `rich-text-fixtures.json` | rich-text behaviour (deltas, events) |
| `gen-observers.mjs` | `observer-fixtures.json` | which observers fire, in what order |
| `gen-update-events.mjs` | `update-event-fixtures.json` | per-transaction `update` / `updateV2` events and diffs against mid-block state vectors |

Two more scripts check rather than generate: `validate-go-fixtures.mjs` applies
the Go-encoded fixtures (written by `go run ./cmd/gen-go-fixtures`) in yjs, and
`check-fixture-counts.mjs` fails when the README's fixture counts disagree with
the files. `fetch-b4-trace.mjs` downloads the B4 benchmark trace.

## Why Node, not Go-only

The JS reference is the source of truth for wire format. Generating fixtures
from the JS side catches divergence we would otherwise miss. CI runs these
scripts on every PR and fails if `testdata/` would change.

## Adding a new generator

1. Create `gen-<topic>.mjs`.
2. Output JSON or binary to `../<topic>.json` or `../<topic>.bin`.
3. Document the schema at the top of the file.
4. Add a Go test in the corresponding package that loads and asserts against it.
5. Add the script to CI's fixtures job and to the table above, and, for a
   byte-level fixture set without pinned divergences, to
   `check-fixture-counts.mjs` and the README totals.
