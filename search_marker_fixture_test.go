package ygo_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Deln0r/ygo"
)

// searchMarkerFixtures is testdata/search-marker-fixtures.json, written by
// testdata/gen/gen-search-markers.mjs from yjs 13.6.33: the search markers
// of a text whose root was typed after formatted text arrived, after every
// op and every commit.
type searchMarkerFixtures struct {
	Yjs       string                 `json:"yjs"`
	Scenarios []searchMarkerScenario `json:"scenarios"`
}

type searchMarkerScenario struct {
	Name     string             `json:"name"`
	Kind     string             `json:"kind"`
	Writer   uint64             `json:"writer"`
	Receiver uint64             `json:"receiver"`
	State    string             `json:"state"`
	Steps    []searchMarkerStep `json:"steps"`
	Final    struct {
		State string `json:"state"`
	} `json:"final"`
}

type searchMarkerStep struct {
	Typed     *searchMarkerDump `json:"typed"`
	Ops       []updateEventOp   `json:"ops"`
	After     json.RawMessage   `json:"after"`
	Committed *searchMarkerDump `json:"committed"`
	Remote    json.RawMessage   `json:"remote"`
	Update    string            `json:"update"`
}

// searchMarkerDump is yjs's _searchMarker as [client, clock, index] in
// array order (null once the markers are off) and the items whose marker
// flag is set, as [client, clock] in list order.
type searchMarkerDump struct {
	Markers *[][3]int64 `json:"markers"`
	Marked  [][2]uint64 `json:"marked"`
}

func dumpSearchMarkers(t *ygo.Text) searchMarkerDump {
	br := t.Branch()
	var d searchMarkerDump
	if !br.FormattedText {
		list := [][3]int64{}
		for i := 0; i < br.TextMarkers.Len(); i++ {
			m := br.TextMarkers.At(i)
			list = append(list, [3]int64{int64(m.P.ID.Client), int64(m.P.ID.Clock), m.Index})
		}
		d.Markers = &list
	}
	d.Marked = [][2]uint64{}
	for it := br.Start; it != nil; it = it.Right {
		if it.IsMarked() {
			d.Marked = append(d.Marked, [2]uint64{it.ID.Client, it.ID.Clock})
		}
	}
	return d
}

func (d searchMarkerDump) String() string {
	b, _ := json.Marshal(d)
	return string(b)
}

func sameSearchMarkerDump(a, b searchMarkerDump) bool { return a.String() == b.String() }

// TestSearchMarkers_Fixtures replays each scenario through ygo's API: the
// receiver applies the writer's state while its root is untyped, types it,
// and runs the same transactions and remote updates; its TextMarkers and
// marker flags must equal yjs's after every op and every commit, and its
// state must be yjs's byte for byte.
func TestSearchMarkers_Fixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "search-marker-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx searchMarkerFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, sc := range fx.Scenarios {
		t.Run(sc.Name, func(t *testing.T) { replaySearchMarkers(t, sc) })
	}
}

func replaySearchMarkers(t *testing.T, sc searchMarkerScenario) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: sc.Receiver})
	if err := ygo.ApplyUpdateWithOrigin(d, mustHex(t, sc.State), "remote"); err != nil {
		t.Fatal(err)
	}
	var text *ygo.Text
	switch sc.Kind {
	case "text":
		text = ygo.NewText(d, "t")
	case "xmltext":
		text = &ygo.NewXmlText(d, "t").Text
	default:
		t.Fatalf("unknown kind %q", sc.Kind)
	}
	check := func(where string, want *searchMarkerDump) {
		t.Helper()
		if want == nil {
			return
		}
		if got := dumpSearchMarkers(text); !sameSearchMarkerDump(got, *want) {
			t.Fatalf("%s:\n got  %s\n want %s", where, got, want)
		}
	}
	for k, st := range sc.Steps {
		switch {
		case st.Typed != nil:
			check("typed", st.Typed)
		case st.Remote != nil:
			if err := ygo.ApplyUpdateWithOrigin(d, mustHex(t, st.Update), "remote"); err != nil {
				t.Fatal(err)
			}
			var after searchMarkerDump
			if err := json.Unmarshal(st.After, &after); err != nil {
				t.Fatal(err)
			}
			check(fmt.Sprintf("step %d after remote %s", k, st.Remote), &after)
		default:
			var after []searchMarkerDump
			if st.After != nil {
				if err := json.Unmarshal(st.After, &after); err != nil {
					t.Fatal(err)
				}
			}
			txn := d.WriteTxn()
			for i, op := range st.Ops {
				if err := applySearchMarkerOp(t, txn, text, op); err != nil {
					txn.Commit()
					t.Fatalf("step %d op %d: %v", k, i, err)
				}
				if after != nil {
					check(fmt.Sprintf("step %d after op %d (%s)", k, i, op.Op), &after[i])
				}
			}
			txn.Commit()
			check(fmt.Sprintf("step %d committed", k), st.Committed)
		}
	}
	if got, want := ygo.EncodeStateAsUpdate(d), mustHex(t, sc.Final.State); !bytes.Equal(got, want) {
		t.Fatalf("state:\n got  %x\n want %x", got, want)
	}
}

func applySearchMarkerOp(t *testing.T, txn *ygo.TransactionMut, x *ygo.Text, op updateEventOp) error {
	t.Helper()
	switch op.Op {
	case "insert":
		if len(op.Attrs) > 0 {
			return x.InsertWithAttributes(txn, op.Index, op.Text, jsonAttrs(t, op.Attrs))
		}
		return x.Insert(txn, op.Index, op.Text)
	case "delete":
		return x.Delete(txn, op.Index, op.Len)
	case "format":
		return x.Format(txn, op.Index, op.Len, jsonAttrs(t, op.Attrs))
	case "embed":
		if len(op.Attrs) > 0 {
			return x.InsertEmbedWithAttributes(txn, op.Index, jsonValue(t, op.Value), jsonAttrs(t, op.Attrs))
		}
		return x.InsertEmbed(txn, op.Index, jsonValue(t, op.Value))
	case "delta":
		return x.ApplyDelta(txn, jsonDelta(t, op.Delta))
	}
	return fmt.Errorf("unknown op %q", op.Op)
}
