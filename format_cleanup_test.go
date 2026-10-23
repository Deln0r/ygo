package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	ygo "github.com/Deln0r/ygo"
)

// boldBoth returns two documents with clients 3141592653 and 271828182
// that both bolded "xy" concurrently, and b's diff for a. Applied to a,
// yjs follows the remote update with a cleanup deleting b's opening
// marker (client 271828182, clock 0).
func boldBoth(t *testing.T) (a *ygo.Doc, ta *ygo.Text, diff []byte) {
	a, ta, _, diff = boldBothWith(t)
	return a, ta, diff
}

// boldBothWith is boldBoth that also returns b.
func boldBothWith(t *testing.T) (a *ygo.Doc, ta *ygo.Text, b *ygo.Doc, diff []byte) {
	t.Helper()
	a = ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	b = ygo.NewDocWithOptions(ygo.Options{ClientID: 271828182})
	ta, tb := ygo.NewText(a, "t"), ygo.NewText(b, "t")
	txn := a.WriteTxn()
	ta.Insert(txn, 0, "xy")
	txn.Commit()
	if err := ygo.ApplyUpdate(b, ygo.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	txn = a.WriteTxn()
	ta.Format(txn, 0, 2, ygo.Attrs{"bold": true})
	txn.Commit()
	txn = b.WriteTxn()
	tb.Format(txn, 0, 2, ygo.Attrs{"bold": true})
	txn.Commit()
	diff, err := ygo.EncodeDiff(b, ygo.EncodeStateVector(a))
	if err != nil {
		t.Fatal(err)
	}
	return a, ta, b, diff
}

const cleanupUpdate = "0001d689cf8101010001" // yjs 13.6.33: delete 271828182:0

// The cleanup after a remote update, as yjs 13.6.33 runs it when no
// observer reads the remote event's delta: the remote transaction's
// events, then a local transaction with origin nil whose observer event is
// a retain of the bolded text and whose update deletes the duplicate.
func TestFormatCleanup_Lifecycle(t *testing.T) {
	a, ta, diff := boldBoth(t)
	var log []string
	ta.Observe(func(e *ygo.TextEvent) {
		d, _ := json.Marshal(e.Delta)
		log = append(log, "observe "+string(d))
	})
	a.OnAfterTransaction(func(tr *ygo.TransactionMut) {
		log = append(log, fmt.Sprintf("after origin=%v local=%v", tr.Origin, tr.Local))
	})
	a.OnUpdate(func(u []byte, o any) { log = append(log, fmt.Sprintf("update origin=%v %x", o, u)) })
	if err := ygo.ApplyUpdateWithOrigin(a, diff, "remote"); err != nil {
		t.Fatal(err)
	}
	retain := `observe [{"Insert":"","Embed":null,"Retain":2,"Delete":0,"Attributes":{"bold":true}}]`
	want := []string{
		retain,
		"after origin=remote local=false",
		fmt.Sprintf("update origin=remote %x", diff),
		retain,
		"after origin=<nil> local=true",
		"update origin=<nil> " + cleanupUpdate,
	}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Fatalf("events\n got  %q\n want %q", log, want)
	}
}

