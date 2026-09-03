package matrix_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// fakeHS is a minimal homeserver double for the endpoints this transport
// touches. It exists to make the failure modes reachable in a unit test, NOT
// to be lenient: a double that accepts more than the real server cannot catch
// the class of bug where our client sends something the real server refuses.
// Where it imitates Dendrite it is pinned against Dendrite by
// TestDendrite_TokenHandling; where it is deliberately stricter, the handler
// says so.
type fakeHS struct {
	t *testing.T

	mu sync.Mutex
	// events is the append-only room log, oldest first, held as raw JSON so a
	// test can plant an event that is syntactically valid but hostile to a
	// typed decoder - the shape a real room member can produce and a strict
	// client chokes on.
	events []json.RawMessage
	// syncWindow is how many of the newest events the /sync timeline returns;
	// the rest must be reached by paginating backward from prev_batch.
	syncWindow int
	// overlap makes the first backward page repeat the newest event, the way
	// some servers do. Redelivery within one Sync must be deduplicated.
	overlap bool
	// pageSize caps how many events one /messages page returns. Servers may
	// return fewer than the requested limit, and a double that always fits
	// the whole room into one page cannot tell a client that paginates from
	// one that reads a single page and stops.
	pageSize int
	// emptyPageAt makes the page starting at this cursor come back with an
	// empty chunk AND a token that leads to more. The spec ends pagination by
	// omitting `end`, not by an empty chunk, so a client that treats the
	// empty chunk as the end loses everything older than this point.
	emptyPageAt int
	// cycleAfter makes the server re-issue an already-used token after this
	// many pages: a malformed server that a naive loop follows forever.
	cycleAfter int
	// roomMissing drops the room from /sync entirely, the way a homeserver
	// does for a room this account is not joined to.
	roomMissing bool
	// syncFails makes /sync answer 500 while /messages keeps working, which
	// is the shape a single pathological event produces on some servers.
	syncFails bool
	// encrypted serves an m.room.encryption state event for the room.
	encrypted bool

	msgCalls   int
	sentTokens []string
	srv        *httptest.Server
}

func newFakeHS(t *testing.T) *fakeHS {
	f := &fakeHS{t: t, syncWindow: 1, pageSize: 1, emptyPageAt: -1}
	mux := http.NewServeMux()
	mux.HandleFunc("/_matrix/client/versions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"versions": []string{"v1.12"}})
	})
	mux.HandleFunc("/_matrix/client/v3/sync", f.handleSync)
	mux.HandleFunc("/_matrix/client/v3/rooms/", f.handleRoom)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func matrixError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errcode": code, "error": msg})
}

func (f *fakeHS) client(t *testing.T) *mautrix.Client {
	t.Helper()
	c, err := mautrix.NewClient(f.srv.URL, id.UserID("@peer:localhost"), "token")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func (f *fakeHS) append(evt *event.Event) {
	b, err := json.Marshal(evt)
	if err != nil {
		f.t.Fatalf("marshal event: %v", err)
	}
	f.appendRaw(b)
}

// appendRaw plants arbitrary JSON in the room log. Room content is written by
// whoever is in the room, and nothing forces it through our struct.
func (f *fakeHS) appendRaw(b json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, b)
}

func (f *fakeHS) snapshot() []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]json.RawMessage, len(f.events))
	copy(out, f.events)
	return out
}

// handleSync returns the newest syncWindow events plus a prev_batch token
// pointing at everything older.
func (f *fakeHS) handleSync(w http.ResponseWriter, r *http.Request) {
	if f.syncFails {
		matrixError(w, http.StatusInternalServerError, "M_UNKNOWN", "internal server error")
		return
	}
	all := f.snapshot()
	start := len(all) - f.syncWindow
	if start < 0 {
		start = 0
	}
	window := all[start:]
	join := map[string]any{}
	if !f.roomMissing {
		timeline := map[string]any{"events": window, "limited": start > 0}
		if start > 0 {
			// Only hand out a back-token when there is actually something
			// older. A sync whose window already covers the room needs no
			// pagination, which lets a test put the merge loop under a
			// microscope with no HTTP call left in it.
			timeline["prev_batch"] = fmt.Sprintf("t%d", start)
		}
		join[string(testRoom)] = map[string]any{"timeline": timeline}
	}
	writeJSON(w, map[string]any{
		"next_batch": "s_end",
		"rooms":      map[string]any{"join": join},
	})
}

// handleRoom serves /messages (backward pagination) and /send.
func (f *fakeHS) handleRoom(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/messages"):
		f.handleMessages(w, r)
	case strings.Contains(r.URL.Path, "/send/"):
		f.handleSend(w, r)
	case strings.Contains(r.URL.Path, "/state/"):
		f.handleState(w, r)
	default:
		matrixError(w, http.StatusNotFound, "M_UNRECOGNIZED", "unknown endpoint")
	}
}

