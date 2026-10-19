package sync

import (
	"errors"
	"fmt"

	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/utf16"
)

// Frame is the decoded outer-envelope message. Type indicates which
// MessageType discriminator the wire bytes started with; the
// remaining fields populate based on Type per
// docs/yrs-port-notes/protocol-sync.md §2.
//
// The decode helpers return a *Frame so callers can switch on Type
// without reaching back into raw bytes. Mutating fields on a Frame
// has no effect on the original wire bytes — the decoder copies.
type Frame struct {
	Type MessageType

	// SyncSub is set only when Type == MessageSync or MessageSyncReply.
	SyncSub SyncSubType

	// AuthSub is set only when Type == MessageAuth.
	AuthSub AuthSubType

	// Payload is the post-discriminator bytes:
	//   MessageSync          → the inner update or state vector bytes
	//                          (already unwrapped from varbuffer)
	//   MessageAwareness     → awareness update bytes
	//                          (already unwrapped from varbuffer)
	//   MessageQueryAwareness→ nil (no payload)
	//   MessageAuth          → token (AuthToken sub) or reason
	//                          (AuthPermissionDenied) — string bytes
	//   MessageStateless     → the stateless payload string bytes
	//   MessageBroadcastStateless → same
	//   MessageClose         → close reason string bytes
	//   MessageSyncStatus    → single byte 0x00 or 0x01
	//   MessagePing/Pong     → nil
	//   other                → the raw remaining envelope bytes,
	//                          left opaque for caller-defined handling
	Payload []byte
}

// ErrTruncated wraps a decode failure where the envelope ended
// before all expected fields were read.
var ErrTruncated = errors.New("sync: envelope truncated")

// ErrUnknownSyncSubType is returned when a MessageSync envelope
// carries a sub-type outside the known {0, 1, 2} set. The receiver
// has no way to interpret an unknown sub-message; the caller should
// surface the error and close the connection per y-protocols
// convention.
var ErrUnknownSyncSubType = errors.New("sync: unknown sync sub-type")

// EncodeSyncStep1 builds a MessageSync envelope carrying SyncStep1.
// sv is the V1-encoded state vector bytes (as produced by
// encoding.EncodeStateVector). The envelope is self-delimited via
// the inner varbuffer and ready to be written as a single WS frame.
//
// Wire layout (port-note §1):
//
//	varuint(0)   = MessageSync
//	varuint(0)   = SyncStep1
//	varbuffer(sv)
func EncodeSyncStep1(sv []byte) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageSync))
	buf = lib0.WriteVarUint(buf, uint64(SyncStep1))
	buf = lib0.WriteVarUint8Array(buf, sv)
	return buf
}

// EncodeSyncStep2 builds a MessageSync envelope carrying SyncStep2.
// update is the V1 update bytes (as produced by encoding.EncodeDiff
// or encoding.EncodeStateAsUpdate). An empty update is legal and
// signals "I have nothing the sender is missing" — see port-note
// gotcha 2.
func EncodeSyncStep2(update []byte) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageSync))
	buf = lib0.WriteVarUint(buf, uint64(SyncStep2))
	buf = lib0.WriteVarUint8Array(buf, update)
	return buf
}

// EncodeSyncUpdate builds a MessageSync envelope carrying an
// unsolicited Update — the steady-state broadcast.
func EncodeSyncUpdate(update []byte) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageSync))
	buf = lib0.WriteVarUint(buf, uint64(SyncUpdate))
	buf = lib0.WriteVarUint8Array(buf, update)
	return buf
}

// EncodeAwareness builds a MessageAwareness envelope carrying the
// given awareness update bytes (as produced by
// internal/awareness.Awareness.Encode).
//
// Wire layout: varuint(1) • varbuffer(awarenessUpdate)
func EncodeAwareness(awarenessUpdate []byte) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageAwareness))
	buf = lib0.WriteVarUint8Array(buf, awarenessUpdate)
	return buf
}

// EncodeQueryAwareness builds an empty-payload QueryAwareness
// envelope — the client's request for the current full awareness
// snapshot. Server replies with an EncodeAwareness frame covering
// every known client.
func EncodeQueryAwareness() []byte {
	return lib0.WriteVarUint(nil, uint64(MessageQueryAwareness))
}

// EncodePong builds a MessagePong envelope — the application-layer
// ping reply (Hocuspocus only; y-websocket uses WS-level ping).
func EncodePong() []byte {
	return lib0.WriteVarUint(nil, uint64(MessagePong))
}

// EncodeAuthToken builds a MessageAuth envelope carrying the
// client-side Token handshake — sent by a Hocuspocus client at
// connection time, consumed by the server's OnAuthenticate hook.
//
// Wire layout: varuint(MessageAuth) varuint(AuthToken) varstring(token).
func EncodeAuthToken(token string) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageAuth))
	buf = lib0.WriteVarUint(buf, uint64(AuthToken))
	return lib0.WriteVarString(buf, utf16.ValidString(token))
}

// EncodeAuthAuthenticated builds the server-side "your token was
// accepted" ack. Hocuspocus clients use this to flip an internal
// "authenticated" flag before proceeding with Sync.
func EncodeAuthAuthenticated() []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageAuth))
	buf = lib0.WriteVarUint(buf, uint64(AuthAuthenticated))
	return lib0.WriteVarString(buf, "")
}