// A cleanup that deletes nothing is still a transaction: yjs's transact
// runs it, so after-transaction handlers see it, empty and local, while
// observers and update handlers see nothing (yjs 13.6.33, concurrent bold
// of the middle of "xyz", whose duplicates sit in an inner gap).
func TestFormatCleanup_EmptyCleanupIsATransaction(t *testing.T) {
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	b := ygo.NewDocWithOptions(ygo.Options{ClientID: 271828182})
	ta, tb := ygo.NewText(a, "t"), ygo.NewText(b, "t")
	txn := a.WriteTxn()
	ta.Insert(txn, 0, "xyz")
	txn.Commit()
	if err := ygo.ApplyUpdate(b, ygo.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	txn = a.WriteTxn()
	ta.Format(txn, 1, 1, ygo.Attrs{"bold": true})
	txn.Commit()
	txn = b.WriteTxn()
	tb.Format(txn, 1, 1, ygo.Attrs{"bold": true})
	txn.Commit()
	diff, err := ygo.EncodeDiff(b, ygo.EncodeStateVector(a))
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	ta.Observe(func(*ygo.TextEvent) { log = append(log, "observe") })
	a.OnAfterTransaction(func(tr *ygo.TransactionMut) {
		log = append(log, fmt.Sprintf("after origin=%v local=%v deleted=%d", tr.Origin, tr.Local, len(tr.DeletedRanges())))
	})
	a.OnUpdate(func([]byte, any) { log = append(log, "update") })
	if err := ygo.ApplyUpdateWithOrigin(a, diff, "remote"); err != nil {
		t.Fatal(err)
	}
	want := []string{"observe", "after origin=remote local=false deleted=0", "update", "after origin=<nil> local=true deleted=0"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Fatalf("events\n got  %q\n want %q", log, want)
	}
}

// The V1 and the V2 apply both run the cleanup.
func TestFormatCleanup_EveryApplyPath(t *testing.T) {
	cases := map[string]func(t *testing.T, a, b *ygo.Doc, diff []byte){
		"V1": func(t *testing.T, a, _ *ygo.Doc, diff []byte) {
			if err := ygo.ApplyUpdate(a, diff); err != nil {
				t.Fatal(err)
			}
		},
		"V2": func(t *testing.T, a, b *ygo.Doc, _ []byte) {
			v2, err := ygo.EncodeDiffV2(b, ygo.EncodeStateVector(a))
			if err != nil {
				t.Fatal(err)
			}
			if err := ygo.ApplyUpdateV2(a, v2); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, apply := range cases {
		t.Run(name, func(t *testing.T) {
			a, _, b, diff := boldBothWith(t)
			var got []string
			a.OnUpdate(func(u []byte, _ any) { got = append(got, hex.EncodeToString(u)) })
			apply(t, a, b, diff)
			if len(got) != 2 || got[1] != cleanupUpdate {
				t.Fatalf("updates %q, want the remote one then %s", got, cleanupUpdate)
			}
		})
	}
}

// A local transaction never asks for a cleanup, and a transaction is local
// exactly when WriteTxn opened it: Origin does not decide it.
func TestTransactionMut_Local(t *testing.T) {
	a, ta, diff := boldBoth(t)
	var locals []bool
	a.OnAfterTransaction(func(tr *ygo.TransactionMut) { locals = append(locals, tr.Local) })
	txn := a.WriteTxn()
	txn.Origin = "remote-looking origin"
	ta.Insert(txn, 2, "!")
	txn.Commit()
	if err := ygo.ApplyUpdateWithOrigin(a, diff, nil); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(locals) != "[true false true]" {
		t.Fatalf("Local per transaction = %v, want [true false true] (local edit, nil-origin apply, its cleanup)", locals)
	}
}

// yjs runs the cleanup for a YText, not for an untyped root (a server's
// document, or one that only relays): a document whose root was never read
// as text applies the same updates and emits no cleanup. Read as text
// afterwards, the root asks for no cleanup, as yjs builds a fresh YText,
// until a format marker arrives.
func TestFormatCleanup_UntypedRoot(t *testing.T) {
	a, _, diff := boldBoth(t)
	c := ygo.NewDocWithOptions(ygo.Options{ClientID: 141421356})
	var got int
	c.OnUpdate(func([]byte, any) { got++ })
	for _, u := range [][]byte{ygo.EncodeStateAsUpdate(a), diff} {
		if err := ygo.ApplyUpdate(c, u); err != nil {
			t.Fatal(err)
		}
	}
	if got != 2 {
		t.Fatalf("untyped root: %d updates for 2 applies, want no cleanup", got)
	}
}

// An XmlText's observer sees its text change, as a TextEvent (yjs fires a
// YTextEvent for a YXmlText).
func TestXmlText_ObserveSeesTextChanges(t *testing.T) {
	d := ygo.NewDoc()
	f := ygo.NewXmlFragment(d, "x")
	txn := d.WriteTxn()
	xt := f.InsertXmlText(txn, 0)
	txn.Commit()
	var deltas []string
	xt.Observe(func(e *ygo.TextEvent) {
		j, _ := json.Marshal(e.Delta)
		deltas = append(deltas, string(j))
	})
	txn = d.WriteTxn()
	xt.Insert(txn, 0, "hi")
	txn.Commit()
	if len(deltas) != 1 || deltas[0] != `[{"Insert":"hi","Embed":null,"Retain":0,"Delete":0,"Attributes":null}]` {
		t.Fatalf("XmlText observer got %q, want one insert of hi", deltas)
	}
}

// With two texts to clean, the cleanup's observers fire in the order the
// cleanup changed the texts, which is the order of their new markers'
// clients in the store, not the order the remote update integrated them
// in (yjs 13.6.33: client 271828182 was known before 3500000000, whose
// marker integrates first, so the remote events go p, q and the cleanup's
// q, p).
func TestFormatCleanup_ObserverOrderOverTwoTexts(t *testing.T) {
	const (
		bEarly  = "0101d689cf81010004010172012100"
		merged  = "02028086f7840d0046cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c02d689cf81010146cdcc83da0b0204626f6c64047472756586cdcc83da0b0304626f6c64046e756c6c00"
		cleanup = "0002cdcc83da0b010401d689cf8101010101"
	)
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	p, q := ygo.NewText(a, "p"), ygo.NewText(a, "q")
	txn := a.WriteTxn()
	p.Insert(txn, 0, "xy")
	q.Insert(txn, 0, "uv")
	txn.Commit()
	u, _ := hex.DecodeString(bEarly)
	if err := ygo.ApplyUpdate(a, u); err != nil {
		t.Fatal(err)
	}
	txn = a.WriteTxn()
	p.Format(txn, 0, 2, ygo.Attrs{"bold": true})
	q.Format(txn, 0, 2, ygo.Attrs{"bold": true})
	txn.Commit()
	var log []string
	p.Observe(func(*ygo.TextEvent) { log = append(log, "p") })
	q.Observe(func(*ygo.TextEvent) { log = append(log, "q") })
	a.OnAfterTransaction(func(tr *ygo.TransactionMut) { log = append(log, fmt.Sprintf("after local=%v", tr.Local)) })
	a.OnUpdate(func(u []byte, o any) {
		if o == nil {
			log = append(log, fmt.Sprintf("update %x", u))
		}
	})
	u, _ = hex.DecodeString(merged)
	if err := ygo.ApplyUpdateWithOrigin(a, u, "remote"); err != nil {
		t.Fatal(err)
	}
	want := []string{"p", "q", "after local=false", "q", "p", "after local=true", "update " + cleanup}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Fatalf("events\n got  %q\n want %q", log, want)
	}
}

// The remote update and its cleanup are one critical section: with another
// writer busy on the same document, the update right after the remote one
// is always its cleanup.
func TestFormatCleanup_NoWriterBetween(t *testing.T) {
	for i := 0; i < 20; i++ {
		a, _, diff := boldBoth(t)
		other := ygo.NewArray(a, "other")
		var mu sync.Mutex
		var got []string
		a.OnUpdate(func(u []byte, o any) {
			mu.Lock()
			got = append(got, fmt.Sprint(o)+" "+hex.EncodeToString(u))
			mu.Unlock()
		})
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				txn := a.WriteTxn()
				txn.Origin = "writer"
				other.Push(txn, 1)
				txn.Commit()
			}
		}()
		if err := ygo.ApplyUpdateWithOrigin(a, diff, "remote"); err != nil {
			t.Fatal(err)
		}
		close(stop)
		<-done
		mu.Lock()
		for k, e := range got {
			if e[:7] == "remote " {
				if k+1 >= len(got) || got[k+1] != "<nil> "+cleanupUpdate {
					t.Fatalf("after the remote update came %q, want the cleanup", got[k+1:])
				}
			}
		}
		mu.Unlock()
	}
}

// A format update that arrives before the text it formats waits in the
// queue; when the text arrives and lets it in, that transaction gets the
// cleanup (yjs 13.6.33: the first apply emits no update, the second emits
// the merged structs and then the cleanup).
func TestFormatCleanup_QueuedUpdateLetIn(t *testing.T) {
	a, _, diff := boldBoth(t)
	c := ygo.NewDocWithOptions(ygo.Options{ClientID: 141421356})
	ygo.NewText(c, "t")
	var got []string
	c.OnUpdate(func(u []byte, o any) { got = append(got, fmt.Sprintf("%v %x", o, u)) })
	if err := ygo.ApplyUpdateWithOrigin(c, diff, "first"); err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdateWithOrigin(c, ygo.EncodeStateAsUpdate(a), "second"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"second 0203cdcc83da0b000401017402787946cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c02d689cf81010046cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c00",
		"<nil> " + cleanupUpdate,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("updates\n got  %q\n want %q", got, want)
	}
}
