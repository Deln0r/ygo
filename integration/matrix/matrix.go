// Package matrix carries ygo document updates over a Matrix room.
//
// A Matrix room is an append-only log that any homeserver in the federation
// can replicate, so it works as a transport for CRDT updates without ygo
// running a server of its own: each peer publishes its local edits as room
// events and merges whatever it reads back. Matrix guarantees neither
// ordering nor exactly-once delivery, which is exactly the delivery model
// Yjs updates tolerate - applying one twice is a no-op, order does not affect
// the result, and an update that arrives before its causal predecessor waits
// in the pending buffer instead of corrupting the document (pinned by
// TestApplyUpdate_IsIdempotent, TestApplyUpdate_OrderIndependent and
// TestApplyUpdate_HoldsClockGapPending in the core module).
//
// This is deliberately a THIN transport: it moves whole update blobs and
// nothing else. No state-vector diffing, no session negotiation, no server.
// The centralised path already exists (github.com/Deln0r/ygo/server, a
// Hocuspocus-compatible WebSocket server); this is the federated alternative,
// not a second implementation of the same thing. Read README.md for what that
// thinness costs - the limits are real and they are documented rather than
// papered over.
//
// It is a separate Go module, so importing ygo does not pull mautrix and its
// transitive tree into a build that only wants the CRDT: adopters who want
// Matrix opt in by importing this package.
package matrix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/Deln0r/ygo"
)

// EventType is the room event type carrying a ygo update. Custom event types
// are namespaced by reverse domain per the Matrix spec; a room may carry
// other traffic (messages, state) and this transport ignores all of it.
var EventType = event.Type{Type: "dev.ygo.update", Class: event.MessageEventType}

// FormatV1 names the payload encoding: a Yjs V1 update, the format
// ygo.EncodeStateAsUpdate emits and ApplyUpdate consumes. The field exists so
// a future encoding (V2, or a compressed envelope) can be introduced without
// making old events unreadable: a peer skips formats it does not know rather
// than failing the whole sync.
const FormatV1 = "yjs-v1"

// MaxUpdateBytes is the largest raw update this transport will publish or
// accept.
//
// The Matrix spec caps a complete event at 65536 bytes, and that budget has
// to cover base64 (4 bytes out per 3 in) plus the event's own JSON and the
// server-added fields. 40000 raw bytes becomes ~53336 bytes of base64 and
// leaves the rest as headroom. The cap is enforced on both sides: publishing
// past it fails locally with a readable message instead of an opaque
// M_TOO_LARGE from the server, and reading past it refuses an oversized
// payload before spending anything on decoding it.
const MaxUpdateBytes = 40000

// maxPages bounds one Sync's walk backward through history. A room is
// finite, so reaching this means the server is not advancing its pagination
// token in a way that terminates; erroring out beats looping forever.
const maxPages = 10000

// Payload is the event body. Keep it JSON-serialisable and boring: it is
// written by us and read by anyone.
type Payload struct {
	// Format names the encoding of Update. Unknown formats are skipped.
	Format string `json:"format"`
	// Update is a Yjs update, base64 (standard, padded) because Matrix event
	// bodies are JSON and updates are arbitrary bytes.
	Update string `json:"payload"`
}

// ErrEmptyUpdate reports an attempt to publish zero bytes.
var ErrEmptyUpdate = errors.New("matrix: refusing to publish an empty update")

// ErrRoomEncrypted reports that the room uses Matrix end-to-end encryption,
// which this transport does not implement. See Transport for why it refuses
// rather than proceeds.
var ErrRoomEncrypted = errors.New("matrix: room is end-to-end encrypted and this transport does not implement Matrix encryption")

// ErrRoomUnavailable reports that the room did not appear in an initial
// /sync: the account is not joined, or the room ID is wrong.
var ErrRoomUnavailable = errors.New("matrix: room absent from an initial sync (not joined, or wrong room ID)")

