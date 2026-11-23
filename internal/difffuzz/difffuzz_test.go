package difffuzz

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestGenerate_Deterministic: a seed gives one scenario, and a scenario
// survives JSON (the form the oracle gets) unchanged and replays the same.
func TestGenerate_Deterministic(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		a, b := Generate(seed), Generate(seed)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("seed %d: two scenarios", seed)
		}
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var c Scenario
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if d := Compare(a, Run(a, RunOptions{}), Run(c, RunOptions{})); d != nil {
			t.Fatalf("seed %d: the scenario through JSON replays differently: %v", seed, d)
		}
	}
}

// TestRun_GeneratedOpsAreValid: the generator only makes steps ygo takes
// without an error or a panic, and runs end converged (strings within the
// BMP; see GenOptions).
func TestRun_GeneratedOpsAreValid(t *testing.T) {
	for seed := uint64(1); seed <= 200; seed++ {
		s := GenerateWith(seed, GenOptions{BMPOnly: true})
		tr := Run(s, RunOptions{})
		for i, st := range tr.Steps {
			if st.Err != "" {
				t.Fatalf("seed %d step %d (%s): %s", seed, i, s.Steps[i].K, st.Err)
			}
		}
		if d := Converged(s, tr); d != nil {
			t.Fatalf("seed %d: %v", seed, d)
		}
	}
}

// TestCompare_CatchesPlanted: a changed byte of an event, a missing event,
// an extra event, a changed view, a changed state hash, changed sent bytes
// and a changed undo result are each the first divergence, at the step
// they were planted in.
func TestCompare_CatchesPlanted(t *testing.T) {
	s := Generate(7)
	base := Run(s, RunOptions{})
	if d := Compare(s, base, Run(s, RunOptions{})); d != nil {
		t.Fatalf("two runs of one scenario differ: %v", d)
	}
	first := func(pred func(StepTrace) bool) int {
		for i, st := range base.Steps {
			if pred(st) {
				return i
			}
		}
		t.Fatal("no step to plant in")
		return -1
	}
	withEvents := first(func(st StepTrace) bool { return len(st.Events) > 0 })
	withSent := first(func(st StepTrace) bool { return st.Sent != "" })
	withPeers := first(func(st StepTrace) bool { return len(st.Peers) > 0 })
	cases := []struct {
		name  string
		step  int
		field string
		plant func(*StepTrace)
	}{
		{"a flipped event byte", withEvents, "events", func(st *StepTrace) {
			h := []byte(st.Events[0].Hex)
			if h[len(h)-1] == '0' {
				h[len(h)-1] = '1'
			} else {
				h[len(h)-1] = '0'
			}
			st.Events[0].Hex = string(h)
		}},
		{"a missing event", withEvents, "events", func(st *StepTrace) { st.Events = st.Events[1:] }},
		{"an extra event", withEvents, "events", func(st *StepTrace) { st.Events = append(st.Events, st.Events[0]) }},
		{"sent bytes", withSent, "sent", func(st *StepTrace) { st.Sent += "00" }},
		{"a view", withPeers, "peers.view", func(st *StepTrace) { st.Peers[0].View += " " }},
		{"a V2 state", withPeers, "peers.v2", func(st *StepTrace) { st.Peers[0].V2 = strings.Repeat("0", 16) }},
		{"pending", withPeers, "peers.pending", func(st *StepTrace) { st.Peers[0].Pending = !st.Peers[0].Pending }},
	}
	for _, c := range cases {
		c := c
		planted := Run(s, RunOptions{Plant: func(i int, st *StepTrace) {
			if i == c.step {
				c.plant(st)
			}
		}})
		d := Compare(s, planted, base)
		if d == nil || d.Step != c.step || d.Field != c.field {
			t.Errorf("%s at step %d: got %v, want %s there", c.name, c.step, d, c.field)
		}
	}
}

// TestCompare_CatchesPlantedObserverEvents: in a scenario with observers,
// a changed, missing or extra observer event is the first divergence.
func TestCompare_CatchesPlantedObserverEvents(t *testing.T) {
	var s Scenario
	for seed := uint64(1); ; seed++ {
		if s = Generate(seed); s.Observe {
			break
		}
	}
	base := Run(s, RunOptions{})
	step := -1
	for i, st := range base.Steps {
		if len(st.Obs) > 0 {
			step = i
			break
		}
	}
	if step < 0 {
		t.Fatal("no observer event to plant in")
	}
	for name, plant := range map[string]func(*StepTrace){
		"changed": func(st *StepTrace) { st.Obs[0].Change += " " },
		"missing": func(st *StepTrace) { st.Obs = st.Obs[1:] },
		"extra":   func(st *StepTrace) { st.Obs = append(st.Obs, st.Obs[0]) },
	} {
		planted := Run(s, RunOptions{Plant: func(i int, st *StepTrace) {
			if i == step {
				plant(st)
			}
		}})
		if d := Compare(s, planted, base); d == nil || d.Step != step || d.Field != "obs" {
			t.Errorf("%s observer event at step %d: got %v", name, step, d)
		}
	}
}

// TestShrink_KeepsTheFailure: a divergence planted at every transaction that
// formats text shrinks to a scenario of one such transaction and the final
// step, failing the same way.
func TestShrink_KeepsTheFailure(t *testing.T) {
	formats := func(st Step) bool {
		for _, op := range st.Ops {
			if op.O == OpFmt {
				return true
			}
		}
		return false
	}
	check := func(c Scenario) *Divergence {
		planted := Run(c, RunOptions{Plant: func(i int, st *StepTrace) {
			if c.Steps[i].K == StepTxn && formats(c.Steps[i]) {
				st.Err = "error"
			}
		}})
		return Compare(c, planted, Run(c, RunOptions{}))
	}
	for seed := uint64(1); seed <= 20; seed++ {
		s := Generate(seed)
		d := check(s)
		if d == nil {
			continue
		}
		small := Shrink(s, d.Class(), 2000, check)
		sd := check(small)
		if sd == nil || sd.Class() != d.Class() {
			t.Fatalf("seed %d: the shrunk scenario fails as %v, want %s", seed, sd, d.Class())
		}
		if len(small.Steps) != 2 || len(small.Steps[0].Ops) != 1 || small.Steps[0].Ops[0].O != OpFmt {
			b, _ := json.Marshal(small)
			t.Fatalf("seed %d: shrunk to %s", seed, b)
		}
		return
	}
	t.Fatal("no seed formats text")
}
