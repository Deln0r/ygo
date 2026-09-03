# Matrix transport for ygo

Carry ygo document updates over a [Matrix](https://matrix.org) room, so peers
synchronise through a room they share instead of through a server you run.

This is a **separate Go module** (`github.com/Deln0r/ygo/integration/matrix`),
so importing ygo does not pull mautrix and its transitive tree into a build
that only wants the CRDT. Import it only if you want Matrix.

> **Not yet importable standalone.** The module pins a core version that
> predates `ygo.ValidateUpdate`, which it calls; the pin is bumped once the
> core release carrying that function lands. Building inside this repository
> works today, via the `replace` directive in its `go.mod`.

```go
import (
    "maunium.net/go/mautrix"
    ygo "github.com/Deln0r/ygo"
    ymatrix "github.com/Deln0r/ygo/integration/matrix"
)

client, _ := mautrix.NewClient("https://matrix.example", userID, accessToken)
tr, _ := ymatrix.New(client, roomID)   // you own the client and the room

doc := ygo.NewDoc()
// ... edit doc ...
tr.PublishDoc(ctx, doc)                // publish local state as a room event
res, err := tr.Sync(ctx, doc)          // merge everything the room holds
```

## Why a Matrix room works as a CRDT transport

A room is an append-only log that any homeserver replicating it can serve. It
guarantees neither ordering nor exactly-once delivery — which is precisely the
delivery model Yjs updates tolerate:

- applying an update twice is a no-op,
- arrival order does not affect the result,
- an update that arrives before its causal predecessor waits in the pending
  buffer instead of corrupting the document.

None of the three is assumed here. All three are pinned in the core module by
`TestApplyUpdate_IsIdempotent`, `TestApplyUpdate_OrderIndependent` and
`TestApplyUpdate_HoldsClockGapPending`. The third one was a real defect found
while building this transport: a delta whose first clock sat above what the
reader had was integrated over the hole, and the update that would have filled
the hole was then discarded as already-known. Reading a room backward is
exactly the delivery pattern that triggers it.

This complements, and does not replace, the centralised path: ygo already ships
a Hocuspocus-compatible WebSocket [server](../../server). Use that when you run
the infrastructure; use this when you would rather not.

## Deliberately thin

The transport moves whole update blobs and nothing else. No state-vector
diffing, no session negotiation, no server of its own.

`PublishDoc` sends full document state. That is the only shape that is safe
with no ordering guarantee at all: a full state depends on nothing that came
before it. `Publish` will take a delta from `ygo.EncodeDiff` if you want
smaller events — deltas converge too, because an early-arriving delta waits for
its predecessor — but a delta whose predecessor is never published stays
invisible forever, and that failure mode is silent. Full state has none.

## On the wire

One event type, `dev.ygo.update`, with a body of:

```json
{ "format": "yjs-v1", "payload": "<base64 of a Yjs V1 update>" }
```

The `format` field exists so a future encoding can be introduced without making
old events unreadable: a peer skips formats it does not know instead of failing
the sync.

Updates are validated **in both directions**, with `ygo.ValidateUpdate`, which
parses without integrating:

- **On publish**, so a corrupt local export fails for the peer that produced it
  rather than becoming every reader's problem. This also catches two updates
  concatenated with `append` instead of combined with `ygo.MergeUpdates` —
  applying such a buffer reads the first update and silently discards the rest,
  in ygo and in yjs alike.
- **On read**, because room content is untrusted input. An unknown format, an
  oversized payload, undecodable base64 or an update the parser rejects is
  skipped and counted in `SyncResult.Skipped`, never fatal.

Responses are decoded one event at a time rather than as a single typed tree.
That distinction is the difference between skipping one bad event and losing a
page: an event whose `content` is a string rather than an object fails a
whole-response decode, taking every legitimate update in that page with it.
Dendrite's own `/send` rejects that shape (`M_BAD_JSON`, measured 2026-09-03),
so it is not something an ordinary member of a Dendrite room can post — it
would arrive from another server implementation, over federation, or from a
server-side defect. The tolerance is kept anyway, because the cost of being
wrong about reachability is losing the room.

## Reading the room

`Sync` takes an initial `/sync`, merges the timeline it returns, then pages
**backward** from that timeline's `prev_batch` token until the server stops
handing out an `end` token. An empty page is not the end of history — the spec
ends pagination by omitting `end` — so an empty chunk mid-history does not stop
the walk.

It never pages from an empty token on that path. Measured against
`ghcr.io/element-hq/dendrite-monolith` on 2026-09-03: Dendrite *accepts* an
empty `from` and answers `200` with only the newest page, while a *malformed*
token gets `400 M_INVALID_PARAM "Invalid from parameter: malformed sync
token"`. The accepting case is the dangerous one, because silent truncation
looks exactly like success. `TestDendrite_TokenHandling` asserts both against
the real server — over raw HTTP for the empty case, because every client
library drops a `from` it considers unset, which is how an earlier version of
that test ended up asserting nothing at all.

Empty-token pagination is used in exactly one place: as a fallback when the
initial `/sync` fails outright, since a single pathological event can break
`/sync` for a whole room on some servers while `/messages` keeps working. If
the fallback cannot start either, the original `/sync` error is what you get.

Within one `Sync`, events are deduplicated by event ID, because the `/sync`
window and the first backward page overlap on some servers. Nothing is
remembered *between* calls: a `Transport` holds no document, and a dedup set
that outlived the call would hand a second document an empty room.

`Transport` is not safe for concurrent use — one per goroutine. One `Transport`
is bound to one room (fixed by `New`) and can serve any number of documents.

## Limits, honestly

Everything below is a consequence of being thin. None of it is hidden by the
API; most of it is a reason to use the [WebSocket server](../../server)
instead once a document gets serious.

- **Event size.** Matrix caps a complete event at 65,536 bytes and base64
  costs a third on top, so `MaxUpdateBytes` is 40,000 raw bytes. `PublishDoc`
  therefore stops working once a document's full state exceeds that; the
  failure is a clear local error, not an opaque server rejection.
- **Room growth and read cost.** Every publish appends; nothing is ever
  compacted. Publishing successive full states makes room storage grow
  quadratically in the number of publishes, and every `Sync` re-reads the whole
  room from the beginning — there is no incremental cursor.
- **Merge cost is superlinear.** Integrating an update is quadratic in the
  number of items conflicting at one position, so a room holding
  hostile-but-legal events is slow to read. That cost belongs to YATA and the
  reference implementation shares it; what this transport does is refuse
  oversized payloads, integrate each update exactly once, and check the
  context between events so a caller's deadline still means something.
- **Pending buffer.** An update that decodes but can never integrate (its
  dependencies are not in the room) stays queued in the document. A room open
  to strangers can therefore grow a peer's pending buffer.
- **History visibility.** A room set to `history_visibility: joined` hands a
  newcomer nothing published before they joined, and the document they rebuild
  is quietly partial. The tests create rooms with `shared` explicitly.
- **Redaction.** Redacting an event strips the payload. The update is gone for
  anyone who reads the room afterwards, while peers that already merged it keep
  it — old and new readers diverge, permanently.
- **Encryption is refused, not ignored.** This transport does not implement
  Megolm. In an encrypted room, publishing would put document contents in the
  clear — Dendrite accepts a plaintext event there without complaint (measured
  2026-09-03) — and reading would skip every real event and report a serenely
  empty room. Both directions return `ErrRoomEncrypted` instead.
- **A freshly joined room is not immediately visible.** Matrix is eventually
  consistent, so the `/sync` right after a join may not list the room yet and
  `Sync` returns `ErrRoomUnavailable` for it. That is a state to poll through,
  not a permanent failure — but it is deliberately not swallowed, because the
  same error is what a wrong room ID produces, and a quiet empty success there
  is indistinguishable from a healthy document.
- **Server-side denial is out of scope.** A member who can make a homeserver
  fail can deny the room; the `/messages` fallback above covers the case where
  `/sync` alone is broken, and nothing here covers a server that is down.

## Tests

`go test ./...` runs the unit suite against an in-process homeserver double and
skips the integration tests when no homeserver is reachable.

The double is deliberately **stricter** than Dendrite where strictness catches
our own bugs (it refuses empty pagination tokens outright, and requires `dir`),
and an exact imitation where fidelity matters (malformed tokens fail the same
way). It returns one event per page, so a client that reads a single page and
stops fails the suite rather than passing it. The difference between imitation
and house rule is spelled out in `fakehs_test.go`, and the imitation half is
pinned against the real server so it cannot drift into being more permissive
than production.

For the full suite, including the real homeserver:

```bash
./testdata/up.sh          # Dendrite on :8008, ready in about a second
go test -race ./...
go run ./cmd/matrixdemo   # convergence check: exits non-zero if peers disagree
./testdata/down.sh
```

Both run in CI on every push, against a real Dendrite.

The demo is a check, not a printout. Two peers edit while they cannot see each
other, publish in the inconvenient order (the second publishes first), read the
room back, and the program fails unless the documents match *and* both edits
survived — convergence on an empty document is also convergence and proves
nothing. It does not swallow sync errors, and it reads into a fresh document on
every attempt, so reaching the target means one `Sync` read the whole room
rather than two partial reads adding up.

## What is not tested

Federation. The compose stack runs a **single** homeserver, so every test here
is client-to-server behaviour. What the newcomer test does establish is the
property federation would carry — that a peer needs the room and nothing else,
with no ygo server anywhere.