// Transport publishes and reads ygo updates in one Matrix room. It holds no
// document: the caller owns the *ygo.Doc and decides when to publish and when
// to sync, so this package never has to guess a merge policy, and one
// Transport can serve more than one document in its room - each Sync reads
// the room from the beginning and is independent of every other Sync.
//
// Not safe for concurrent use by multiple goroutines: the cached room-
// encryption answer is unsynchronised. One Transport per goroutine is the
// intended shape.
type Transport struct {
	client *mautrix.Client
	roomID id.RoomID

	// encryptionKnown/encrypted cache one lookup of the room's
	// m.room.encryption state. Measured against Dendrite on 2026-09-03:
	// posting a plaintext event into an encrypted room SUCCEEDS, returning a
	// normal event ID, so nothing about the send path reveals the mistake.
	// The state event is also absent from an initial /sync's room state
	// there, so it has to be asked for directly - once per Transport, then
	// remembered.
	encryptionKnown bool
	encrypted       bool
}

// New returns a Transport for roomID over an authenticated client. The caller
// owns the client (its homeserver URL, access token and lifetime); Transport
// never closes it and never joins the room on its own.
func New(client *mautrix.Client, roomID id.RoomID) (*Transport, error) {
	if client == nil {
		return nil, errors.New("matrix: nil client")
	}
	if roomID == "" {
		return nil, errors.New("matrix: empty room ID")
	}
	return &Transport{client: client, roomID: roomID}, nil
}

// checkPlaintextAllowed refuses encrypted rooms, in both directions.
//
// Publishing into one without implementing Megolm would put document
// contents in the clear in a room whose members were promised otherwise -
// the server accepts it silently. Reading one is equally broken the other
// way: real members' events arrive as m.room.encrypted, this transport skips
// every one of them, and Sync reports a serenely empty room. Refusing is the
// only honest answer a thin transport can give.
func (t *Transport) checkPlaintextAllowed(ctx context.Context) error {
	if !t.encryptionKnown {
		var content map[string]any
		err := t.client.StateEvent(ctx, t.roomID, event.StateEncryption, "", &content)
		switch {
		case err == nil:
			t.encrypted, t.encryptionKnown = true, true
		case isNotFound(err):
			// No m.room.encryption state: an ordinary unencrypted room.
			t.encrypted, t.encryptionKnown = false, true
		default:
			// Do not cache a network failure as "unencrypted".
			return fmt.Errorf("matrix: check room encryption: %w", err)
		}
	}
	if t.encrypted {
		return fmt.Errorf("%w (room %s)", ErrRoomEncrypted, t.roomID)
	}
	return nil
}

func isNotFound(err error) bool {
	var he mautrix.HTTPError
	if errors.As(err, &he) {
		return he.RespError != nil && he.RespError.ErrCode == "M_NOT_FOUND"
	}
	return false
}

// Publish sends one update to the room. The update is parsed before it is
// sent: a corrupt local export, or two updates accidentally concatenated
// instead of merged, fails here for the peer that produced it instead of
// becoming every reader's problem. Publishing the same update twice is legal
// - applying twice is a no-op - though the two events are distinct events
// with distinct IDs, so readers will decode and apply both.
func (t *Transport) Publish(ctx context.Context, update []byte) (id.EventID, error) {
	if len(update) == 0 {
		return "", ErrEmptyUpdate
	}
	if len(update) > MaxUpdateBytes {
		return "", fmt.Errorf("matrix: update is %d bytes, over the %d-byte limit one Matrix event can carry; publish a smaller delta or move the document to the WebSocket server", len(update), MaxUpdateBytes)
	}
	if err := ygo.ValidateUpdate(update); err != nil {
		return "", fmt.Errorf("matrix: refusing to publish an invalid update: %w", err)
	}
	if err := t.checkPlaintextAllowed(ctx); err != nil {
		return "", err
	}
	body := Payload{Format: FormatV1, Update: base64.StdEncoding.EncodeToString(update)}
	resp, err := t.client.SendMessageEvent(ctx, t.roomID, EventType, body)
	if err != nil {
		return "", fmt.Errorf("matrix: send update: %w", err)
	}
	return resp.EventID, nil
}

