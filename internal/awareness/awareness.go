// Package awareness implements the y-protocols Awareness layer —
// the ephemeral, presence-style sibling of the document CRDT used
// to track per-client transient state like cursor positions, user
// names, and selection ranges.
//
// Awareness is NOT part of YATA. It is a last-write-wins map keyed
// by clientID, with a per-client monotonic clock and a wall-clock
// LastUpdated timestamp used for timeout sweeps. Wire-format
// compatibility is against y-protocols (the package every JS Yjs
// client speaks); yrs is mirrored where its design diverges from
// JS in ways that affect Go translation choices. See
// docs/yrs-port-notes/awareness.md for the full reference.
//
// State payloads on the wire are raw JSON strings, NOT lib0 Any
// TLV. Awareness stores them as opaque []byte and only invokes
// encoding/json at the typed-helper boundary
// (SetLocalStateJSON / DecodeStateJSON). This matches yrs's
// Arc<str> discipline and avoids unnecessary marshal/unmarshal
// round-trips inside Apply.
//
// Awareness is independent of doc.Doc. The local clientID is
// passed at construction time; no doc.Doc reference is required.
// Embedders that already have a Doc typically use d.ClientID() to
// keep the two layers in sync.
package awareness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Deln0r/ygo/internal/utf16"
)

// NullStateJSON is the literal four-byte JSON string "null" that
// y-protocols and yrs emit when a client is marked offline. The
// removal sentinel is JSON-null at the wire level — NOT the lib0
// Any null tag byte. Per docs/yrs-port-notes/awareness.md gotcha 1.
const NullStateJSON = "null"

// DefaultTimeout is the y-protocols convention for stale-entry
// eviction (30 seconds, per y-protocols/awareness.js:13 outdatedTimeout).
// It is a convention, not a hard requirement; callers pick their
// own threshold when calling SweepOutdated.
const DefaultTimeout = 30 * time.Second

// ClientState is the per-client awareness entry. Stored in the
// Awareness state map; exposed only via accessor methods so the
// internal mutex protects mutation.
//
// Data is nil for removed clients; the entry is retained so the
// Clock survives — without it a stale "I'm back" message at a
// lower clock could resurrect a removed client.
type ClientState struct {
	Clock       uint32
	LastUpdated time.Time
	Data        []byte
}

// Summary is the event payload describing which clients were
// affected by a single Apply call or local mutation. Field
// semantics match y-protocols/awareness.js:
//
//   - Added: clientIDs that were not previously known.
//   - Updated: clientIDs whose entry was applied (clock bumped),
//     regardless of whether content changed.
//   - Removed: clientIDs marked offline by this batch.
//
// OnChange subscribers receive a Summary where Updated is filtered
// to the subset whose Data actually differs from the previous
// value — heartbeats (re-broadcast of unchanged state) are filtered
// out. OnUpdate subscribers receive the full Summary.
type Summary struct {
	Added, Updated, Removed []uint64
}

// IsEmpty reports whether the summary touches zero clients. Used
// to skip no-op callback dispatches.
func (s Summary) IsEmpty() bool {
	return len(s.Added) == 0 && len(s.Updated) == 0 && len(s.Removed) == 0
}

// Sub is the subscriber callback shape registered via OnUpdate or
// OnChange. origin is the caller-supplied tag forwarded through
// Apply (mirrors doc.TransactionMut.Origin); use it to distinguish
// local vs remote-applied changes.
//
// Subscribers MUST NOT call Awareness mutator methods from inside
// the callback — the surrounding lock has already been released,
// but a re-entrant Set/Remove could trigger a chain of nested
// events that complicates reasoning. If a callback needs to mutate
// state, dispatch the work to another goroutine.
type Sub func(summary Summary, origin any)