// EncodeAuthPermissionDenied builds the server-side "your token
// was rejected" response with a human-readable reason. After
// sending, the server typically follows with an EncodeClose
// envelope and closes the WS with code 4401
// (CloseStatusUnauthorized).
func EncodeAuthPermissionDenied(reason string) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageAuth))
	buf = lib0.WriteVarUint(buf, uint64(AuthPermissionDenied))
	return lib0.WriteVarString(buf, utf16.ValidString(reason))
}

// EncodeStateless builds a MessageStateless envelope carrying an
// opaque string payload. Routed to the recipient's OnStateless
// callback (single-conn delivery).
func EncodeStateless(payload string) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageStateless))
	return lib0.WriteVarString(buf, utf16.ValidString(payload))
}

// EncodeBroadcastStateless builds a MessageBroadcastStateless
// envelope. Same payload as EncodeStateless but the recipient
// fans out to every connection on the doc.
func EncodeBroadcastStateless(payload string) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageBroadcastStateless))
	return lib0.WriteVarString(buf, utf16.ValidString(payload))
}

// EncodeClose builds a MessageClose envelope with a reason
// string. Typically followed by a WS-level close with a numeric
// status code (e.g. 4401 for unauthorized).
func EncodeClose(reason string) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageClose))
	return lib0.WriteVarString(buf, utf16.ValidString(reason))
}

// EncodeSyncStatus builds a MessageSyncStatus envelope. The
// payload is a single byte: 0x00 = not synced, 0x01 = synced.
// Servers emit 0x01 after the initial SyncStep1/SyncStep2 round
// completes so clients can flip a "ready" UI state.
func EncodeSyncStatus(synced bool) []byte {
	buf := lib0.WriteVarUint(nil, uint64(MessageSyncStatus))
	if synced {
		return append(buf, 0x01)
	}
	return append(buf, 0x00)
}

// DecodeEnvelope parses one envelope from b. Returns the decoded
// Frame plus the unconsumed tail (in case multiple envelopes are
// concatenated; over WebSocket each Read returns exactly one
// envelope and the tail is empty).
//
// Returns ErrTruncated when bytes run out mid-field, or
// ErrUnknownSyncSubType when a Sync envelope's inner discriminator
// is outside {0, 1, 2}.
func DecodeEnvelope(b []byte) (*Frame, []byte, error) {
	if len(b) < 1 {
		return nil, b, fmt.Errorf("%w: empty envelope", ErrTruncated)
	}
	typeU, n, err := lib0.ReadVarUint(b)
	if err != nil {
		return nil, b, fmt.Errorf("decode messageType: %w", err)
	}
	b = b[n:]
	mt := MessageType(typeU)

	frame := &Frame{Type: mt}

	switch mt {
	case MessageSync, MessageSyncReply:
		subU, n, err := lib0.ReadVarUint(b)
		if err != nil {
			return nil, b, fmt.Errorf("decode syncSubType: %w", err)
		}
		b = b[n:]
		sub := SyncSubType(subU)
		switch sub {
		case SyncStep1, SyncStep2, SyncUpdate:
			frame.SyncSub = sub
		default:
			return nil, b, fmt.Errorf("%w: %d", ErrUnknownSyncSubType, subU)
		}
		payload, n, err := lib0.ReadVarUint8Array(b)
		if err != nil {
			return nil, b, fmt.Errorf("decode sync payload: %w", err)
		}
		// Copy the payload so the caller can keep the Frame past
		// the next Read call's buffer reuse.
		frame.Payload = append([]byte(nil), payload...)
		return frame, b[n:], nil

	case MessageAwareness:
		payload, n, err := lib0.ReadVarUint8Array(b)
		if err != nil {
			return nil, b, fmt.Errorf("decode awareness payload: %w", err)
		}
		frame.Payload = append([]byte(nil), payload...)
		return frame, b[n:], nil

	case MessageQueryAwareness, MessagePing, MessagePong:
		// Empty payload — no further bytes consumed.
		return frame, b, nil

	case MessageAuth:
		subU, n, err := lib0.ReadVarUint(b)
		if err != nil {
			return nil, b, fmt.Errorf("decode authSubType: %w", err)
		}
		b = b[n:]
		frame.AuthSub = AuthSubType(subU)
		// Every Auth sub-type carries a varstring payload
		// (PermissionDenied = reason, Authenticated = empty,
		// Token = token bytes).
		payload, n, err := lib0.ReadVarString(b)
		if err != nil {
			return nil, b, fmt.Errorf("decode auth payload: %w", err)
		}
		frame.Payload = []byte(payload)
		return frame, b[n:], nil

	case MessageStateless, MessageBroadcastStateless, MessageClose:
		payload, n, err := lib0.ReadVarString(b)
		if err != nil {
			return nil, b, fmt.Errorf("decode %d payload: %w", mt, err)
		}
		frame.Payload = []byte(payload)
		return frame, b[n:], nil

	case MessageSyncStatus:
		// Single-byte payload: synced flag.
		if len(b) < 1 {
			return nil, b, fmt.Errorf("%w: SyncStatus needs 1 byte", ErrTruncated)
		}
		frame.Payload = []byte{b[0]}
		return frame, b[1:], nil

	default:
		// Unknown / not-yet-implemented type. Stash remaining bytes
		// opaquely; caller can forward to OnUnknownMessage hook or
		// drop. No further decoding here.
		frame.Payload = append([]byte(nil), b...)
		return frame, nil, nil
	}
}