// PublishDoc exports doc's full state and publishes it.
//
// Whole-state publishing is what keeps the transport thin, and it is the only
// shape that is safe with no ordering guarantee at all. A caller that wants
// smaller events can compute a delta with ygo.EncodeDiff and hand it to
// Publish - deltas arriving out of order are held in the pending buffer until
// their predecessor turns up, so they converge too, but a delta whose
// predecessor is never published stays invisible forever. Full state has no
// such failure mode, which is why it is the default here. Its cost is size:
// see MaxUpdateBytes and README.md.
func (t *Transport) PublishDoc(ctx context.Context, doc *ygo.Doc) (id.EventID, error) {
	return t.Publish(ctx, ygo.EncodeStateAsUpdate(doc))
}

// SyncResult reports what one Sync did.
type SyncResult struct {
	// Applied is the number of update events accepted and handed to the
	// document during this call. Note both halves of that: Sync reads the
	// room from the beginning every time, so a second Sync over an unchanged
	// room reports the same count again - the number describes the room, not
	// what is new - and an update whose causal dependencies are not in the
	// room counts here while sitting in the document's pending buffer rather
	// than in its content. To learn whether anything actually changed,
	// compare ygo.EncodeStateVector(doc) across the call.
	Applied int
	// Skipped counts events of our type that could not be used: a content
	// that is not a JSON object, an unknown format, an oversized payload,
	// undecodable base64, or an update the parser rejected. A bad event is
	// skipped rather than fatal, so one malformed publisher cannot deny the
	// READ PATH to everyone else.
	//
	// That is availability, not integrity, and the difference matters. Yjs
	// updates do not authenticate deletes: anyone who can publish to the room
	// can tombstone another peer's content or push a client's clock past its
	// own writes, in a handful of legal bytes, exactly as they could by making
	// an ordinary edit. The reference implementation behaves the same way
	// (measured). A room carrying a ygo document is therefore as trusted as
	// the document itself - invite accordingly.
	Skipped int
}

// Sync reads the room's history and merges every ygo update it finds into
// doc. It is safe to call repeatedly and safe to call with different
// documents: nothing is remembered between calls, so a second document gets
// the same complete history as the first.
//
// The read starts from an initial /sync (which yields the newest timeline
// slice plus a prev_batch token) and then pages BACKWARD from that token
// until the server stops handing out an end token. Reading in reverse is
// invisible in the result because merging is order-independent.
//
// On failure doc may already hold part of the room: Sync merges as it reads,
// and a CRDT has nothing to roll back to. A caller that retries should retry
// into the SAME document - starting a fresh one is also correct, just slower.
func (t *Transport) Sync(ctx context.Context, doc *ygo.Doc) (SyncResult, error) {
	var out SyncResult
	if err := t.checkPlaintextAllowed(ctx); err != nil {
		return out, err
	}

	// seen is per-call, not per-Transport. It exists for one reason: the
	// /sync window and the first backward page overlap on some homeservers,
	// so the same event arrives twice within this call and would be counted
	// twice. Keeping it across calls would be worse than useless - it would
	// make a second Sync into a DIFFERENT document silently skip the entire
	// history, and it would grow without bound on a room anyone can post to.
	seen := make(map[id.EventID]struct{})

	from, syncErr := t.readSyncWindow(ctx, doc, seen, &out)
	if syncErr != nil {
		// A single room member can break /sync for everyone joined to the
		// room - a legal but pathological event is enough on some servers -
		// while /messages on the same room keeps working. Falling back to
		// pagination alone turns a permanent outage into a slower read. If
		// the fallback cannot start either, the original error is the more
		// informative one to report.
		fallback, err := t.readNewestPage(ctx, doc, seen, &out)
		if err != nil {
			return out, syncErr
		}
		from = fallback
	}

	visited := map[string]struct{}{}
	for pages := 0; from != ""; pages++ {
		if pages >= maxPages {
			return out, fmt.Errorf("matrix: paginate room history: still going after %d pages; the server is not converging on the start of history", maxPages)
		}
		if _, loop := visited[from]; loop {
			return out, fmt.Errorf("matrix: paginate room history: server returned a pagination token it had already issued (%q); refusing to loop", from)
		}
		visited[from] = struct{}{}

		page, err := t.messages(ctx, from)
		if err != nil {
			return out, fmt.Errorf("matrix: paginate room history: %w", err)
		}
		if err := t.mergeEvents(ctx, page.Chunk, doc, seen, &out); err != nil {
			return out, err
		}
		// An empty chunk is NOT the end of history: the spec ends pagination
		// by omitting `end`, and a server may legally hand back an empty page
		// with a token that leads to more. Stopping on the empty chunk drops
		// everything past it and still returns success.
		from = page.End
	}
	return out, nil
}