// Awareness tracks per-client ephemeral state. Safe for concurrent
// use; an internal RWMutex serializes mutation and a separate
// mutex protects the subscriber list.
type Awareness struct {
	clientID uint64
	now      func() time.Time

	mu     sync.RWMutex
	states map[uint64]*ClientState
	// maxClients bounds the number of distinct tracked clientIDs
	// (live entries plus retained tombstones); 0 means unlimited.
	// Guarded by mu. See SetMaxClients.
	maxClients int
	// maxForeignClients sub-bounds admissions made via ApplyForeign
	// (presence relayed from another instance over a cluster backplane):
	// a brand-new foreign clientID is admitted only while the tracked
	// set holds fewer than maxForeignClients entries, so foreign
	// presence can occupy at most maxForeignClients slots and can never
	// consume the maxClients-maxForeignClients slots reserved for local
	// clients. 0 means no sub-cap (foreign shares the full maxClients
	// bound with local, the historical behavior). Guarded by mu. See
	// SetMaxForeignClients.
	maxForeignClients int

	// live holds the clients with a state in the order their state
	// appeared, as yjs's awareness.states Map keeps them: the local
	// client when it first sets a state, others as their states arrive;
	// a removed client leaves it and comes back at the end. dead holds
	// the tombstones (removed clients whose clock is kept) in the order
	// they became tombstones. Guarded by mu.
	live []uint64
	dead []uint64

	subMu       sync.Mutex
	nextSubID   atomic.Uint64
	onUpdateSub []subscription
	onChangeSub []subscription
}

// subscription is one OnUpdate / OnChange registration; the slices keep
// registration order, which lib0's Observable keeps for y-protocols.
type subscription struct {
	id uint64
	fn Sub
}

// New returns an Awareness instance for the given local clientID
// using time.Now as the timestamp source.
func New(clientID uint64) *Awareness {
	return NewWithClock(clientID, time.Now)
}

// NewWithClock returns an Awareness instance with a caller-supplied
// time source. Used by tests that need deterministic LastUpdated
// values; pass time.Now in production.
func NewWithClock(clientID uint64, now func() time.Time) *Awareness {
	return &Awareness{
		clientID: clientID,
		now:      now,
		states:   map[uint64]*ClientState{},
	}
}

// ClientID returns the local client's identifier passed to New.
func (a *Awareness) ClientID() uint64 { return a.clientID }

// SetMaxClients bounds the number of distinct clientIDs this
// Awareness tracks — live entries plus retained tombstones. A
// non-positive value means unlimited (the default). When the cap is
// reached, Apply silently drops brand-new clientIDs from incoming
// updates; already-tracked clients keep updating, and the local
// client is always admitted. Servers set this per room to bound the
// memory one peer can force by flooding fabricated clientIDs.
//
// Lowering the cap below the current entry count evicts nothing; it
// only refuses further additions until SweepOutdated / PurgeTombstones
// bring the count back under the limit.
func (a *Awareness) SetMaxClients(n int) {
	if n < 0 {
		n = 0
	}
	a.mu.Lock()
	a.maxClients = n
	a.mu.Unlock()
}

// SetMaxForeignClients bounds how many tracked clientIDs an ApplyForeign
// admission may fill, reserving the rest of the maxClients budget for local
// clients. While the tracked set already holds at least n entries, ApplyForeign
// refuses brand-new clientIDs (already-tracked foreign clients keep updating);
// plain Apply is unaffected and keeps admitting up to maxClients. A non-positive
// value disables the sub-cap, so foreign presence again shares the full
// maxClients bound with local clients.
//
// This partitions the per-room presence cap by origin: because foreign
// admissions stop once the tracked set reaches n, at most n slots are ever held
// by relayed presence, so a sustained presence flood arriving over the backplane
// cannot starve new local clients of a slot. Servers set n below MaxClients per
// room; the local floor is MaxClients-n.
func (a *Awareness) SetMaxForeignClients(n int) {
	if n < 0 {
		n = 0
	}
	a.mu.Lock()
	a.maxForeignClients = n
	a.mu.Unlock()
}

// LocalState returns a copy of the local client's current state
// JSON bytes. The second return reports whether a local state is
// set; false means the client either never set a state or was
// removed via RemoveLocalState.
func (a *Awareness) LocalState() ([]byte, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.states[a.clientID]
	if !ok || s.Data == nil {
		return nil, false
	}
	out := make([]byte, len(s.Data))
	copy(out, s.Data)
	return out, true
}

