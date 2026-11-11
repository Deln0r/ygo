package sync_test

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
)

func newHocusModeConn(t *testing.T) (*syncpkg.Conn, *memTransport) {
	t.Helper()
	tr := &memTransport{}
	c := syncpkg.New(doc.NewDocWithOptions(doc.Options{ClientID: 3918302149}), awareness.New(1), "conn-1")
	c.DocName = "seeded-b"
	c.Send = tr.send
	c.Broadcast = tr.broadcast
	c.Hocuspocus = true
	return c, tr
}

func handle(t *testing.T, c *syncpkg.Conn, env []byte) {
	t.Helper()
	f, _, err := syncpkg.DecodeEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.HandleFrame(f); err != nil {
		t.Fatal(err)
	}
}

func frames(t *testing.T, envs [][]byte) []*syncpkg.Frame {
	t.Helper()
	out := make([]*syncpkg.Frame, len(envs))
	for i, e := range envs {
		f, _, err := syncpkg.DecodeEnvelope(e)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = f
	}
	return out
}

// TestHocusMode_SyncStep1Reply: a client SyncStep1 gets the server's
// SyncStep1 first, then the SyncStep2, as every Hocuspocus server sends
// them (captured: 0007 01 c597b2cc0e 04, then the diff).
func TestHocusMode_SyncStep1Reply(t *testing.T) {
	c, tr := newHocusModeConn(t)
	tx := types.NewText(c.Doc.Branch("t"))
	txn := c.Doc.WriteTxn()
	_ = tx.Insert(txn, 0, "seed")
	txn.Commit()

	handle(t, c, syncpkg.EncodeSyncStep1(mustHex(t, "00")))
	got := frames(t, tr.Sent)
	if len(got) != 2 || got[0].SyncSub != syncpkg.SyncStep1 || got[1].SyncSub != syncpkg.SyncStep2 {
		t.Fatalf("replies %+v, want SyncStep1 then SyncStep2", got)
	}
	if h := hex.EncodeToString(tr.Sent[0]); h != "000007"+"01c597b2cc0e04" {
		t.Errorf("SyncStep1 reply %s, want 00000701c597b2cc0e04 (captured)", h)
	}
	if h := hex.EncodeToString(tr.Sent[1]); h != "0001120101c597b2cc0e0004010174047365656400" {
		t.Errorf("SyncStep2 reply %s, want the captured diff", h)
	}
}

// TestHocusMode_SyncStatus: every client SyncStep2 and Update gets one
// SyncStatus 1, before the broadcast; one that fails to apply gets
// SyncStatus 1 and no broadcast, without an error.
func TestHocusMode_SyncStatus(t *testing.T) {
	c, tr := newHocusModeConn(t)
	update := mustHex(t, "0101919fa0d904000401017402633100")
	handle(t, c, syncpkg.EncodeSyncStep2(mustHex(t, "0000")))
	handle(t, c, syncpkg.EncodeSyncUpdate(update))
	handle(t, c, syncpkg.EncodeSyncUpdate(mustHex(t, "ff")))
	sent := frames(t, tr.Sent)
	if len(sent) != 3 {
		t.Fatalf("sent %d frames, want 3 SyncStatus", len(sent))
	}
	for i, f := range sent {
		if f.Type != syncpkg.MessageSyncStatus || f.Payload[0] != 1 {
			t.Errorf("reply %d = %+v, want SyncStatus 1", i, f)
		}
	}
	if len(tr.Broadcast) != 2 {
		t.Errorf("broadcasts = %d, want 2 (the empty SyncStep2 and the update; not the bad bytes)", len(tr.Broadcast))
	}
}

// TestHocusMode_ReadOnly: a read-only connection's SyncStep2 gets
// SyncStatus 1 when it adds nothing and 0 when it would; its Update
// always gets 0 (captured hp4-hps4-readonly: 1, 0, 0). Nothing applies.
func TestHocusMode_ReadOnly(t *testing.T) {
	c, tr := newHocusModeConn(t)
	c.ReadOnly = true
	content := mustHex(t, "0101919fa0d904000401017402633100")
	handle(t, c, syncpkg.EncodeSyncStep2(mustHex(t, "0000")))
	handle(t, c, syncpkg.EncodeSyncStep2(content))
	handle(t, c, syncpkg.EncodeSyncUpdate(content))
	handle(t, c, syncpkg.EncodeSyncUpdate(mustHex(t, "0000")))
	var got []byte
	for _, f := range frames(t, tr.Sent) {
		if f.Type != syncpkg.MessageSyncStatus {
			t.Fatalf("unexpected reply %+v", f)
		}
		got = append(got, f.Payload[0])
	}
	if hex.EncodeToString(got) != "01000000" {
		t.Errorf("SyncStatus values %x, want 01000000", got)
	}
	if len(tr.Broadcast) != 0 {
		t.Errorf("a read-only connection broadcast %d frames", len(tr.Broadcast))
	}
	rtxn := c.Doc.ReadTxn()
	n := len(rtxn.Store().GetStateVector())
	rtxn.Close()
	if n != 0 {
		t.Error("a read-only connection's update was applied")
	}
}

// TestHocusMode_Silent: QueryAwareness, Ping and Auth after establishment
// get no reply on a Hocuspocus connection (no server answers them).
func TestHocusMode_Silent(t *testing.T) {
	c, tr := newHocusModeConn(t)
	handle(t, c, syncpkg.EncodeQueryAwareness())
	handle(t, c, mustHex(t, "09"))
	handle(t, c, syncpkg.EncodeAuthToken("again"))
	if len(tr.Sent) != 0 || len(tr.Broadcast) != 0 {
		t.Errorf("replies %d, broadcasts %d, want none", len(tr.Sent), len(tr.Broadcast))
	}
}

// TestUpdateAddsNothing covers the snapshot check: a known block, a known
// deletion, a new block and a new deletion.
func TestUpdateAddsNothing(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1})
	tx := types.NewText(d.Branch("t"))
	txn := d.WriteTxn()
	_ = tx.Insert(txn, 0, "abc")
	txn.Commit()
	full := encoding.EncodeStateAsUpdate(d)
	txn = d.WriteTxn()
	_ = tx.Delete(txn, 1, 1)
	txn.Commit()
	withDelete := encoding.EncodeStateAsUpdate(d)

	other := doc.NewDocWithOptions(doc.Options{ClientID: 2})
	if err := encoding.ApplyUpdate(other, full); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		d      *doc.Doc
		update []byte
		want   bool
	}{
		{"empty update", other, mustHex(t, "0000"), true},
		{"blocks it has", other, full, true},
		{"a deletion it lacks", other, withDelete, false},
		{"the same deletion it has", d, withDelete, true},
		{"blocks it lacks", doc.NewDoc(), full, false},
	} {
		got, err := encoding.UpdateAddsNothing(c.d, c.update)
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
}

func TestHocusMode_AppliedUpdateBroadcastsAfterFailedAck(t *testing.T) {
	c, tr := newHocusModeConn(t)
	d := doc.NewDoc()
	a := types.NewArray(d.Branch("items"))
	txn := d.WriteTxn()
	a.Push(txn, "saved")
	txn.Commit()
	writeErr := errors.New("socket closed")
	c.Send = func([]byte) error { return writeErr }
	f, _, err := syncpkg.DecodeEnvelope(syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(d)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.HandleFrame(f); !errors.Is(err, writeErr) {
		t.Fatal(err)
	}
	if len(tr.Broadcast) != 1 {
		t.Fatal("applied update did not reach broadcast/persistence callback")
	}
}
