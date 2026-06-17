package gomobile

import (
	"encoding/json"
	"strconv"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/awareness"
)

// PresenceListener receives the room's presence (awareness) snapshot
// whenever any peer's ephemeral state changes — a cursor moves, a user
// joins, or a user leaves. statesJSON is a JSON object mapping each
// online peer's clientID (as a decimal string) to that peer's raw
// state JSON:
//
//	{"42":{"name":"ian","cursor":"BASE64..."},"77":{"name":"sam"}}
//
// Render every collaborator's cursor and identity from it. Your own
// entry is included; filter it with Client.ClientID() if you only want
// peers. Called from a background goroutine; dispatch to the main
// thread before touching UI and do not call back into the client
// synchronously.
type PresenceListener interface {
	OnPresenceChange(statesJSON []byte)
}

// ObservePresence registers a listener fired whenever the room's
// presence changes — a peer's cursor or identity updates, or a peer
// joins or leaves. It delivers the full current snapshot each time,
// ready to render. Replaces any previously registered presence
// listener. May be called before or after Connect; pass nil to stop
// observing.
//
// Publish your own presence with SetAwarenessState, typically a small
// JSON object carrying your name and an encoded cursor:
//
//	cur, _ := text.EncodeCursor(index, 0)
//	state, _ := json.Marshal(map[string]any{
//	    "name":   "ian",
//	    "cursor": base64.StdEncoding.EncodeToString(cur),
//	})
//	client.SetAwarenessState(state)
//
// A peer reading that entry decodes the cursor bytes and calls
// text.ResolveCursor to map it to its own local index.
func (c *Client) ObservePresence(l PresenceListener) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelPresence != nil {
		c.cancelPresence()
		c.cancelPresence = nil
	}
	c.presence = l
	if l != nil && c.inner != nil {
		c.cancelPresence = c.startPresence(c.inner, l)
	}
}

// PresenceStates returns the current presence snapshot — the same JSON
// shape ObservePresence delivers — or "{}" when not connected. Use it
// for the initial render before the first change arrives.
func (c *Client) PresenceStates() []byte {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	if inner == nil {
		return []byte("{}")
	}
	return presenceSnapshot(inner.Awareness())
}

// ClientID returns this client's awareness/document clientID, the key
// of its own entry in the presence snapshot. Use it to skip yourself
// when rendering peer cursors.
func (c *Client) ClientID() uint64 { return c.d.inner.ClientID() }

// startPresence subscribes pl to inner's awareness change feed and
// returns the unsubscribe func. The caller holds c.mu.
func (c *Client) startPresence(inner *client.Client, pl PresenceListener) func() {
	aw := inner.Awareness()
	return aw.OnChange(func(awareness.Summary, any) {
		pl.OnPresenceChange(presenceSnapshot(aw))
	})
}

// presenceSnapshot renders the awareness state map as a JSON object
// keyed by clientID string, each value the peer's raw state JSON.
// Only online peers appear (a removed peer drops out, so the listener
// sees it leave).
func presenceSnapshot(aw *awareness.Awareness) []byte {
	states := aw.States()
	out := make(map[string]json.RawMessage, len(states))
	for id, st := range states {
		out[strconv.FormatUint(id, 10)] = json.RawMessage(st)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return []byte("{}")
	}
	return b
}