// SetLocalState sets the local client's state to the given raw
// JSON bytes. The clock bumps unconditionally — even no-op equal
// sets — per the 15-second heartbeat invariant
// (y-protocols/awareness.js:106-140). The deep-equality check
// affects only whether OnChange fires; OnUpdate always fires.
//
// A nil jsonState argument is equivalent to RemoveLocalState. To
// set a JSON-null state explicitly, pass []byte("null") — but note
// that this triggers Removal semantics on the wire and is
// indistinguishable from removal for receiving peers.
func (a *Awareness) SetLocalState(jsonState []byte) {
	if jsonState == nil {
		a.RemoveLocalState()
		return
	}

	a.mu.Lock()
	prev, existed := a.states[a.clientID]
	var prevData []byte
	if existed {
		prevData = prev.Data
	}
	now := a.now()
	// y-protocols starts every awareness at an implicit {} with clock 0,
	// so its first explicit state goes out with clock 1, and a receiver
	// that has not seen the client takes only a clock above 0. Starting
	// here at 0 made the first state of a Go client invisible to
	// JavaScript peers until the next one. There is no implicit state:
	// a passive server room stays empty.
	newClock := uint32(1)
	if existed {
		newClock = prev.Clock + 1
	}
	// y-protocols reads the state with lib0's fatal UTF-8 decoder: each
	// invalid byte becomes U+FFFD here, once, as the state is set.
	stateCopy := []byte(utf16.ValidString(string(jsonState)))
	a.states[a.clientID] = &ClientState{
		Clock:       newClock,
		LastUpdated: now,
		Data:        stateCopy,
	}
	if !existed || prevData == nil {
		a.markLive(a.clientID)
	}
	a.mu.Unlock()

	summary := Summary{}
	if !existed || prevData == nil {
		summary.Added = []uint64{a.clientID}
	} else {
		summary.Updated = []uint64{a.clientID}
	}

	// 'change' before 'update', as y-protocols emits them.
	contentChanged := !existed || prevData == nil || !sameState(prevData, stateCopy)
	if contentChanged {
		a.fireChange(summary, originLocal)
	}
	a.fireUpdate(summary, originLocal)
}

// RenewLocalState re-announces the local state when it was last set or
// renewed at least olderThan ago: the clock bumps and OnUpdate fires, as
// y-protocols' check does when it sets the same state again (OnChange does
// not fire, the state is equal). It reports whether it renewed; a removed
// or never-set state is left alone. The check and the bump are one step,
// so a RemoveLocalState racing it is never undone.
func (a *Awareness) RenewLocalState(olderThan time.Duration) bool {
	a.mu.Lock()
	s, ok := a.states[a.clientID]
	now := a.now()
	if !ok || s.Data == nil || now.Sub(s.LastUpdated) < olderThan {
		a.mu.Unlock()
		return false
	}
	s.Clock++
	s.LastUpdated = now
	a.mu.Unlock()
	a.fireUpdate(Summary{Updated: []uint64{a.clientID}}, originLocal)
	return true
}

// SetLocalStateJSON marshals v with encoding/json and forwards to
// SetLocalState. Returns the marshal error if any; the awareness
// state is unchanged on error. Pass nil to remove the local state.
func (a *Awareness) SetLocalStateJSON(v any) error {
	if v == nil {
		a.RemoveLocalState()
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("awareness: marshal local state: %w", err)
	}
	a.SetLocalState(raw)
	return nil
}

// RemoveLocalState marks the local client offline, as y-protocols
// setLocalState(null) does: the clock bumps, the wire carries the entry
// with JSON "null", and the local entry is kept (with Data = nil) so the
// clock survives later revival races. OnChange and OnUpdate report the
// local client removed, also when it was removed already or never set
// a state: y-protocols starts every awareness at an implicit {} with
// clock 0, so its first removal goes out at clock 1, which is what
// clears a state a peer still holds for this client ID.
func (a *Awareness) RemoveLocalState() {
	a.mu.Lock()
	prev, existed := a.states[a.clientID]
	if existed {
		prev.Clock++
		prev.LastUpdated = a.now()
		prev.Data = nil
	} else {
		a.states[a.clientID] = &ClientState{Clock: 1, LastUpdated: a.now()}
	}
	a.markDead(a.clientID)
	a.mu.Unlock()

	summary := Summary{Removed: []uint64{a.clientID}}
	a.fireChange(summary, originLocal)
	a.fireUpdate(summary, originLocal)
}