// readSyncWindow merges the newest slice of the room and returns the token to
// continue backward from.
func (t *Transport) readSyncWindow(ctx context.Context, doc *ygo.Doc, seen map[id.EventID]struct{}, out *SyncResult) (string, error) {
	var resp syncResponse
	u := t.client.BuildURLWithQuery(mautrix.ClientURLPath{"v3", "sync"}, map[string]string{"timeout": "0"})
	if _, err := t.client.MakeRequest(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return "", fmt.Errorf("matrix: initial sync: %w", err)
	}
	room, joined := resp.Rooms.Join[t.roomID]
	if !joined {
		// A since-less sync lists every joined room, empty ones included
		// (measured against Dendrite on 2026-09-03: a freshly created room
		// with no messages is present). So absence is not "nobody has
		// published yet" - it means this account cannot see the room, and
		// returning an empty success would let a typo in a room ID look like
		// a healthy, quiet document.
		return "", fmt.Errorf("%w: %s", ErrRoomUnavailable, t.roomID)
	}
	if err := t.mergeEvents(ctx, room.Timeline.Events, doc, seen, out); err != nil {
		return "", err
	}
	if room.Timeline.Limited && room.Timeline.PrevBatch == "" {
		// The server says it truncated the timeline but gave nothing to
		// continue from. Everything older is unreachable; reporting success
		// would present a partial document as a complete one.
		return "", errors.New("matrix: initial sync: timeline is limited but carries no prev_batch, so the rest of the room is unreachable")
	}
	return room.Timeline.PrevBatch, nil
}

// readNewestPage asks /messages for the newest page without a from-token, when
// /sync is unusable. It MERGES that page and returns the token to continue
// backward from - an earlier version returned only the token and dropped the
// events it had already fetched, losing the newest update in the room every
// time the fallback ran.
//
// Dendrite treats a missing from-token as "start at the newest" and answers
// 200 with a page and a continuation token (measured 2026-09-03); servers that
// refuse simply make this fallback unavailable, and the caller gets the
// original /sync error instead.
func (t *Transport) readNewestPage(ctx context.Context, doc *ygo.Doc, seen map[id.EventID]struct{}, out *SyncResult) (string, error) {
	page, err := t.messages(ctx, "")
	if err != nil {
		return "", err
	}
	if err := t.mergeEvents(ctx, page.Chunk, doc, seen, out); err != nil {
		return "", err
	}
	if page.End == "" {
		return "", errors.New("matrix: fallback pagination: server returned no continuation token")
	}
	return page.End, nil
}

