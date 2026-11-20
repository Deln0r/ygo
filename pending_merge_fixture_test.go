package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/Deln0r/ygo"
)

// pendingMergeScenario is one entry of testdata/pending-merge-fixtures.json
// (testdata/gen/gen-pending-merge.mjs, yjs 13.6.33).
type pendingMergeScenario struct {
	pendingStateScenario
	UpdatesV2     []string `json:"updatesV2"`
	Final         string   `json:"final"`
	FinalUpdateV2 string   `json:"finalUpdateV2"`
	FinalV1       string   `json:"finalV1"`
	FinalV2       string   `json:"finalV2"`
	FinalPending  bool     `json:"finalPending"`
	Events        []string `json:"events"`
	EventsV2      []string `json:"eventsV2"`
}

// TestPendingMerge_Fixtures: updates that wait for what they depend on and
// cover the same clocks in different ways join the queue as yjs merges
// them (mergeUpdatesV2 of the queue and the leftover): at an equal start
// the decoder ahead wins and the other is cut, the one ahead carried from
// client to client; GC and an item at one start go to the second; GC runs
// merge, items never. The state, whole and as diffs, V1 and V2, then the
// document after the missing update and that transaction's update events,
// are yjs's. Each scenario is replayed with V1 and with V2 updates.
func TestPendingMerge_Fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/pending-merge-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []pendingMergeScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, s := range f.Scenarios {
		for _, format := range []string{"v1", "v2"} {
			t.Run(s.Name+"/"+format, func(t *testing.T) {
				d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99, DisableGC: !s.GC})
				apply := func(u string, v2 string) {
					t.Helper()
					var err error
					if format == "v1" {
						b, _ := hex.DecodeString(u)
						err = ygo.ApplyUpdate(d, b)
					} else {
						b, _ := hex.DecodeString(v2)
						err = ygo.ApplyUpdateV2(d, b)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				for i, u := range s.Updates {
					apply(u, s.UpdatesV2[i])
				}
				if !ygo.HasPending(d) {
					t.Fatal("nothing pending")
				}
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
					t.Errorf("state V1:\n got %s\nwant %s", got, s.V1)
				}
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
					t.Errorf("state V2:\n got %s\nwant %s", got, s.V2)
				}
				for _, df := range s.Diffs {
					sv, _ := hex.DecodeString(df.SV)
					g1, err := ygo.EncodeDiff(d, sv)
					if err != nil {
						t.Fatal(err)
					}
					g2, err := ygo.EncodeDiffV2(d, sv)
					if err != nil {
						t.Fatal(err)
					}
					if got := hex.EncodeToString(g1); got != df.V1 {
						t.Errorf("diff V1 against %s:\n got %s\nwant %s", df.SV, got, df.V1)
					}
					if got := hex.EncodeToString(g2); got != df.V2 {
						t.Errorf("diff V2 against %s:\n got %s\nwant %s", df.SV, got, df.V2)
					}
				}
				// Encoding cuts copies: after all of the above the queue and
				// the store still write the same state.
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
					t.Errorf("state V1 after the diffs:\n got %s\nwant %s", got, s.V1)
				}
				if s.Final == "" {
					return
				}
				var events, eventsV2 []string
				d.OnUpdate(func(u []byte, _ any) { events = append(events, hex.EncodeToString(u)) })
				d.OnUpdateV2(func(u []byte, _ any) { eventsV2 = append(eventsV2, hex.EncodeToString(u)) })
				apply(s.Final, s.FinalUpdateV2)
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.FinalV1 {
					t.Errorf("final state V1:\n got %s\nwant %s", got, s.FinalV1)
				}
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.FinalV2 {
					t.Errorf("final state V2:\n got %s\nwant %s", got, s.FinalV2)
				}
				if got := ygo.HasPending(d); got != s.FinalPending {
					t.Errorf("pending after the missing update: %v, want %v", got, s.FinalPending)
				}
				if !slices.Equal(events, s.Events) {
					t.Errorf("update events V1:\n got %v\nwant %v", events, s.Events)
				}
				if !slices.Equal(eventsV2, s.EventsV2) {
					t.Errorf("update events V2:\n got %v\nwant %v", eventsV2, s.EventsV2)
				}
			})
		}
	}
}