// States returns a snapshot of all currently-online client states
// as a deep-copy map. Removed/offline entries (Data nil but Clock
// retained) are excluded.
//
// Modifying the returned map or its values does not affect the
// Awareness state.
func (a *Awareness) States() map[uint64][]byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[uint64][]byte, len(a.states))
	for id, s := range a.states {
		if s.Data == nil {
			continue
		}
		c := make([]byte, len(s.Data))
		copy(c, s.Data)
		out[id] = c
	}
	return out
}

// ClientIDs returns the clients that have a state, in the order their
// states appeared (yjs awareness.getStates() order): a removed client
// is not in it, and one that comes back is at the end.
func (a *Awareness) ClientIDs() []uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]uint64(nil), a.live...)
}

// EncodeLive encodes every client that has a state, in ClientIDs order,
// as y-protocols callers encode "everyone" with
// encodeAwarenessUpdate(awareness, [...getStates().keys()]). With no such
// client the update has no entries; Encode(nil) would send the
// tombstones instead.
func (a *Awareness) EncodeLive() []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	// The IDs and their states under one lock: taking the IDs, unlocking
	// and encoding them later could send a client removed in between, or
	// an order that never existed.
	return a.encodeLocked(a.live)
}

// Meta returns the per-client metadata. The third return is false
// when clientID has never been seen.
func (a *Awareness) Meta(clientID uint64) (clock uint32, lastUpdated time.Time, ok bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, present := a.states[clientID]
	if !present {
		return 0, time.Time{}, false
	}
	return s.Clock, s.LastUpdated, true
}

// RemoveState marks an arbitrary clientID offline. Used by
// embedders to evict peers after a disconnect notification or
// manual administrative action. Local client cannot be removed via
// this method — use RemoveLocalState. Removing a never-seen
// clientID is a no-op.
func (a *Awareness) RemoveState(clientID uint64) {
	if clientID == a.clientID {
		a.RemoveLocalState()
		return
	}
	a.mu.Lock()
	prev, existed := a.states[clientID]
	if !existed || prev.Data == nil {
		a.mu.Unlock()
		return
	}
	// The clock stays: y-protocols removeAwarenessStates raises only the
	// local client's. The removal goes out at the client's last clock,
	// which receivers holding its state accept, and the client's next
	// state, one clock up, is taken again. Raising it here made the
	// client's next state look stale and dropped it.
	prev.LastUpdated = a.now()
	prev.Data = nil
	a.markDead(clientID)
	a.mu.Unlock()

	summary := Summary{Removed: []uint64{clientID}}
	a.fireChange(summary, originLocal)
	a.fireUpdate(summary, originLocal)
}

// SweepOutdated removes every non-local client whose LastUpdated
// is older than (now - threshold). Returns the sorted-ascending
// IDs of removed clients. The local client is never swept — its
// heartbeat is the caller's responsibility.
//
// Fires OnUpdate and OnChange with origin = "timeout".
func (a *Awareness) SweepOutdated(threshold time.Duration) []uint64 {
	cutoff := a.now().Add(-threshold)

	a.mu.Lock()
	var removed []uint64
	for id, s := range a.states {
		if id == a.clientID {
			continue
		}
		if s.Data == nil {
			continue
		}
		if s.LastUpdated.Before(cutoff) {
			// The clock stays, as in RemoveState.
			s.LastUpdated = a.now()
			s.Data = nil
			removed = append(removed, id)
		}
	}
	sortAscending(removed)
	for _, id := range removed {
		a.markDead(id)
	}
	a.mu.Unlock()

	if len(removed) == 0 {
		return nil
	}
	summary := Summary{Removed: removed}
	a.fireChange(summary, originTimeout)
	a.fireUpdate(summary, originTimeout)
	return removed
}

