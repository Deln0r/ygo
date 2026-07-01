package sync_test

import (
	"bytes"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
)

// memTransport collects Sent and Broadcast envelopes in memory for
// assertion. Safe for the test's main goroutine plus any callbacks.
type memTransport struct {
	mu        sync.Mutex
	Sent      [][]byte
	Broadcast [][]byte
}

func (m *memTransport) send(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sent = append(m.Sent, append([]byte(nil), b...))
	return nil
}

func (m *memTransport) broadcast(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Broadcast = append(m.Broadcast, append([]byte(nil), b...))
}

func newTestConn(t *testing.T, d *doc.Doc, aw *awareness.Awareness, id string) (*syncpkg.Conn, *memTransport) {
	t.Helper()
	tr := &memTransport{}
	c := syncpkg.New(d, aw, id)
	c.Send = tr.send
	c.Broadcast = tr.broadcast
	return c, tr
}

func decodeOrFail(t *testing.T, b []byte) *syncpkg.Frame {
	t.Helper()
	f, _, err := syncpkg.DecodeEnvelope(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return f
}

func TestHandler_ReadOnly_DropsInboundUpdate(t *testing.T) {
	// An update that adds one item to "items".
	src := doc.NewDocWithOptions(doc.Options{ClientID: 7})
	sa := types.NewArray(src.Branch("items"))
	stxn := src.WriteTxn()
	sa.Push(stxn, "edit")
	stxn.Commit()
	update := encoding.EncodeStateAsUpdate(src)

	// Read-only connection: the update is neither applied nor broadcast.
	server := doc.NewDocWithOptions(doc.Options{ClientID: 100})
	conn, tr := newTestConn(t, server, awareness.New(100), "viewer")
	conn.ReadOnly = true
	if err := conn.HandleFrame(decodeOrFail(t, syncpkg.EncodeSyncUpdate(update))); err != nil {
		t.Fatalf("HandleFrame (read-only): %v", err)
	}
	if got := types.NewArray(server.Branch("items")).Len(); got != 0 {
		t.Errorf("read-only doc len = %d, want 0 (update must be dropped)", got)
	}
	if len(tr.Broadcast) != 0 {
		t.Errorf("read-only broadcast count = %d, want 0", len(tr.Broadcast))
	}

	// Control: a read-write connection applies and broadcasts the same
	// update, confirming the drop above is the read-only flag's doing.
	rw := doc.NewDocWithOptions(doc.Options{ClientID: 101})
	conn2, tr2 := newTestConn(t, rw, awareness.New(101), "editor")
	if err := conn2.HandleFrame(decodeOrFail(t, syncpkg.EncodeSyncUpdate(update))); err != nil {
		t.Fatalf("HandleFrame (read-write): %v", err)
	}
	if got := types.NewArray(rw.Branch("items")).Len(); got != 1 {
		t.Errorf("read-write doc len = %d, want 1", got)
	}
	if len(tr2.Broadcast) != 1 {
		t.Errorf("read-write broadcast count = %d, want 1", len(tr2.Broadcast))
	}
}

func TestHandler_ReadOnly_ServesReadsAndAwareness(t *testing.T) {
	// A read-only conn still serves SyncStep1 (state out) and still
	// broadcasts inbound awareness (presence), but still drops a
	// SyncStep2-delivered update (the other doc-mutating sub-type).
	server := doc.NewDocWithOptions(doc.Options{ClientID: 100})
	m := types.NewMap(server.Branch("settings"))
	txn := server.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()

	conn, tr := newTestConn(t, server, awareness.New(100), "viewer")
	conn.ReadOnly = true

	// SyncStep1 in must still be answered with SyncStep2 out: reads are
	// allowed for a read-only connection.
	emptySV := encoding.EncodeStateVector(map[uint64]uint64{}, nil)
	if err := conn.HandleFrame(decodeOrFail(t, syncpkg.EncodeSyncStep1(emptySV))); err != nil {
		t.Fatalf("HandleFrame SyncStep1: %v", err)
	}
	if len(tr.Sent) != 1 {
		t.Fatalf("read-only Sent count = %d, want 1 (SyncStep2 reply)", len(tr.Sent))
	}
	if reply := decodeOrFail(t, tr.Sent[0]); reply.Type != syncpkg.MessageSync || reply.SyncSub != syncpkg.SyncStep2 {
		t.Errorf("read-only SyncStep1 reply = %d/%d, want MessageSync/SyncStep2", reply.Type, reply.SyncSub)
	}

	// Awareness in must still be broadcast: presence flows for viewers.
	remoteAw := awareness.New(500)
	remoteAw.SetLocalState([]byte(`{"cursor":1}`))
	if err := conn.HandleFrame(decodeOrFail(t, syncpkg.EncodeAwareness(remoteAw.Encode(nil)))); err != nil {
		t.Fatalf("HandleFrame Awareness: %v", err)
	}
	if len(tr.Broadcast) != 1 {
		t.Errorf("read-only awareness broadcast = %d, want 1 (presence must flow)", len(tr.Broadcast))
	}

	// A SyncStep2-delivered update (the other mutating sub-type) is also
	// dropped: not applied, not broadcast.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 7})
	sa := types.NewArray(src.Branch("items"))
	stxn := src.WriteTxn()
	sa.Push(stxn, "edit")
	stxn.Commit()
	if err := conn.HandleFrame(decodeOrFail(t, syncpkg.EncodeSyncStep2(encoding.EncodeStateAsUpdate(src)))); err != nil {
		t.Fatalf("HandleFrame SyncStep2: %v", err)
	}
	if got := types.NewArray(server.Branch("items")).Len(); got != 0 {
		t.Errorf("read-only doc len after SyncStep2 = %d, want 0 (dropped)", got)
	}
	if len(tr.Broadcast) != 1 {
		t.Errorf("read-only broadcast after SyncStep2 = %d, want 1 (unchanged; update dropped)", len(tr.Broadcast))
	}
}

func TestHandler_SyncStep1_RepliesWithSyncStep2(t *testing.T) {
	// Server has one item in the map; client sends an empty SV
	// (knows nothing); server must reply with SyncStep2 carrying
	// the full state.
	server := doc.NewDocWithOptions(doc.Options{ClientID: 100})
	m := types.NewMap(server.Branch("settings"))
	txn := server.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()

	conn, tr := newTestConn(t, server, awareness.New(100), "client-A")

	emptySV := encoding.EncodeStateVector(map[uint64]uint64{}, nil)
	step1 := syncpkg.EncodeSyncStep1(emptySV)
	frame := decodeOrFail(t, step1)

	if err := conn.HandleFrame(frame); err != nil {
		t.Fatalf("HandleFrame: %v", err)
	}

	if len(tr.Sent) != 1 {
		t.Fatalf("Sent count = %d, want 1", len(tr.Sent))
	}
	reply := decodeOrFail(t, tr.Sent[0])
	if reply.Type != syncpkg.MessageSync || reply.SyncSub != syncpkg.SyncStep2 {
		t.Errorf("reply type/sub = %d/%d, want MessageSync/SyncStep2", reply.Type, reply.SyncSub)
	}
	// Apply the reply to a fresh target — should converge on the
	// same map state.
	target := doc.NewDoc()
	if err := encoding.ApplyUpdate(target, reply.Payload); err != nil {
		t.Fatal(err)
	}
	tm := types.NewMap(target.Branch("settings"))
	if got := tm.Get("k"); got != "v" {
		t.Errorf("target Get(k) = %v, want v", got)
	}
}

func TestHandler_SyncUpdate_AppliesAndBroadcasts(t *testing.T) {
	receiver := doc.NewDoc()
	conn, tr := newTestConn(t, receiver, awareness.New(receiver.ClientID()), "peer-X")

	// Build an Update from a source doc.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 200})
	arr := types.NewArray(src.Branch("items"))
	tx := src.WriteTxn()
	arr.Push(tx, "shipped")
	tx.Commit()
	update := encoding.EncodeStateAsUpdate(src)

	wire := syncpkg.EncodeSyncUpdate(update)
	frame := decodeOrFail(t, wire)
	if err := conn.HandleFrame(frame); err != nil {
		t.Fatalf("HandleFrame: %v", err)
	}

	// Receiver doc now has the item.
	larr := types.NewArray(receiver.Branch("items"))
	if larr.Len() != 1 {
		t.Errorf("Len = %d, want 1", larr.Len())
	}

	// Broadcast: one envelope, identical sync-update payload re-wrapped.
	if len(tr.Broadcast) != 1 {
		t.Fatalf("Broadcast count = %d, want 1", len(tr.Broadcast))
	}
	rebroadcast := decodeOrFail(t, tr.Broadcast[0])
	if rebroadcast.Type != syncpkg.MessageSync || rebroadcast.SyncSub != syncpkg.SyncUpdate {
		t.Errorf("rebroadcast type/sub mismatch")
	}
	if !bytes.Equal(rebroadcast.Payload, update) {
		t.Errorf("rebroadcast payload mismatch")
	}
}

func TestHandler_Awareness_AppliesAndTracksControlled(t *testing.T) {
	aw := awareness.New(999) // local clientID — separate from connection origin
	d := doc.NewDoc()
	conn, tr := newTestConn(t, d, aw, "peer-Y")

	// A remote peer (client 500) announces state via this conn.
	remoteAw := awareness.New(500)
	remoteAw.SetLocalState([]byte(`{"cursor":42}`))
	remoteUpdate := remoteAw.Encode(nil)

	wire := syncpkg.EncodeAwareness(remoteUpdate)
	if err := conn.HandleFrame(decodeOrFail(t, wire)); err != nil {
		t.Fatalf("HandleFrame: %v", err)
	}

	// Awareness state now has client 500.
	states := aw.States()
	if _, ok := states[500]; !ok {
		t.Errorf("client 500 not in awareness after apply; states=%v", states)
	}

	// Controlled list tracks 500 (this conn introduced it).
	controlled := conn.ControlledClients()
	if len(controlled) != 1 || controlled[0] != 500 {
		t.Errorf("ControlledClients = %v, want [500]", controlled)
	}

	// Broadcast forwarded the same awareness frame.
	if len(tr.Broadcast) != 1 {
		t.Fatalf("Broadcast count = %d, want 1", len(tr.Broadcast))
	}
	rebroadcast := decodeOrFail(t, tr.Broadcast[0])
	if rebroadcast.Type != syncpkg.MessageAwareness || !bytes.Equal(rebroadcast.Payload, remoteUpdate) {
		t.Errorf("rebroadcast mismatch")
	}
}

func TestHandler_QueryAwareness_RepliesWithSnapshot(t *testing.T) {
	aw := awareness.New(1)
	aw.SetLocalState([]byte(`{"local":true}`))

	// Add a remote client.
	remote := awareness.New(2)
	remote.SetLocalState([]byte(`{"remote":true}`))
	aw.Apply(remote.Encode(nil), "test-origin")

	d := doc.NewDoc()
	conn, tr := newTestConn(t, d, aw, "querier")

	if err := conn.HandleFrame(decodeOrFail(t, syncpkg.EncodeQueryAwareness())); err != nil {
		t.Fatal(err)
	}

	if len(tr.Sent) != 1 {
		t.Fatalf("Sent count = %d, want 1", len(tr.Sent))
	}
	reply := decodeOrFail(t, tr.Sent[0])
	if reply.Type != syncpkg.MessageAwareness {
		t.Errorf("reply type = %d, want MessageAwareness", reply.Type)
	}

	// Apply the snapshot to a fresh awareness and confirm both
	// clients are present.
	target := awareness.New(99)
	if _, err := target.Apply(reply.Payload, "test"); err != nil {
		t.Fatal(err)
	}
	got := target.States()
	if len(got) != 2 {
		t.Errorf("snapshot states = %v, want 2 clients", got)
	}
}

func TestHandler_Ping_RepliesWithPong(t *testing.T) {
	conn, tr := newTestConn(t, doc.NewDoc(), awareness.New(1), "pinger")

	wire := []byte{byte(syncpkg.MessagePing)}
	if err := conn.HandleFrame(decodeOrFail(t, wire)); err != nil {
		t.Fatal(err)
	}
	if len(tr.Sent) != 1 {
		t.Fatalf("Sent count = %d, want 1", len(tr.Sent))
	}
	if !bytes.Equal(tr.Sent[0], syncpkg.EncodePong()) {
		t.Errorf("reply = %x, want %x", tr.Sent[0], syncpkg.EncodePong())
	}
}

func TestHandler_UnknownMessageType_NoOp(t *testing.T) {
	conn, tr := newTestConn(t, doc.NewDoc(), awareness.New(1), "weird")
	wire := []byte{byte(syncpkg.MessageStateless), 0x00}
	if err := conn.HandleFrame(decodeOrFail(t, wire)); err != nil {
		t.Errorf("HandleFrame on unknown type returned error: %v", err)
	}
	if len(tr.Sent) != 0 || len(tr.Broadcast) != 0 {
		t.Errorf("unknown type emitted Sent=%d Broadcast=%d, want both 0",
			len(tr.Sent), len(tr.Broadcast))
	}
}

func TestHandler_Disconnect_TombstonesControlledClients(t *testing.T) {
	aw := awareness.New(1)
	d := doc.NewDoc()
	conn, _ := newTestConn(t, d, aw, "departing")

	// Introduce remote client via apply.
	remote := awareness.New(500)
	remote.SetLocalState([]byte(`{"x":1}`))
	conn.HandleFrame(decodeOrFail(t, syncpkg.EncodeAwareness(remote.Encode(nil))))

	if _, ok := aw.States()[500]; !ok {
		t.Fatal("setup: client 500 not in awareness")
	}

	tombstoned := conn.Disconnect()
	if len(tombstoned) != 1 || tombstoned[0] != 500 {
		t.Errorf("Disconnect returned %v, want [500]", tombstoned)
	}
	if _, ok := aw.States()[500]; ok {
		t.Errorf("client 500 still in awareness after Disconnect")
	}
}

func TestHandler_SendInitialSync_EmitsStep1AndAwareness(t *testing.T) {
	server := doc.NewDocWithOptions(doc.Options{ClientID: 100})
	m := types.NewMap(server.Branch("settings"))
	txn := server.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()

	aw := awareness.New(100)
	aw.SetLocalState([]byte(`{"name":"server"}`))

	conn, tr := newTestConn(t, server, aw, "fresh-client")
	if err := conn.SendInitialSync(); err != nil {
		t.Fatal(err)
	}

	if len(tr.Sent) != 2 {
		t.Fatalf("Sent count = %d, want 2 (Step1 + Awareness)", len(tr.Sent))
	}
	first := decodeOrFail(t, tr.Sent[0])
	if first.Type != syncpkg.MessageSync || first.SyncSub != syncpkg.SyncStep1 {
		t.Errorf("first frame = %d/%d, want SyncStep1", first.Type, first.SyncSub)
	}
	second := decodeOrFail(t, tr.Sent[1])
	if second.Type != syncpkg.MessageAwareness {
		t.Errorf("second frame type = %d, want MessageAwareness", second.Type)
	}
}
