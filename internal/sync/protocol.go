// Package sync implements the y-protocols/sync wire format and the
// Hocuspocus outer message envelope. It owns the message-type
// constants, encode/decode primitives, and per-connection state
// machine that wraps an *encoding.Pending-backed Doc plus an
// *awareness.Awareness.
//
// Two layers as described in docs/yrs-port-notes/protocol-sync.md:
//
//   - Inner sync subprotocol: SyncStep1 / SyncStep2 / Update.
//     The handshake every Yjs client-server pair performs on
//     connect, plus the steady-state Update broadcast.
//
//   - Outer envelope: a varuint message-type tag that multiplexes
//     Sync alongside Awareness (and, for full Hocuspocus support,
//     Auth / QueryAwareness / Stateless / SyncStatus / Ping /
//     Pong). The bare y-websocket subset uses only tags 0 and 1;
//     ygo v0.1 ships that subset plus tag 3 (QueryAwareness,
//     necessary for Hocuspocus client compatibility per gotcha 1
//     in the port note).
//
// The package does not own the WebSocket transport — that lives in
// `server/`. Splitting transport from protocol keeps the framing
// re-usable for testing, alternative transports (a TCP/WS hybrid
// for benchmarks, a unix-socket variant for in-process tests), and
// future Hocuspocus extensions.
package sync

// MessageType is the outer-envelope varuint discriminator. Values
// match @hocuspocus/server `MessageType` enum from
// packages/server/src/types.ts:56-67.
//
// ygo v0.1 implements MessageSync, MessageAwareness, and
// MessageQueryAwareness. The remaining tags are decoded but
// dispatched to the OnUnknownMessage hook so adopters can add
// custom handling without recompiling.
type MessageType uint8

const (
	// MessageSync wraps a y-protocols/sync sub-message (SyncStep1 /
	// SyncStep2 / SyncUpdate). Payload layout:
	//   varuint(SyncSubType) • varbuffer(payload)
	MessageSync MessageType = 0

	// MessageAwareness wraps an awareness update blob — see
	// internal/awareness for the inner wire format. Payload layout:
	//   varbuffer(awarenessUpdateBytes)
	MessageAwareness MessageType = 1

	// MessageAuth carries the Hocuspocus auth-token handshake. Not
	// implemented in v0.1; bare y-websocket does not support it.
	// Deferred per docs/tech-debt.md.
	MessageAuth MessageType = 2

	// MessageQueryAwareness is sent by a client (typically right
	// after connect) to request the full current awareness snapshot.
	// Payload is empty. Server replies with a MessageAwareness frame
	// containing every known client's state. Mandatory for
	// Hocuspocus-client interop even when nothing else from
	// Hocuspocus is implemented (port-note gotcha 1).
	MessageQueryAwareness MessageType = 3

	// MessageSyncReply mirrors MessageSync but originates from the
	// server side (used by Hocuspocus internals). Wire layout is
	// identical. We decode it but treat it as MessageSync.
	MessageSyncReply MessageType = 4

	// MessageStateless / MessageBroadcastStateless / MessageClose /
	// MessageSyncStatus / MessagePing / MessagePong are Hocuspocus
	// extensions. Decoded into typed frames; not actively handled
	// in v0.1.
	MessageStateless          MessageType = 5
	MessageBroadcastStateless MessageType = 6
	MessageClose              MessageType = 7
	MessageSyncStatus         MessageType = 8
	MessagePing               MessageType = 9
	MessagePong               MessageType = 10
)

// SyncSubType is the inner-message varuint discriminator inside a
// MessageSync frame. Values match y-protocols/sync.js:37-39.
type SyncSubType uint8

const (
	// SyncStep1 carries a state vector. The receiver replies with
	// SyncStep2 containing the diff the sender is missing.
	SyncStep1 SyncSubType = 0

	// SyncStep2 carries a V1 update — the response to a SyncStep1.
	// May carry an empty update (well-formed bytes encoding zero
	// blocks and zero delete-set ranges) when the receiver has
	// nothing the sender is missing.
	SyncStep2 SyncSubType = 1

	// SyncUpdate carries an unsolicited V1 update — a steady-state
	// edit being broadcast to peers. Wire format is identical to
	// SyncStep2; the only semantic difference is that SyncUpdate
	// is not expected to be a reply to a SyncStep1.
	SyncUpdate SyncSubType = 2
)

// AuthSubType discriminates the inner Auth-message variant inside a
// MessageAuth envelope. Values match @hocuspocus/common auth.ts
// AuthMessageType, the same in 2.15.3, 3.4.4 and 4.8.0. Before 1.36.0
// ygo had them in the reverse order.
//
// Wire shape:
//
//	varuint(MessageAuth)       outer tag = 2
//	varuint(AuthSubType)       0=Token, 1=PermissionDenied, 2=Authenticated
//	varstring(payload)         token | reason | scope (per sub-type)
type AuthSubType uint8

const (
	// AuthToken is the client's "here is my token" handshake:
	// varstring(token), and from provider 4 on a varstring(version)
	// that ygo ignores. From a server, with no fields, it asks the
	// client to send its token again.
	AuthToken AuthSubType = 0

	// AuthPermissionDenied is the server's "your token was
	// rejected" response. Payload is a varstring with a human-
	// readable reason.
	AuthPermissionDenied AuthSubType = 1

	// AuthAuthenticated is the server's "your token was accepted"
	// ack. Payload is the scope, ScopeReadWrite or ScopeReadOnly.
	AuthAuthenticated AuthSubType = 2
)

// The scopes an Authenticated reply carries (@hocuspocus/common).
const (
	ScopeReadWrite = "read-write"
	ScopeReadOnly  = "readonly"
)

// CloseStatus codes the server may include in a MessageClose
// envelope. These map onto WebSocket close codes per the
// Hocuspocus convention.
const (
	// CloseStatusUnauthorized maps to WS close code 4401 — the
	// reserved code Hocuspocus uses for failed authentication.
	CloseStatusUnauthorized = 4401
)