// ForgetRemote drops every client but the local one, as y-websocket does
// when its connection closes, and forgets their clocks too, so the states
// the next connection brings are taken as new: a peer still online comes
// back at the clock it had, which a kept tombstone would refuse until its
// next renewal. OnChange and OnUpdate report the clients that had a
// state as removed. It returns them.
func (a *Awareness) ForgetRemote() []uint64 {
	a.mu.Lock()
	var removed []uint64
	for _, id := range a.live {
		if id != a.clientID {
			removed = append(removed, id)
		}
	}
	for id := range a.states {
		if id != a.clientID {
			delete(a.states, id)
			a.forget(id)
		}
	}
	a.mu.Unlock()
	if len(removed) == 0 {
		return nil
	}
	summary := Summary{Removed: removed}
	a.fireChange(summary, originLocal)
	a.fireUpdate(summary, originLocal)
	return removed
}

// PurgeTombstones permanently deletes every non-local entry that is
// already a tombstone (Data nil) and whose LastUpdated is older than
// (now - threshold), reclaiming the map slot. Returns the count
// purged. Fires no events — the clients were already removed. The
// local client is never purged.
//
// SweepOutdated marks silent clients offline but retains their key so
// a stale low-clock revival cannot resurrect them; over a long-lived
// room those tombstones accumulate without bound. PurgeTombstones is
// the second-stage GC: once an entry has sat tombstoned past
// threshold, a lower-clock revival is no longer a realistic concern
// and the slot is freed. Pick a threshold comfortably larger than the
// SweepOutdated timeout so live evictions settle before the key goes.
func (a *Awareness) PurgeTombstones(threshold time.Duration) int {
	cutoff := a.now().Add(-threshold)

	a.mu.Lock()
	defer a.mu.Unlock()
	purged := 0
	for id, s := range a.states {
		if id == a.clientID {
			continue
		}
		if s.Data != nil {
			continue
		}
		if s.LastUpdated.Before(cutoff) {
			delete(a.states, id)
			a.forget(id)
			purged++
		}
	}
	return purged
}

// Encode produces V1 awareness wire bytes for the given clientIDs.
// Pass nil or an empty slice to encode every known client (online
// or removed-but-clock-retained). Unknown IDs are silently skipped.
//
// Iteration order is determined by the caller — pass a sorted
// slice for deterministic output. The wire format itself does not
// canonicalize order; round-trip tests should compare structurally.
func (a *Awareness) Encode(clientIDs []uint64) []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var ids []uint64
	if len(clientIDs) == 0 {
		// Live clients in the order their states appeared, then the
		// tombstones in the order they were removed: the first half is
		// yjs's getStates() order, the second is this package's own.
		ids = make([]uint64, 0, len(a.live)+len(a.dead))
		ids = append(append(ids, a.live...), a.dead...)
	} else {
		ids = clientIDs
	}

	return a.encodeLocked(ids)
}

// encodeLocked encodes the clients ids in that order, skipping unknown
// ones. Caller holds mu.
func (a *Awareness) encodeLocked(ids []uint64) []byte {
	entries := make([]wireEntry, 0, len(ids))
	for _, id := range ids {
		s, ok := a.states[id]
		if !ok {
			continue
		}
		var payload []byte
		if s.Data == nil {
			payload = []byte(NullStateJSON)
		} else {
			payload = s.Data
		}
		entries = append(entries, wireEntry{
			ClientID: id,
			Clock:    s.Clock,
			JSON:     payload,
		})
	}
	return encodeUpdate(entries)
}

// ErrInvalidUpdate wraps decode failures of an incoming wire blob.
// The Awareness state is unchanged when Apply returns this error.
var ErrInvalidUpdate = errors.New("awareness: invalid update bytes")

// Apply integrates a wire-encoded update into the local state.
// origin is forwarded to OnUpdate / OnChange subscribers — typical
// values are "remote", "timeout", a peer ID, or nil.
//
// Returns the Summary describing every applied entry. On decode
// error the state is unchanged and the returned Summary is empty.
//
// Apply implements the y-protocols LWW algorithm exactly per
// docs/yrs-port-notes/awareness.md §4:
//
//   - Accept iff currClock < incomingClock, OR
//     currClock == incomingClock AND incoming is null AND current is non-null
//     (tied-clock removal wins).
//   - Self-eviction defense: if a remote tries to remove the local
//     client while the local state is still non-null, bump the
//     local clock instead of removing.
//   - Removal preserves the entry locally with Data = nil so the
//     clock survives stale revivals.
//
// Brand-new clientIDs are admitted up to the full MaxClients bound. Use
// ApplyForeign for presence relayed from another instance to hold it to the
// foreign sub-cap instead.
func (a *Awareness) Apply(update []byte, origin any) (Summary, error) {
	return a.apply(update, origin, false)
}