func (f *fakeHS) handleMessages(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.msgCalls++
	f.mu.Unlock()

	// Three cases, and they are NOT the same kind of rule.
	//
	// (1) NO from parameter at all: served like Dendrite, which answers 200
	// with the newest page AND a continuation token (measured 2026-09-03 and
	// asserted by TestDendrite_TokenHandling). This is the transport's
	// fallback path when /sync is unusable, so the double has to support it
	// or the fallback could never be tested.
	//
	// (2) A present but EMPTY from: this double is deliberately STRICTER than
	// Dendrite, which treats it as absent. Refusing it here keeps our own
	// tests loud if the main pagination path ever starts sending one, because
	// relying on a server to interpret an empty token is not portable. House
	// rule, not imitation.
	//
	// (3) A malformed token: mirrors Dendrite exactly - 400 M_INVALID_PARAM
	// "Invalid from parameter: malformed sync token", pinned against the real
	// server so this branch cannot drift into being more permissive than
	// production.
	vals, present := r.URL.Query()["from"]
	from := ""
	if present {
		from = vals[0]
	}
	all0 := f.snapshot()
	cursor := len(all0)
	switch {
	case !present:
		// newest page: cursor stays at the end of the log
	case from == "":
		matrixError(w, http.StatusBadRequest, "M_INVALID_PARAM", "empty from-token (stricter than Dendrite on purpose)")
		return
	default:
		if _, err := fmt.Sscanf(from, "t%d", &cursor); err != nil {
			matrixError(w, http.StatusBadRequest, "M_INVALID_PARAM", "Invalid from parameter: malformed sync token")
			return
		}
	}
	if d := r.URL.Query().Get("dir"); d != "b" {
		// Real servers require dir, and this transport only ever reads
		// backward. A double that ignores it cannot tell a client that pages
		// backward from one that asks for the wrong direction entirely.
		matrixError(w, http.StatusBadRequest, "M_MISSING_PARAM", "dir must be b for this double, got "+d)
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &limit); err != nil {
			matrixError(w, http.StatusBadRequest, "M_INVALID_PARAM", "bad limit")
			return
		}
	}

	all := f.snapshot()
	if cursor > len(all) {
		cursor = len(all)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// A server that keeps re-issuing a token it has already handed out. A
	// loop that only compares against its immediately previous token follows
	// this forever.
	if f.cycleAfter > 0 && len(f.sentTokens) >= f.cycleAfter {
		reissue := f.sentTokens[0]
		f.sentTokens = append(f.sentTokens, reissue)
		writeJSON(w, map[string]any{"start": from, "chunk": []json.RawMessage{}, "end": reissue})
		return
	}

	// An empty chunk that is NOT the end of history. The token differs from
	// the one just used (so it is not a loop) but addresses the same cursor,
	// so the following page returns the events a client would have lost by
	// stopping here.
	if cursor == f.emptyPageAt && cursor > 0 {
		f.emptyPageAt = -1
		next := fmt.Sprintf("t%db", cursor)
		f.sentTokens = append(f.sentTokens, next)
		writeJSON(w, map[string]any{"start": from, "chunk": []json.RawMessage{}, "end": next})
		return
	}

	// A page is allowed to be shorter than the requested limit, and this one
	// always is: a double that fits the whole room into a single page cannot
	// tell a client that paginates from one that reads one page and stops.
	size := f.pageSize
	if size > limit {
		size = limit
	}
	if size < 1 {
		size = 1
	}
	lo := cursor - size
	if lo < 0 {
		lo = 0
	}
	chunk := make([]json.RawMessage, 0, size+1)
	for i := cursor - 1; i >= lo; i-- {
		chunk = append(chunk, all[i])
	}
	if f.overlap && cursor < len(all) {
		// Repeat the oldest event the /sync window already delivered.
		chunk = append([]json.RawMessage{all[cursor]}, chunk...)
	}

	body := map[string]any{"start": from, "chunk": chunk}
	if lo > 0 {
		next := fmt.Sprintf("t%d", lo)
		f.sentTokens = append(f.sentTokens, next)
		body["end"] = next
	}
	// No `end` key at all once lo == 0: that, and only that, ends history
	// per the spec.
	writeJSON(w, body)
}

// handleState answers the one state lookup the transport makes. 404 with
// M_NOT_FOUND for an unencrypted room is what Dendrite returns (measured
// 2026-09-03); the transport must read that as "not encrypted" rather than as
// a failure.
func (f *fakeHS) handleState(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.Path, "m.room.encryption") {
		matrixError(w, http.StatusNotFound, "M_NOT_FOUND", "no such state event")
		return
	}
	if !f.encrypted {
		matrixError(w, http.StatusNotFound, "M_NOT_FOUND", "Room does not have encryption enabled.")
		return
	}
	writeJSON(w, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"})
}

func (f *fakeHS) handleSend(w http.ResponseWriter, r *http.Request) {
	var content map[string]any
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		matrixError(w, http.StatusBadRequest, "M_NOT_JSON", "bad body")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	evtType := parts[len(parts)-2]
	f.mu.Lock()
	eid := id.EventID(fmt.Sprintf("$evt%d", len(f.events)))
	f.mu.Unlock()
	b, err := json.Marshal(&event.Event{
		ID:      eid,
		Type:    event.Type{Type: evtType, Class: event.MessageEventType},
		RoomID:  testRoom,
		Content: event.Content{Raw: content},
	})
	if err != nil {
		matrixError(w, http.StatusInternalServerError, "M_UNKNOWN", "marshal")
		return
	}
	f.appendRaw(b)
	writeJSON(w, map[string]any{"event_id": eid})
}
