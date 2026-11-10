package gomobile_test

import (
	"math"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/gomobile"
)

type mapChanges struct{ got []string }

func (m *mapChanges) OnMapChange(keysJSON []byte) { m.got = append(m.got, string(keysJSON)) }

// TestJSON_AnyValuesAsJSONStringify: values a peer stored as undefined,
// -0 or BigInt reach Swift and Kotlin as JSON.stringify would write them
// (BigInt, where it throws, as a number), and an undefined oldValue is left
// out of the event, as JSON.stringify leaves out an undefined entry.
func TestJSON_AnyValuesAsJSONStringify(t *testing.T) {
	src := ygo.NewDoc()
	m := ygo.NewMap(src, "m")
	txn := src.WriteTxn()
	m.Set(txn, "obj", map[string]any{"a": ygo.Undefined{}, "b": math.Copysign(0, -1), "c": ygo.BigInt(1<<53 + 1)})
	m.Set(txn, "arr", []any{ygo.Undefined{}, math.Copysign(0, -1)})
	m.Set(txn, "u", ygo.Undefined{})
	txn.Commit()

	d := gomobile.NewDoc()
	if err := d.ApplyUpdate(ygo.EncodeStateAsUpdate(src)); err != nil {
		t.Fatal(err)
	}
	gm := d.Map("m")
	for key, want := range map[string]string{
		"obj": `{"b":0,"c":9007199254740993}`,
		"arr": `[null,0]`,
		"u":   `null`,
	} {
		if got := string(gm.GetJSON(key)); got != want {
			t.Errorf("GetJSON(%q) = %s, want %s", key, got, want)
		}
	}

	changes := &mapChanges{}
	gm.ObserveChanges(changes)
	sv := ygo.EncodeStateVector(src)
	txn = src.WriteTxn()
	m.Set(txn, "u", int64(1))
	txn.Commit()
	diff, err := ygo.EncodeDiff(src, sv)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyUpdate(diff); err != nil {
		t.Fatal(err)
	}
	if len(changes.got) != 1 || changes.got[0] != `{"u":{"action":"update"}}` {
		t.Fatalf("map change JSON = %q, want {\"u\":{\"action\":\"update\"}}", changes.got)
	}
}
