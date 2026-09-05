# Changelog

All notable changes to ygo are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project has
followed [Semantic Versioning](https://semver.org/spec/v2.0.0.html) since
v1.0.0: new functionality lands as a minor release, breaking changes are
deferred to a future major.

Where upgrading to a release can change what an existing deployment does, the
entry opens with an **Upgrade impact** note. Most releases have none.

Two optional adapters live in nested modules and are versioned independently of
ygo itself: the NATS backplane (`server/backplane/nats`) and the Matrix
transport (`integration/matrix`). Their releases are listed at the end of this
file.

## [1.19.0] - 2026-09-05

Merging updates no longer loses the half that could not be integrated.

**Upgrade impact, and it is not only about merging.** `ApplyUpdate` changes
too: a document that queues two views of one client's run starting at the same
clock now keeps the longer one instead of whichever arrived first. That path
runs for every document - `ApplyUpdate` -> `Update.Apply` -> `foldUpdate` ->
`Pending.addBlock` - so a deployment peered with yjs, which slices runs at the
state-vector boundary, could silently drop content on 1.18.x and no longer
does. If you never call `MergeUpdates`, this is still the paragraph that
concerns you.

`MergeUpdates` and `MergeUpdatesV2` now preserve updates whose causal ancestor
is absent from the set being merged, instead of dropping them. This **retires the warning added in 1.18.0**: merging an incomplete set is
no longer a way to lose data, and callers that were told to "merge complete
sets, or check `HasPending` on your own replay first" no longer have to.

The output can now contain `Skip` runs, which is how the wire format expresses
a hole and what yjs has always emitted for this shape. Any decoder that could
read yjs's own merged output can read ours; ygo has decoded Skip runs since
v13.5-format support landed. A caller that applies a merged update to an empty
document and expects it to integrate fully must still supply the missing
history first - the merged update carries the far side, but carrying is not
integrating.

Merging a set with nothing pending is byte-identical to before.

### Fixed

- `MergeUpdates` / `MergeUpdatesV2` applied every update to a scratch document
  and re-encoded it, so anything parked in the pending buffer never reached the
  output. Merging a lone delta whose ancestor was absent returned an EMPTY
  update and the edit was gone, with no error anywhere. The reference
  implementation returns that delta unchanged, which is now also what ygo does -
  byte for byte, in the lone case.

  The two encoders emit the same plan through different writers, because V2
  carries a Skip's length in the rest stream rather than the len column. Both
  are covered by their own tests; the V1 output was additionally handed to
  yjs 13.6.32, which read it, held the far side pending, converged once the gap
  was filled, and re-merged our bytes without loss.

  Note the sibling function `persist.MergeUpdates` is unchanged: it still
  refuses an incomplete log with `ErrIncompleteLog` rather than compacting it.
  Compaction is a destructive replace, so refusing remains the safe answer
  there; making it use the preserving encoder is a separate change with its own
  risks.

- Overlapping views of one client's run are reconciled before they are
  emitted. Neither wire format carries a per-block clock - a receiver derives
  each block's clock by accumulating lengths from the run's declared start - so
  emitting two records that cover overlapping clocks relabels the second one at
  the end of the first, duplicating content on clocks its client never minted
  and shifting every later block. Both ygo and yjs accept the result without
  complaint, which is what made it worth finding: an adversarial review of this
  change reproduced `"cccccXccc"` where every other path gives `"ccXccc"`, from
  two ordinary diffs - one peer publishing a run whole while another, having
  spliced into the middle of it, published the same run split in two.

- Queued deletes are carried too. A delete whose target has not arrived yet
  sits in the same buffer as an unintegrated block, and dropping it silently
  resurrects content the author deleted once the insert finally shows up.

- The pending buffer no longer discards a longer block that starts at a clock
  it has already queued. Skipping by start clock alone was meant to stop a
  re-applied update double-queueing, but a longer block at a known start clock
  is new content, and losing it showed up as a Skip run over clocks the buffer
  had been handed. Not reachable through ygo's own encoders, which emit whole
  client runs; a peer that slices at the state-vector boundary produces it.

**Cost note.** Merging updates that cannot integrate is superlinear in how many
of them there are: the pending buffer is re-drained per update. Measured on
this machine, 1600 such updates (24 KB) merge in ~100 ms and the curve is
roughly quadratic. A merge set with nothing pending takes the ordinary path and
is unaffected, which covers compaction of a healthy log.

### Infrastructure

- The Codeberg mirror job was failing on release days - a release pushes the
  commit and then the tag a second later, and the two mirror runs raced for the
  same remote. The mirror stayed correct only because whichever run won pushed
  everything. Runs are now serialised, and the job verifies the result rather
  than trusting the push's exit code: it fails unless `main` matches on both
  sides and every local tag exists on the mirror. The README's "auto-synced on
  every push" is a checkable statement again.

## [1.18.1] - 2026-09-04

Wire-format fidelity at the top of the clock space, and an honest note about
what update validation does and does not buy.

**Upgrade impact** - none for updates produced by ygo or yjs. The decoder now
refuses a GC run, a Skip run or a delete range naming a clock above
`Number.MAX_SAFE_INTEGER`. No yjs peer can emit one: handed such a value the
reference implementation throws `Integer out of Range` (measured against yjs
13.6.32), so this only closes the gap where ygo accepted what the reference
refuses. Runs BELOW that limit are still accepted, because yjs accepts them and
diverging in that direction would break interoperability.

### Fixed

- GC and Skip block lengths and delete-set ranges are bounded to the clock
  space the wire format can represent. A hand-built update declaring a run of
  2^64-1 was previously accepted, in twelve bytes, and pushed a client's clock
  space to the end of the range.

### Documented

- **Yjs updates do not authenticate deletes, and no amount of input validation
  changes that.** Anyone who can hand a document an update can tombstone
  another client's content and stop that client from being heard again - with a
  dozen legal bytes, or equally by making an ordinary edit that deletes their
  text. The deletion then propagates to every peer. The reference
  implementation behaves identically; this was measured in both directions,
  with the victim's client ID pinned on each side. `TestUpdateFormat_DeletesAreNotAuthenticated`
  keeps the claim honest, and will fail if ygo ever diverges here.

  The practical consequence, now stated where it belongs rather than implied
  away: skipping malformed input protects a reader's *availability*, not the
  document's *integrity*. Anywhere ygo accepts updates from parties you do not
  trust with the document itself - the Matrix transport most of all, where
  room membership is write access - that distinction is the whole story.

## [1.18.0] - 2026-09-03

A data-loss fix in out-of-order update handling, and the strict validator the
Matrix transport needed. The transport itself ships as a separate module; see
"Nested module: integration/matrix" at the end of this file.

**Upgrade impact** - an update whose first clock sits above what the receiving
document knows for that client is now held in the pending buffer until the gap
is filled, instead of being integrated immediately. This is the yjs and yrs
behaviour, and it fixes silent, permanent data loss: integrating over the hole
advanced the store clock past it, so the update that would have filled the hole
arrived later, read as already-known, and was discarded. Reproduced with one
client editing two roots and publishing only the second delta - delivered
newest-first, the first root's text vanished, and the same sequence on
yjs 13.6.32 kept both.

Applies to both wire formats: V1 and V2 share the integration path, and each
has its own regression test.

Code that applies a causally complete stream is unaffected. Code that applies
out-of-order deltas will see `HasPending` report true where it previously
reported false, and `ApplyUpdate` will end up with more data, not less. Two
consequences of that wider pending class are worth naming, because they do not
go through `ApplyUpdate`:

- `MergeUpdates` and `MergeUpdatesV2` build a document and re-encode it, so an
  update that is queued rather than integrated is **dropped from the merged
  result**. Merging a set that does not contain the causal ancestor of every
  update in it now loses more than it did before. Merge complete sets, or check
  `HasPending` on your own replay first. (This is the same class of hazard the
  1.17.0 note describes for stored logs, in the one entry point that does not
  refuse.)
- `persist.MergeUpdates`, and therefore `Server.Flush`, `persist.SaveVersion`
  and `FlushEvery`, refuse with `persist.ErrIncompleteLog` when a stored log
  holds an update with a clock gap. That is the 1.17.0 guard doing its job -
  refusing beats silently compacting the update away - but a log whose filling
  update never arrives will not compact until it does.

### Added

- `ygo.ValidateUpdate` - parses a V1 update without integrating it, for callers
  that accept updates from somewhere they do not control. Stricter than
  `ApplyUpdate` in the way that matters: `ApplyUpdate` decodes one update and
  silently ignores whatever bytes follow, so `append(a, b...)` applies only `a`
  and loses `b` with no error anywhere (yjs 13.6.32 behaves identically, which
  is why `ApplyUpdate` keeps that behaviour and this is a separate check).
  Combine updates with `MergeUpdates`, never with `append`. V1 only: the V2
  decoder does not report a trailing remainder, so there is no equivalent
  strict check for V2 bytes - V2 bytes handed to it are named as such rather
  than misdiagnosed as a concatenation.

### Fixed

- Updates with a clock gap in their own client sequence are queued instead of
  integrated, closing the data-loss path described under Upgrade impact. The
  same guard covers GC ranges, which carried the identical monotonicity
  precondition as blocks.
- `MissingSV` now reports the clock gap itself, so a document stuck behind one
  can name what it is waiting for. It previously returned an empty vector in
  exactly the case that matters - an item that is first in its root type has no
  Origin, RightOrigin or Parent-by-ID to walk, so the gap is its only
  dependency - which left the documented "ask a peer for what is missing"
  round trip asking for nothing.
- The package documentation said "Status: pre-alpha. Public API is unstable.",
  contradicting the stability promise the project has made since v1.0.0 and
  displayed it as the first thing a reader sees on pkg.go.dev. `Version` is now
  set at release time instead of reading `0.0.0-dev`.
- `ApplyUpdate` and `ApplyUpdateV2` document the trailing-byte behaviour at the
  point of use, and the note about mixing wire formats now says what actually
  happens in each direction: `ApplyUpdateV2` on V1 bytes errors loudly, while
  `ApplyUpdate` on V2 bytes is a silent no-op.

## [1.17.0] - 2026-08-29

Compaction integrity, idle keep-warm, automatic log compaction.

**Upgrade impact** — `Server.Flush`, `persist.MergeUpdates` and
`persist.SaveVersion` can now return `persist.ErrIncompleteLog` instead of
succeeding, when the update log momentarily contains an update whose causal
ancestor has not been stored yet (normal for an instant on a busy document,
because updates are applied and broadcast before they are persisted). Before
this release that exact situation was worse than an error: Flush compacted the
log into a snapshot that silently dropped the dangling update, and the
destructive replace erased the original bytes — permanent loss of an
applied-and-broadcast edit. Callers should treat `ErrIncompleteLog` as
"retry shortly"; the log is intact and replays correctly. Code that only
checked `err != nil` keeps working and simply retries later.

### Fixed
- Compacting a causally incomplete update log no longer loses the dangling
  update: `persist.MergeUpdates` detects unintegrated updates after replaying
  the log and refuses with `ErrIncompleteLog`, leaving the original bytes in
  place. Reproduced with genuine yjs transaction updates (the exact shape
  y-websocket clients broadcast) before fixing; the reproduction is the
  regression test. Affected `Server.Flush` (since v1.14.0), `SaveVersion`
  (a labelled version could silently omit content), and the new `FlushEvery`.
- The nightly fuzz workflow retries once on the Go fuzz coordinator's
  boundary-deadline flake (three occurrences across unrelated targets, tens
  of millions of executions, never a failing input) — but refuses to retry
  when the run wrote a new crasher, so a genuine finding still fails on the
  first attempt.

### Added
- `Options.DocIdleTimeout` keeps a document resident and warm for a bounded
  time after its last connection departs, so a quick reconnect (a page
  refresh) reuses the live document instead of reloading from the Store. The
  document is flushed at the moment it parks, durability matching eviction;
  with a Backplane it keeps applying foreign updates while idle, so it stays
  current. `Options.MaxIdleDocs` bounds how many idle documents stay resident,
  evicting the least-recently-idle. Both default to zero (previous behaviour:
  evict immediately). `Stats` gains `IdleDocuments`.
- `Options.FlushEvery` compacts a document's persisted log after every N
  stored updates, bounding log growth without adopter-side scheduling. A
  store failure backs the retry interval off geometrically; a causally
  incomplete log is skipped quietly and retried at the next threshold.
- A cross-language fixture scenario locking V2 decode of GC structs that
  precede later live content — the data-loss class reearth/ygo fixed in their
  v1.49.1, which ygo was verified clean against.

### Changed
- `modernc.org/sqlite` v1.56.0 → v1.57.0.

## [1.16.0] - 2026-08-13

Supported toolchain, current dependencies.

**Upgrade impact** — the minimum Go version is now **1.25**, raised from 1.22.
Go itself supports only its two most recent releases (1.25 and 1.26), so 1.22
through 1.24 no longer receive upstream fixes; building ygo now requires a
supported toolchain. If you are pinned to an older Go, stay on v1.15.0. The
raise is what allows the dependencies below to move: to keep the 1.22 floor,
`modernc.org/sqlite` had been held at v1.29.10 (released May 2024) in the layer
that stores your documents, along with four transitive pins and dependabot
ignore rules.

### Changed
- Minimum Go version raised to 1.25; the CI matrix is now 1.25 and 1.26, the
  releases upstream supports.
- `modernc.org/sqlite` v1.29.10 → v1.56.0 and `coder/websocket` v1.8.12 →
  v1.8.15, with every version pin and dependabot ignore rule removed so both
  track upstream again. `modernc.org/gc/v3`, `hashicorp/golang-lru/v2`,
  `modernc.org/strutil` and `modernc.org/token` leave the dependency graph
  entirely. No API or wire-format change: the full suite, the persistence layer
  at `-race -count=10`, and the cross-language fixtures all pass unchanged.

### Fixed
- Three decode errors in the `Any` codec no longer start with a capitalized
  word, so they read correctly when wrapped by a caller.

## [1.15.0] - 2026-07-27

Cross-cluster presence.

**Upgrade impact** — affects only deployments that configure a `Backplane`
(multi-instance clustering). Backplane payloads are now framed with a one-byte
kind prefix (0 = document update, 1 = presence update) instead of the bare V1
update bytes v1.14.0 published, and the framing is not version-negotiated. A
mixed-version cluster sharing one broker misparses in both directions, so
upgrade every instance together — drain the old instances rather than rolling
one at a time through a shared broker. Presence is also published over the
Backplane unconditionally now, so cursors cross instances and broker traffic
grows with presence heartbeats; `Options.MaxForeignAwarenessClients` bounds how
many of a room's presence slots relayed presence may occupy but does not
disable the relay. The `Backplane` interface is unchanged, so custom
implementations keep compiling — they carry opaque bytes. Single-instance
servers (`Backplane` nil) and all non-server API are unaffected.

### Added
- Presence/awareness travels over the cluster Backplane, so clients on
  different instances see each other's cursors. A clean disconnect propagates
  an explicit tombstone, clearing a departing cursor cluster-wide promptly
  instead of waiting for each instance's timeout sweep. Presence remains
  best-effort and is never persisted or surfaced through `OnChange`.
- `Options.MaxForeignAwarenessClients` partitions a room's presence budget by
  origin: relayed presence occupies at most that many slots and the rest stay
  reserved for local clients, so a presence flood on one instance cannot deny a
  new local client its slot elsewhere. Zero (the default) reserves about 1/8 of
  a bounded `MaxAwarenessClients` for local clients; a negative value disables
  the partition.
- `cmd/yload`, a runnable multi-client load-test harness, with measured results
  published in BENCHMARKS.md.

### Changed
- Backplane payloads carry a one-byte kind prefix distinguishing document
  updates from presence (see Upgrade impact).

### Fixed
- `RestoreVersion` on the SQLite store no longer fails with
  `SQLITE_BUSY_SNAPSHOT` when writes are landing concurrently; like `Flush`, it
  now takes the write lock up front.

## [1.14.0] - 2026-07-16

Horizontal scaling with a cluster backplane.

### Added
- `server/backplane`: a pluggable pub/sub backplane lets several server
  instances behind a load balancer converge on the same document. Each instance
  publishes its own clients' updates and applies foreign ones to its in-memory
  copy, without re-persisting, re-publishing, or firing `OnChange` a second
  time. Ships an in-process `Memory` hub; implement the small `Backplane`
  interface for a cross-machine broker. Multi-instance deployments must share a
  single Store, and presence is still per-instance in this release.
- `Options.MaxConns` (and `yserve -max-conns`) caps total live connections
  server-wide, complementing the per-document `MaxConnsPerDoc`. It defaults to
  unlimited, so existing servers are unaffected until you set it; over-cap
  connections are refused at the upgrade with 1013 TryAgainLater.
- `Server.Flush(ctx, docName)` forces a persistence checkpoint on demand,
  compacting a document's log without evicting it or disturbing live
  connections; `Stats.Docs` adds a per-document connection breakdown alongside
  the aggregate counts.

### Fixed
- `Flush` on the SQLite store no longer fails intermittently with
  `SQLITE_BUSY_SNAPSHOT` when a document is being written concurrently; the
  transaction now takes the write lock up front, so a competing writer waits
  instead of failing the checkpoint.

## [1.13.0] - 2026-07-13

Update-level utilities and runnable examples.

### Added
- `MergeUpdatesV2` coalesces V2 update blobs (the V2 counterpart of
  `MergeUpdates`).
- `EncodeStateVectorFromUpdate` derives an update's state vector straight from
  the bytes, matching yjs semantics (a contiguous run from clock 0, stopping at
  the first Skip or gap), so the result is safe to diff against; `DiffUpdate`
  returns the part of an update a peer with a given state vector is missing.
  Together they let a server compact and diff stored updates without loading
  full documents into memory.
- Runnable examples under `examples/` (collab-server, collab-client,
  offline-first), output-verified pkg.go.dev examples for the core API, and
  server transport benchmarks (broadcast fan-out, connect handshake) in
  BENCHMARKS.md.

## [1.12.0] - 2026-07-06

Durable concurrent writes and a first-load hook.

**Upgrade impact** — if you use the on-disk SQLite store, `persist/sqlite.Open`
now sets `journal_mode(WAL)` and `busy_timeout(5000)` on every pooled
connection. Journal mode is written into the database file header and is
permanent: an existing database is converted on the first open after upgrading
and stays WAL even for later readers that set no pragmas. WAL needs a writable
*directory* (not just the file) for the `-wal`/`-shm` sidecars and a local
filesystem — it does not work over NFS or similar network shares — and any
backup or copy procedure must checkpoint first or copy the sidecars, or it
captures a stale snapshot. Separately, a call that previously failed fast with
`SQLITE_BUSY` under write contention now blocks for up to 5 seconds instead.
`:memory:` stores are unaffected and no application code changes are required.

### Added
- `Options.OnLoadDocument func(docName) ([]byte, error)` runs on a document's
  first load, after any stored history, to seed or load initial content from a
  source outside the Store. The returned V1 update is applied in memory and
  never persisted, so it re-applies on every load and must carry stable item
  IDs; an error aborts the load.

### Changed
- An on-disk SQLite store opens every pooled connection in WAL mode with a 5s
  busy timeout (see Upgrade impact); the `:memory:` store is unchanged.

### Fixed
- The SQLite store no longer silently loses updates under concurrent writes:
  contending writers now wait for the file lock instead of failing immediately
  with `SQLITE_BUSY` and dropping the update (a 320-concurrent-write repro went
  from 16 landing to all 320).

## [1.11.0] - 2026-07-02

Server load stats, read-only connections, and a document-change hook.

### Added
- `Server.Stats()` returns a snapshot of resident documents and total live
  connections, cheap and safe for concurrent use, for health, metrics and
  capacity surfaces.
- `Options.ReadOnly(docName, r)` marks a connection read-only from the upgrade
  request: its document updates are dropped before they reach the document or
  any peer, while it still receives updates and may still publish awareness, so
  a viewer sees edits and shows a cursor but cannot write.
- `Options.OnChange(docName, update)` fires after a document update is applied
  and broadcast, for search indexing, webhooks or external persistence. It
  fires whether or not a Store is configured, and not for awareness frames,
  handshake frames or a read-only connection's dropped update. It runs
  synchronously on the originating read goroutine, fires even if a configured
  `StoreUpdate` failed, and the update slice is only valid for the duration of
  the call.

## [1.10.0] - 2026-06-27

Server resource caps and connection lifecycle hooks.

**Upgrade impact** — `MaxConnsPerDoc` defaults to 4096 where connections per
document were previously unlimited, so a deployment with a room above that
ceiling starts refusing additional sockets (close code 1008) after upgrading.
Established connections are unaffected. Set `Options.MaxConnsPerDoc` to a
negative value (or run `yserve -max-conns-per-doc -1`) to restore unlimited.
The new `MaxDocs` cap needs no action: it defaults to unlimited.

### Added
- `Options.MaxConnsPerDoc` bounds simultaneous connections to one document; the
  newest socket over the cap is refused with a policy-violation close, so an
  established collaborator is never displaced.
- `Options.MaxDocs` bounds the distinct documents held in memory at once
  (unlimited by default, opt in); a connection that would create a document
  past the cap is refused with a try-again-later (1013) close, while an already
  resident room is always admitted.
- `Options.OnConnect` and `OnDisconnect`: request-aware gating on headers,
  cookies, TLS state or remote address beyond the token-only `OnAuthenticate`,
  with a correlatable per-connection id. `OnConnect` can reject a connection,
  and both a rejection and a panic tear down cleanly without leaking the room
  slot.
- `yserve` gains `-max-conns-per-doc` and `-max-docs` flags.

### Fixed
- A connection can no longer be admitted onto a document that is being evicted:
  admission now finds-or-creates the document and inserts the connection under
  a single lock hold, closing the window that split a room onto divergent
  copies.

## [1.9.0] - 2026-06-22

Concurrency and correctness hardening across client, server and binding.

**Upgrade impact** — server-to-client writes are now bounded by a default 10s
deadline where they were previously unbounded, and a peer that exceeds it is
closed with a policy-violation status. A client on a slow link receiving a
large initial sync or a large update can now be disconnected where it
previously blocked (and blocked the room) until it drained. The deadline cannot
be disabled: `Options.WriteTimeout` only raises it, since zero and negative
values both select the 10s default, so deployments serving very large documents
over slow links should set it explicitly.

### Added
- `Options.WriteTimeout` bounds a single write to one peer; on timeout that
  peer is closed and dropped instead of stalling the fan-out.

### Changed
- Server writes to a client (broadcast, awareness fan-out and the initial sync)
  are deadline-bounded rather than unbounded.

### Fixed
- client: `Connect`/`Close` no longer race — lifecycle handles are published
  atomically under a closed-latch, so a `Close` overlapping a `Connect` can no
  longer leak the transaction observer and its goroutines.
- client: the reconnect backoff resets after a session that actually synced, so
  a transient drop redials quickly instead of at the backoff grown by earlier
  dial failures; a stale connection is also cleared between sessions.
- client: `Close` can no longer hang on a wedged local store — the final flush
  runs under a 5s deadline, and a write cancelled by `Close` is no longer
  reported as an error.
- server: a document could be evicted while a new connection was being placed
  on it, splitting a room onto divergent copies that never reconverged;
  eviction now re-checks document identity and emptiness under the lock, and
  only the evicting releaser flushes to the Store.
- gomobile: `Connect`/`Close` guarded against a use-after-close window,
  negative indices rejected across the typed accessors, and Quill insert values
  classified explicitly (string is text, object/array/number is an embed, null
  is an error).

## [1.8.0] - 2026-06-18

Full shared-type set on the mobile binding.

### Added
- gomobile `Array`: typed JSON elements (`PushJSON` / `InsertJSON` / `GetJSON`
  / `ToJSON` / `DeleteAt`), nested Text / Map / Array, and `ObserveChanges`
  delivering a Quill-style delta whose inserts are runs of values.
- gomobile `Map`: typed values via `SetJSON` / `GetJSON`, nested `SetMap` /
  `SetArray` / `SetText` with matching getters, `KeysJSON` and `Clear`
  alongside the existing string helpers.
- gomobile XML: `XmlFragment` / `XmlElement` / `XmlText` with node names,
  attributes, ordered children and rich-text leaves, for ProseMirror / Tiptap
  on iOS and Android.
- Values cross the binding as JSON (lib0 `Any` shapes), so mobile writes stay
  byte-compatible with JS Yjs; nested types read back through the dedicated
  getters rather than `GetJSON`.

## [1.7.0] - 2026-06-18

Rich text authoring on the mobile SDK.

### Added
- `Text.ApplyDelta(quillJSON)` applies a Quill-style delta in one transaction,
  in the exact shape `ObserveChanges` emits, so a native Quill or ProseMirror
  editor binds in both directions symmetrically.
- `Text.Format(index, length, attributesJSON)` for toolbar formatting over a
  range, plus `Text.InsertWithAttributes` and `Text.InsertEmbed` for inserting
  formatted text or embeds such as images.
- Indices and lengths are UTF-16 units, matching JS Yjs.

## [1.6.1] - 2026-06-17

The integrate path no longer hangs on a malformed update.

### Fixed
- A malformed update carrying a Skip block at a multi-billion clock could hang
  the apply path indefinitely: commit-time scans walked the clock range one
  clock at a time, and a Skip reaching the top of the uint64 space wrapped the
  cursor back to clock 0 and spun forever. Reachable from any peer over the
  sync wire, so any server or client accepting untrusted updates should take
  this release.
- Clock-range scans across update apply, delete-set apply, subdoc tracking, and
  undo/redo now walk cell by cell and refuse to step when it would not make
  forward progress. Valid encodings always advance, so well-formed updates are
  unaffected. No API change.

## [1.6.0] - 2026-06-17

Live collaborator presence and remote cursors on the mobile SDK.

### Added
- `PresenceListener` and `Client.ObservePresence` deliver the whole room's
  awareness as JSON keyed by clientID on every change (cursor move, join,
  leave), so a Swift or Kotlin app can render collaborators in real time.
  Register before or after `Connect`; released on `Close`.
- `Client.PresenceStates` returns the current snapshot for the initial paint,
  and `Client.ClientID` lets you filter out your own entry.
- Combined with the existing `EncodeCursor` / `ResolveCursor`, a mobile client
  can publish a cursor in its state and resolve peers' cursors to local indices
  that stay correct through concurrent edits.

## [1.5.0] - 2026-06-16

Configurable WebSocket read limit for large documents.

### Added
- `Options.ReadLimit` sets the maximum WebSocket message the server reads from
  one client frame. Raise it (for example `128 << 20`) for large collaborative
  documents whose sync frame would otherwise exceed the limit and trigger a
  reconnect loop, or set `-1` for unlimited. Zero keeps the previous 32 KiB
  default.
- `yserve` gains `-read-limit`, plus `-awareness-timeout` and
  `-max-awareness-clients` for tuning the presence layer from the binary.

## [1.4.0] - 2026-06-15

Presence and decode DoS hardening plus a fuzz suite.

**Upgrade impact** — two server defaults changed. No code edits are required,
but an existing deployment behaves differently after upgrading.

1. Presence sweeping is now unconditional. `server.New` treats
   `Options.AwarenessTimeout == 0` as 30s and always starts the sweep
   goroutine; v1.3.0 shipped `Awareness.SweepOutdated` but never drove it, so
   servers evicted nothing. Any non-local awareness client whose last update is
   older than the timeout is marked offline and the removal is broadcast to the
   room. The sweep cannot be disabled, only lengthened. Clients that heartbeat
   presence (y-websocket) are unaffected, but ygo's own `client` package sends
   awareness only when the local state changes, so an idle Go peer (bot, agent,
   CLI) drops out of presence after 30s and does not come back until it changes
   state or reconnects. If you have such peers, re-send awareness periodically
   or raise `Options.AwarenessTimeout`.
2. `Options.MaxAwarenessClients == 0` now caps a room at 4096 distinct presence
   clientIDs (live entries plus tombstones not yet purged), where the count was
   previously unbounded. Past the cap, brand-new clientIDs in incoming updates
   are silently dropped and are no longer relayed to other peers;
   already-tracked clients keep updating. Set the field to a negative value to
   restore unlimited behavior.

Also note that the new awareness decode caps are hard constants, not options:
an update declaring more than 65536 entries, or carrying a single client state
larger than 64 KiB, is now a decode error. If you put large payloads into
awareness state, move them out of the presence channel before upgrading.

### Added
- `Options.AwarenessTimeout` and `Options.MaxAwarenessClients` configure the
  new presence sweep and the per-room client cap; `Awareness.SetMaxClients` and
  `Awareness.PurgeTombstones` expose the same controls to embedders driving
  awareness directly.
- A fuzz suite (`FuzzDecodeUpdate`, `FuzzApplyUpdate`, `FuzzDecodeUpdateV2`,
  `FuzzDecodeSnapshot`, plus awareness, sync, and relative-position targets)
  with discovered crashers committed as a regression corpus, fuzzed nightly and
  on every decode-path change in CI.

### Changed
- A server runs an always-on background presence sweep: silent awareness
  clients are marked offline and the removal is broadcast to the room, and
  tombstones are garbage-collected after twice the timeout (see Upgrade
  impact).

### Security
- The awareness decoder rejects length-prefix amplification before allocating:
  at most 65536 entries per update (`MaxUpdateEntries`) and 64 KiB of JSON
  state per client (`MaxStatePayloadBytes`).
- Every wire-supplied element count in the V1/V2 update, snapshot, id-set,
  state-vector, and `Any`-content decoders is bounded against the actual input
  length. A 9-byte update could previously force a multi-terabyte allocation;
  the bound never rejects a valid encoding (cross-language fixtures still
  pass).
- The server re-broadcasts only the awareness entries it actually accepted,
  re-encoded from its own state instead of relaying the raw inbound payload, so
  entries dropped by the per-room cap never reach cap-less browser peers.

## [1.3.0] - 2026-06-15

Offline-first local persistence for the sync client.

### Added
- `client.Options.LocalStore` (a `persist.Store`) makes the sync client
  offline-first: the document loads from the local store on `Connect` so it is
  usable before any network, every transaction is persisted
  watermark-incrementally via `EncodeDiff` with periodic `Flush` compaction,
  state survives restart, and edits made offline are carried up by the next
  handshake.
- gomobile `Client.EnableOfflineStore(dbPath)` gives the mobile SDK on-device
  persistence backed by pure-Go SQLite (no CGO); call it before `Connect`.

### Fixed
- `Client.Close` is idempotent and race-free: teardown runs exactly once under
  concurrent or repeated calls, the observer is removed first, and the waits
  happen outside the lock so the final local flush completes before `Close`
  returns.
- Local-store write failures are surfaced through `OnError` / the log and
  retried rather than silently lost.

## [1.2.0] - 2026-06-14

Change observers, a Go sync client, and an editable mobile SDK.

### Added
- Shared-type change observers: `Map.Observe` (add / update / delete with
  `OldValue`), `Array.Observe` and `Text.Observe` (Quill-style insert / delete
  / retain delta, formatting-aware for Text), plus `Map.ObserveDeep` /
  `Array.ObserveDeep` where events bubble from nested types carrying their
  `Path`. `MapEvent`, `KeyChange`, `ArrayEvent`, `ArrayDeltaOp` and `TextEvent`
  are exported from the root package; semantics match `yjs@13.6.31` and
  observers fire for both local edits and applied remote updates.
- The `client` package: a Go-native y-websocket / Hocuspocus provider with
  dial, handshake, incremental update broadcast, awareness exchange, offline
  edits flowing up on reconnect, and exponential backoff — the building block
  for bots, CLI tools, and server-side agents.
- The gomobile SDK is editable rather than bytes-only: `Text` / `Map`
  mutators, `UndoManager`, cursor anchors, an embedded sync `Client`, and
  `Text` / `Map.ObserveChanges` delivering each change as a Quill-style JSON
  delta a native Swift or Kotlin editor applies directly.

### Fixed
- A failed persist write in the server is logged instead of discarded, and no
  longer marks the document dirty — auto-versioning can no longer capture
  in-memory state that was never durably stored.
- `UndoManager` teardown is race-safe when a background sync client commits
  remote transactions concurrently (its closed flag is read under the lock).

## [1.1.0] - 2026-06-11

Collaborative cursors, versioned history, and the yserve binary.

### Added
- Relative positions for collaborative cursors:
  `CreateRelativePositionFromTypeIndex`,
  `CreateAbsolutePositionFromRelativePosition`, `EncodeRelativePosition` /
  `DecodeRelativePosition`. Anchors stay on the same logical character as
  concurrent edits land, and the binary form is byte-compatible with
  `Y.encodeRelativePosition`, so anchors can be exchanged with JS peers.
- `persist.VersionStore`: named point-in-time document versions kept
  independently of the update log, so compaction never touches history and
  pruning never touches live state. Save / list / load / atomic restore /
  prune, with the sqlite backend as the reference implementation (its
  `ygo_versions` table is created on `Open`, so existing databases pick it up
  with no migration step).
- `Options.VersionInterval` and `KeepVersions` turn on periodic auto-versioning
  for documents that received updates since the last sweep.
- yserve (`cmd/yserve`): the bundled server as one static binary, speaking the
  Hocuspocus wire protocol so existing `@hocuspocus/provider` and
  `y-websocket` clients connect unchanged, with SQLite persistence,
  auto-versioning flags, a `FROM scratch` Dockerfile, and it still embeds as a
  plain `http.Handler`.

### Changed
- `cmd/ygo-server` is a deprecated alias of `cmd/yserve`. It still builds and
  runs identically; new server features land in yserve only, and the alias is
  slated for removal in a future major release.

## [1.0.1] - 2026-06-09

Recursive garbage collection of deleted nested types.

### Fixed
- Deleting a populated nested shared type (a Map / Array / Text held inside
  another type) now collapses its whole subtree into garbage-collected runs, so
  a re-encoded document byte-matches `yjs@13.6.31`. Previously only the
  reference item was tombstoned while the child items stayed live in the store,
  inflating re-encodes of documents that delete large nested structures.
- Cross-language fixtures now cover Map-in-Map, Array-in-Map, two-level
  nesting, and a nested type inside an Array.

## [1.0.0] - 2026-06-09

First stable release, with commit-time garbage collection.

**Upgrade impact** — garbage collection actually runs from this release, and it
is on by default. Through v0.10.0 the commit-time GC step was a no-op, so
deleted content was retained in every document regardless of the `DisableGC`
option. From v1.0.0 every commit — including the commit that applies a remote
or persisted update's delete set — irreversibly replaces the content of items
tombstoned in that transaction with a same-length `ContentDeleted` marker on
any document created with `ygo.NewDoc()`. Two consequences: updates encoded
from a default document no longer carry deleted payloads, and loading a
previously persisted document into a default document discards the deleted
content it still held as soon as its delete set is applied. If you need history
(snapshots, time-travel, or any read of deleted content), create the document
with `ygo.NewDocWithOptions(ygo.Options{DisableGC: true})`. Items an
`UndoManager` has in scope are marked keep during the same commit, so undo and
redo of deletions keep working on a default document. No exported identifier or
signature changed, and convergence and JS interop are unaffected
(`ContentDeleted` is the form yjs itself emits).

### Added
- Subdocument lifecycle: observe added / removed / loaded GUIDs per transaction
  with `Doc.OnSubdocs`, mark a nested document to sync with `Doc.Load` /
  `ShouldLoad`, and set the wire `autoLoad` flag with `Map.SetDocWithOptions`,
  so a sync provider knows which nested documents to fetch.

### Changed
- Deleted content is freed at commit and adjacent deleted runs are merged into
  a yjs-aligned `ContentDeleted` marker, skipped when the doc is created with
  `DisableGC` or when an `UndoManager` keeps the item. Delete-heavy documents
  shrink sharply: insert-then-delete-everything goes from 35,885 to 18 bytes in
  V1, and a 260k-edit real-world trace from ~1.97 MB to ~223 KB, putting V1
  within ~1.4x of V2.
- Reference suite re-anchored to `yjs@13.6.31`, `lib0@0.2.117`,
  `y-protocols@1.0.7`.
- The public API is stable under semver from this release.

## [0.10.0] - 2026-06-09

Undo, snapshots, subdocuments, and compact V1 encoding.

**Upgrade impact** — move every ygo peer in a deployment to v0.10.0 at the same
time; do not mix it with v0.9.0 or older on the same sync wire. With
commit-time squash a document emits merged blocks that can span clocks the
receiver only partly has, and the diff encoder sends such a straddling block
whole. A v0.10.0 receiver slices it and integrates the unknown tail; a v0.9.0
receiver drops the whole block, silently loses that tail, and then stalls every
later block from that client on the now-unreachable origin. Resyncing does not
recover it, because the diff re-sends the same straddling block. JavaScript yjs
peers are unaffected, since yjs slices on integrate. No exported signature
changed and persisted documents remain readable.

### Added
- UndoManager: `ygo.NewUndoManager` / `NewUndoManagerWithOptions` give scoped
  undo/redo over Map, Array and Text, with capture-timeout grouping of bursty
  edits and tracked-origin filtering (local edits only by default).
- Snapshots and time-travel: `CreateSnapshot` / `EncodeSnapshot` /
  `DecodeSnapshot` / `RestoreSnapshot`, byte-compatible with `Y.encodeSnapshot`
  and `Y.createDocFromSnapshot`. The source document must be created with
  `DisableGC`, otherwise `RestoreSnapshot` returns the new `ErrSnapshotGC`.
- Subdocuments: `Map.SetDoc` / `Map.GetDoc` plus `Doc.GUID` / `Doc.Subdoc`,
  with a `ContentDoc` wire codec (GUID + options) matching yjs.
- `Doc.OnAfterTransaction`, plus before/after state on the transaction, for
  change-tracking observers such as sync providers.

### Changed
- Per-character edits merge into single items at commit, cutting V1 overhead to
  about 1 byte per character; the apply side slices a merged remote block that
  overlaps what the receiver already has, so peers still converge.
- V1 delete sets and state vectors emit clients in descending order, matching
  yjs byte-for-byte for multi-client documents. Decoding is order-independent
  on both sides, so only the emitted bytes change.
- 53-bit client IDs are locked in and byte-verified above 2^32, ready for the
  wider client-ID space `yjs@14` introduces.

## [0.9.0] - 2026-05-17

First public alpha of the pure-Go Yjs port.

### Added
- Yjs V1 and V2 wire formats, byte-for-byte compatible with `yjs@13.6.20` and
  verified in both directions by 109 cross-language fixtures, so JavaScript
  clients sync directly with Go servers and the reverse.
- The full Yjs shared-type set: Map, Array, Text (plain, rich-text formatting
  and Quill deltas), XmlFragment / XmlElement / XmlText, nested to arbitrary
  depth, plus a y-protocols-compatible Awareness CRDT.
- A Hocuspocus-compatible WebSocket sync server, with a stand-alone binary in
  `cmd/ygo-server` and pluggable persistence behind a `modernc.org/sqlite`
  reference store.
- Pure Go with no CGO: a bytes-only subset binds through gomobile, verified end
  to end as an iOS xcframework (device and simulator slices) and an Android AAR
  across 4 architectures.

---

# Nested module: server/backplane/nats

The optional NATS backplane adapter is a separate Go module
(`github.com/Deln0r/ygo/server/backplane/nats`) so the ygo core stays
dependency-free. It is versioned independently of ygo itself.

## [nats/0.2.0] - 2026-07-27

JetStream adapter for reliable cross-instance delivery.

### Added
- `NewJetStream` publishes each update into a persistent file-backed JetStream
  stream and consumes it with a JetStream ordered consumer that transparently
  recreates itself and resumes from its pinned start sequence across a NATS
  reconnect or a JetStream server restart, so a transient outage does not
  silently stop delivery the way core NATS can. No-loss is bounded by the
  stream's retention (`WithJSMaxAge`, default 10m); beyond it the shared Store
  backfills on the next document load. Duplicates from a consumer reset are
  safe, because ygo applies are idempotent and commutative. Options:
  `WithJSPrefix`, `WithStreamName`, `WithJSMaxAge`.
- The stream is created once and never updated, so one instance cannot silently
  rewrite a shared stream's configuration; its default name is namespaced by
  the subject prefix, and a colliding name whose subjects do not match fails
  fast.

### Changed
- Adapter documentation corrected: presence/awareness *is* carried over the
  backplane, since payloads are opaque to the adapter.

## [nats/0.1.0] - 2026-07-16

Core NATS backplane adapter.

### Added
- `New(nc, opts...)` fans document updates between ygo server instances over
  NATS, satisfying the `backplane.Backplane` interface. The caller owns the
  connection; `docName` is base64url-encoded into the subject so names
  containing `.`, spaces or the `*`/`>` wildcards cannot break routing, and
  self-publishes are filtered by an instance-origin header.
- `Subscribe` flushes before returning, so the subscription is registered on
  the client's own NATS server before the caller proceeds — the ordering the
  server's subscribe-before-load relies on.
- Delivery is core-NATS at-most-once. Where a dropped delta is unacceptable,
  use `NewJetStream` (v0.2.0) instead.

---

# Nested module: integration/matrix

The Matrix transport is a separate Go module
(`github.com/Deln0r/ygo/integration/matrix`) so that importing ygo does not
pull mautrix and its transitive tree into a build that only wants the CRDT. It
is versioned independently of ygo itself.

## [matrix/0.1.1] - 2026-09-04

Documentation only; no behaviour change.

### Documented
- The untrusted-input section said a bad publisher "cannot deny the room to
  everybody else". That is true of the read path and only of the read path: a
  room member needs no malformed input at all to destroy the document, because
  Yjs deletes are unauthenticated (see the 1.18.1 entry). The module README now
  carries that as a first-class limit - room membership is write access to the
  document - and `SyncResult.Skipped` says which of the two it protects.

## [matrix/0.1.0] - 2026-09-03

Carry ygo updates over a Matrix room, with no ygo server involved.

### Added
- `New(client, roomID)`, `Publish`, `PublishDoc` and `Sync` move whole update
  blobs through a Matrix room as `dev.ygo.update` events. A room is an
  append-only log with neither ordering nor exactly-once delivery, which is the
  delivery model Yjs updates tolerate; the three properties that make it work -
  idempotence, order independence, and holding an early-arriving update pending -
  are pinned by tests in the core module rather than assumed.
- Untrusted-input handling throughout, because room content is written by
  whoever is in the room: `ygo.ValidateUpdate` on both the publish and the read
  path (so an update is decoded strictly and integrated exactly once), responses
  decoded one event at a time so a single event whose `content` is not an object
  costs one event instead of a whole page, a 40,000-byte ceiling on raw updates
  checked before decoding, and `context` observed between events since
  integration is superlinear in the conflicts an update carries.
- History is read backward from the `/sync` `prev_batch` token. An empty page is
  not the end of history (the spec ends pagination by omitting `end`), a
  re-issued pagination token is refused rather than followed forever, a room
  absent from an initial sync is an error instead of a quiet empty success, and
  a timeline marked limited with no `prev_batch` fails rather than truncating
  silently. When `/sync` itself is unusable, pagination falls back to the
  newest page.
- End-to-end-encrypted rooms are refused in both directions (`ErrRoomEncrypted`)
  rather than written to in the clear, which a homeserver accepts without
  complaint.
- Integration tests and a convergence demo run against a real Dendrite in CI on
  every push, alongside a unit suite against a deliberately strict in-process
  double.

### Known limits
Documented in the module README rather than papered over: full-state publishing
hits the Matrix event-size ceiling as a document grows, rooms grow without
compaction and every sync re-reads the whole room, merge cost is quadratic in
conflicting items, a room set to `history_visibility: joined` hands a newcomer a
partial document, redaction makes old and new readers diverge, and federation is
not tested (the compose stack runs a single homeserver).

[1.19.0]: https://github.com/Deln0r/ygo/releases/tag/v1.19.0
[1.18.1]: https://github.com/Deln0r/ygo/releases/tag/v1.18.1
[1.18.0]: https://github.com/Deln0r/ygo/releases/tag/v1.18.0
[1.17.0]: https://github.com/Deln0r/ygo/releases/tag/v1.17.0
[1.16.0]: https://github.com/Deln0r/ygo/releases/tag/v1.16.0
[1.15.0]: https://github.com/Deln0r/ygo/releases/tag/v1.15.0
[1.14.0]: https://github.com/Deln0r/ygo/releases/tag/v1.14.0
[1.13.0]: https://github.com/Deln0r/ygo/releases/tag/v1.13.0
[1.12.0]: https://github.com/Deln0r/ygo/releases/tag/v1.12.0
[1.11.0]: https://github.com/Deln0r/ygo/releases/tag/v1.11.0
[1.10.0]: https://github.com/Deln0r/ygo/releases/tag/v1.10.0
[1.9.0]: https://github.com/Deln0r/ygo/releases/tag/v1.9.0
[1.8.0]: https://github.com/Deln0r/ygo/releases/tag/v1.8.0
[1.7.0]: https://github.com/Deln0r/ygo/releases/tag/v1.7.0
[1.6.1]: https://github.com/Deln0r/ygo/releases/tag/v1.6.1
[1.6.0]: https://github.com/Deln0r/ygo/releases/tag/v1.6.0
[1.5.0]: https://github.com/Deln0r/ygo/releases/tag/v1.5.0
[1.4.0]: https://github.com/Deln0r/ygo/releases/tag/v1.4.0
[1.3.0]: https://github.com/Deln0r/ygo/releases/tag/v1.3.0
[1.2.0]: https://github.com/Deln0r/ygo/releases/tag/v1.2.0
[1.1.0]: https://github.com/Deln0r/ygo/releases/tag/v1.1.0
[1.0.1]: https://github.com/Deln0r/ygo/releases/tag/v1.0.1
[1.0.0]: https://github.com/Deln0r/ygo/releases/tag/v1.0.0
[0.10.0]: https://github.com/Deln0r/ygo/releases/tag/v0.10.0
[0.9.0]: https://github.com/Deln0r/ygo/releases/tag/v0.9.0
[nats/0.2.0]: https://github.com/Deln0r/ygo/releases/tag/server/backplane/nats/v0.2.0
[nats/0.1.0]: https://github.com/Deln0r/ygo/releases/tag/server/backplane/nats/v0.1.0
[matrix/0.1.1]: https://github.com/Deln0r/ygo/releases/tag/integration/matrix/v0.1.1
[matrix/0.1.0]: https://github.com/Deln0r/ygo/releases/tag/integration/matrix/v0.1.0