// ApplyForeign is Apply for presence relayed from another instance over a
// cluster backplane. It behaves identically except that a brand-new clientID is
// held to the foreign sub-cap (SetMaxForeignClients) rather than the full
// MaxClients bound, so relayed presence can never consume the slots reserved for
// local clients. Already-tracked clientIDs keep updating regardless of origin.
func (a *Awareness) ApplyForeign(update []byte, origin any) (Summary, error) {
	return a.apply(update, origin, true)
}

// apply integrates a wire-encoded update. foreign selects the presence cap that
// gates brand-new clientIDs: the foreign sub-cap when true, the full MaxClients
// bound otherwise. See Apply and ApplyForeign.
func (a *Awareness) apply(update []byte, origin any, foreign bool) (Summary, error) {
	entries, _, err := decodeUpdate(update)
	if err != nil {
		return Summary{}, fmt.Errorf("%w: %w", ErrInvalidUpdate, err)
	}

	a.mu.Lock()
	// The classes y-protocols applyAwarenessUpdate reports: added for a
	// client never seen; removed for a null from a client seen before,
	// also one already removed; updated for a state from a client seen
	// before, a revival included, and changed (the 'change' event's
	// updated) for those whose state differs.
	var added, updated, removed, changed []uint64
	for _, e := range entries {
		incomingIsNull := bytes.Equal(e.JSON, []byte(NullStateJSON))

		prev, existed := a.states[e.ClientID]
		var prevData []byte
		var prevClock uint32
		if existed {
			prevData = prev.Data
			prevClock = prev.Clock
		}
		prevIsNull := !existed || prevData == nil

		accept := false
		if !existed {
			accept = true
		} else if prevClock < e.Clock {
			accept = true
		} else if prevClock == e.Clock && incomingIsNull && !prevIsNull {
			accept = true
		}
		if !accept {
			continue
		}

		// Self-eviction defense: a remote removing our local entry
		// while we are still online bumps our clock instead, so
		// the next heartbeat supersedes the stale removal.
		if e.ClientID == a.clientID && incomingIsNull && !prevIsNull {
			prev.Clock = e.Clock + 1
			prev.LastUpdated = a.now()
			// Reported removed, as y-protocols does, though the state is
			// kept: the next heartbeat carries it at the new clock.
			removed = append(removed, e.ClientID)
			continue
		}

		// Presence cap: refuse brand-new clientIDs once the tracked
		// set (live plus tombstoned) is full. The local client is
		// exempt. Dropping the entry rather than erroring keeps a
		// flood of fabricated IDs from voiding legitimate entries
		// batched alongside it. A foreign apply (relayed presence) is
		// held to the tighter foreign sub-cap so it cannot fill the
		// slots reserved for local clients; foreign admissions stop at
		// maxForeignClients while local ones continue to maxClients.
		if !existed && e.ClientID != a.clientID {
			limit := a.maxClients
			if foreign && a.maxForeignClients > 0 && (limit <= 0 || a.maxForeignClients < limit) {
				limit = a.maxForeignClients
			}
			if limit > 0 && len(a.states) >= limit {
				continue
			}
		}

		now := a.now()
		if !existed {
			a.states[e.ClientID] = &ClientState{
				Clock:       e.Clock,
				LastUpdated: now,
				Data:        nil,
			}
			prev = a.states[e.ClientID]
		} else {
			prev.Clock = e.Clock
			prev.LastUpdated = now
		}

		if incomingIsNull {
			prev.Data = nil
			if existed {
				removed = append(removed, e.ClientID)
			}
			// A tombstone, seen before or not.
			a.markDead(e.ClientID)
			continue
		}

		dataCopy := make([]byte, len(e.JSON))
		copy(dataCopy, e.JSON)
		prev.Data = dataCopy
		if prevIsNull {
			a.markLive(e.ClientID)
		}

		if !existed {
			added = append(added, e.ClientID)
		} else {
			updated = append(updated, e.ClientID)
			if prevIsNull || !sameState(prevData, e.JSON) {
				changed = append(changed, e.ClientID)
			}
		}
	}
	a.mu.Unlock()

	summary := Summary{
		Added:   added,
		Updated: updated,
		Removed: removed,
	}
	// 'change' before 'update', as y-protocols emits them.
	if len(added) > 0 || len(removed) > 0 || len(changed) > 0 {
		changeSummary := Summary{
			Added:   added,
			Updated: changed,
			Removed: removed,
		}
		a.fireChange(changeSummary, origin)
	}
	a.fireUpdate(summary, origin)

	return summary, nil
}