// messages fetches one backward page. from may be empty only on the fallback
// path above.
func (t *Transport) messages(ctx context.Context, from string) (*messagesResponse, error) {
	q := map[string]string{"dir": "b", "limit": "100"}
	if from != "" {
		q["from"] = from
	}
	u := t.client.BuildURLWithQuery(mautrix.ClientURLPath{"v3", "rooms", t.roomID, "messages"}, q)
	var resp messagesResponse
	if _, err := t.client.MakeRequest(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// syncResponse and messagesResponse decode only the fields this transport
// uses, and hold each event as raw JSON.
//
// This is the difference between skipping one bad event and losing a room.
// Decoding a whole response into one typed tree - which is what mautrix's
// RespSync and RespMessages do - lets a single event whose `content` is a
// string or a number rather than an object fail the ENTIRE page, taking every
// legitimate update alongside it. TestSync_SurvivesUndecodableEvent fails
// outright on the typed path, which is how that was established.
//
// Where such an event comes from, measured rather than assumed: Dendrite's
// own /send refuses a non-object content with M_BAD_JSON (2026-09-03), so it
// is not something a member of a Dendrite room can simply post. It reaches a
// client from elsewhere - another server implementation, a federated event, a
// server-side defect. The tolerance is kept because the cost of being wrong
// about that is losing the whole room, and the cost of the tolerance is a
// smaller struct.
type syncResponse struct {
	Rooms struct {
		Join map[id.RoomID]struct {
			Timeline struct {
				Events    []json.RawMessage `json:"events"`
				Limited   bool              `json:"limited"`
				PrevBatch string            `json:"prev_batch"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

type messagesResponse struct {
	Chunk []json.RawMessage `json:"chunk"`
	End   string            `json:"end"`
}

// roomEvent is the whole of a Matrix event this transport needs. Content stays
// raw so a hostile shape costs one event, not the page.
type roomEvent struct {
	EventID id.EventID      `json:"event_id"`
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

// mergeEvents applies every well-formed, not-yet-seen update event to doc.
// It returns an error only when the caller's context is done - room content
// itself never aborts the sync.
func (t *Transport) mergeEvents(ctx context.Context, events []json.RawMessage, doc *ygo.Doc, seen map[id.EventID]struct{}, out *SyncResult) error {
	for _, raw := range events {
		// Integrating an update is superlinear in the number of conflicting
		// items it carries, so a room full of hostile-but-legal events is
		// slow to read. It cannot be made fast here (the cost is YATA's, and
		// the reference implementation shares it), but it can be made
		// interruptible, so a caller's deadline is worth something.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("matrix: merging room history: %w", err)
		}
		var evt roomEvent
		if err := json.Unmarshal(raw, &evt); err != nil {
			// Not even the envelope parses. It cannot be one of ours (ours
			// are written by Publish), so it is not counted as skipped any
			// more than a chat message is.
			continue
		}
		if evt.Type != EventType.Type {
			continue
		}
		if _, dup := seen[evt.EventID]; dup {
			continue
		}
		seen[evt.EventID] = struct{}{}

		update, ok := decodePayload(evt.Content)
		if !ok {
			out.Skipped++
			continue
		}
		if err := ygo.ApplyUpdate(doc, update); err != nil {
			// Unreachable as the two functions stand: ValidateUpdate above
			// decodes the same bytes with the same decoder, and ApplyUpdate
			// can only fail on a decode. Kept because "the validator and the
			// applier agree" is an invariant of another package, not of this
			// one, and the cost of being wrong about it is a panic-free
			// document silently taking bytes nobody checked.
			out.Skipped++
			continue
		}
		out.Applied++
	}
	return nil
}

// decodePayload extracts a usable update from an event body, or reports that
// the body is unusable. Every failure mode here is reachable from a hostile or
// merely buggy room member, so none of them panics or aborts the sync.
func decodePayload(content json.RawMessage) ([]byte, bool) {
	var p Payload
	if err := json.Unmarshal(content, &p); err != nil {
		return nil, false
	}
	if p.Format != FormatV1 {
		return nil, false // a newer peer's encoding, or junk
	}
	if base64.StdEncoding.DecodedLen(len(p.Update)) > MaxUpdateBytes {
		return nil, false // refuse before spending the decode
	}
	update, err := base64.StdEncoding.DecodeString(p.Update)
	if err != nil || len(update) == 0 || len(update) > MaxUpdateBytes {
		return nil, false
	}
	// ygo.ValidateUpdate decodes without integrating, so a malformed or
	// over-long payload is rejected for the price of a parse. Integration
	// then happens exactly once, on the caller's real document - the earlier
	// shape of this code integrated every update twice, once into a throwaway
	// probe document and once for real.
	if err := ygo.ValidateUpdate(update); err != nil {
		return nil, false
	}
	return update, true
}