// OnUpdate registers a callback fired for every applied entry,
// including no-op heartbeat re-broadcasts. Returns an unsubscribe
// closure that removes the callback.
//
// Use OnUpdate for traffic accounting or debug logging. For UI
// redraws prefer OnChange — heartbeats are noise.
func (a *Awareness) OnUpdate(f Sub) (unsub func()) {
	return a.subscribe(&a.onUpdateSub, f)
}

// OnChange registers a callback fired only when the JSON state
// content actually differs from the previous value, or on
// add/remove. Returns an unsubscribe closure.
func (a *Awareness) OnChange(f Sub) (unsub func()) {
	return a.subscribe(&a.onChangeSub, f)
}

const (
	originLocal   = "local"
	originTimeout = "timeout"
)

// subscribe adds f to list in registration order and returns its
// removal.
func (a *Awareness) subscribe(list *[]subscription, f Sub) (unsub func()) {
	id := a.nextSubID.Add(1)
	a.subMu.Lock()
	*list = append(*list, subscription{id: id, fn: f})
	a.subMu.Unlock()
	return func() {
		a.subMu.Lock()
		defer a.subMu.Unlock()
		for i, s := range *list {
			if s.id == id {
				*list = append((*list)[:i:i], (*list)[i+1:]...)
				return
			}
		}
	}
}

func (a *Awareness) fireUpdate(s Summary, origin any) {
	a.fire(&a.onUpdateSub, s, origin)
}

func (a *Awareness) fireChange(s Summary, origin any) {
	a.fire(&a.onChangeSub, s, origin)
}

// fire calls a copy of list taken now, in registration order, as lib0's
// Observable.emit copies its listeners before calling them.
func (a *Awareness) fire(list *[]subscription, s Summary, origin any) {
	if s.IsEmpty() {
		return
	}
	a.subMu.Lock()
	subs := append([]subscription(nil), (*list)...)
	a.subMu.Unlock()
	for _, sub := range subs {
		sub.fn(s, origin)
	}
}

// markLive records that clientID has a state now (yjs states.set): it
// leaves the tombstones and, if it was not live, joins the end of the
// live order. Caller holds mu.
func (a *Awareness) markLive(clientID uint64) {
	a.dead = removeID(a.dead, clientID)
	if !containsID(a.live, clientID) {
		a.live = append(a.live, clientID)
	}
}

// markDead records that clientID is a tombstone now (yjs
// states.delete, its meta kept). Caller holds mu.
func (a *Awareness) markDead(clientID uint64) {
	a.live = removeID(a.live, clientID)
	if !containsID(a.dead, clientID) {
		a.dead = append(a.dead, clientID)
	}
}

// forget drops clientID from both orders (PurgeTombstones). Caller holds
// mu.
func (a *Awareness) forget(clientID uint64) {
	a.live = removeID(a.live, clientID)
	a.dead = removeID(a.dead, clientID)
}

// sameState reports whether two state payloads hold the same JSON value,
// as y-protocols decides with lib0 equalityDeep on the parsed states: key
// order and number spelling (1, 1.0) do not count. Payloads that do not
// parse compare as bytes.
func sameState(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func containsID(ids []uint64, id uint64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func removeID(ids []uint64, id uint64) []uint64 {
	for i, x := range ids {
		if x == id {
			return append(ids[:i:i], ids[i+1:]...)
		}
	}
	return ids
}

// sortAscending is a tiny insertion-sort for the small slices
// returned by SweepOutdated. Avoids a sort.Slice closure-alloc.
func sortAscending(ids []uint64) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
}
