package difffuzz

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Divergence is the first place two traces differ, or where one
// implementation's peers did not converge.
type Divergence struct {
	Step  int    `json:"step"`
	Kind  string `json:"kind"`  // the step's kind
	Field string `json:"field"` // e.g. "events", "peers.v1", "converge.go"
	Where string `json:"where"` // the peer or event index, for the report
	Go    string `json:"go"`
	JS    string `json:"js"`
}

// Class is what a shrunk scenario must keep failing on.
func (d *Divergence) Class() string { return d.Kind + "/" + d.Field }

func (d *Divergence) String() string {
	return fmt.Sprintf("step %d (%s): %s differs %s\n  go: %s\n  js: %s", d.Step, d.Kind, d.Field, d.Where, clip(d.Go), clip(d.JS))
}

func clip(s string) string {
	if len(s) <= 600 {
		return s
	}
	n := 600
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + fmt.Sprintf("... (%d bytes)", len(s))
}

// Compare walks the traces of s step by step and returns the first
// difference, then checks that each side's peers converged at the end.
func Compare(s Scenario, g, j Trace) *Divergence {
	for i := 0; i < len(s.Steps); i++ {
		kind := s.Steps[i].K
		if i >= len(g.Steps) || i >= len(j.Steps) {
			return &Divergence{Step: i, Kind: kind, Field: "steps", Go: fmt.Sprint(len(g.Steps)), JS: fmt.Sprint(len(j.Steps))}
		}
		if d := compareStep(g.Steps[i], j.Steps[i]); d != nil {
			d.Step, d.Kind = i, kind
			return d
		}
	}
	// A side whose peers end apart: yjs does where two peers cut one
	// surrogate pair in different places, each replacing the halves it cut
	// with U+FFFD locally. The steps above already hold Go to yjs there;
	// Go's peers must converge wherever yjs's do.
	if converged(s, j) == nil {
		if d := Converged(s, g); d != nil {
			d.Field += ".go"
			return d
		}
	}
	return nil
}

func compareStep(g, j StepTrace) *Divergence {
	diff := func(field, where, a, b string) *Divergence {
		return &Divergence{Field: field, Where: where, Go: a, JS: b}
	}
	if g.Skip != j.Skip {
		return diff("skip", "", fmt.Sprint(g.Skip), fmt.Sprint(j.Skip))
	}
	if (g.Err != "") != (j.Err != "") || strings.HasPrefix(g.Err, "panic") {
		return diff("err", "", g.Err, j.Err)
	}
	if (g.Result == nil) != (j.Result == nil) || (g.Result != nil && *g.Result != *j.Result) {
		return diff("result", "", show(g.Result), show(j.Result))
	}
	if g.Sent != j.Sent {
		return diff("sent", "", g.Sent, j.Sent)
	}
	if g.SentSV != j.SentSV || g.SentMeta != j.SentMeta {
		return diff("sentMeta", "", g.SentSV+" "+g.SentMeta, j.SentSV+" "+j.SentMeta)
	}
	if show(g.Depths) != show(j.Depths) {
		return diff("depths", "", show(g.Depths), show(j.Depths))
	}
	if len(g.Events) != len(j.Events) {
		return diff("events", "count", show(g.Events), show(j.Events))
	}
	for k := range g.Events {
		if g.Events[k] != j.Events[k] {
			return diff("events", fmt.Sprintf("#%d peer %d V%s", k, j.Events[k].Peer, j.Events[k].V), show(g.Events[k]), show(j.Events[k]))
		}
	}
	if len(g.Peers) != len(j.Peers) {
		return diff("peers", "count", show(g.Peers), show(j.Peers))
	}
	if len(g.Obs) != len(j.Obs) {
		return diff("obs", "count", show(g.Obs), show(j.Obs))
	}
	for k := range g.Obs {
		if g.Obs[k] != j.Obs[k] {
			return diff("obs", fmt.Sprintf("#%d peer %d root %s", k, j.Obs[k].Peer, j.Obs[k].Root), show(g.Obs[k]), show(j.Obs[k]))
		}
	}
	for k := range g.Peers {
		a, b := g.Peers[k], j.Peers[k]
		where := fmt.Sprintf("peer %d", b.Peer)
		switch {
		case a.Peer != b.Peer:
			return diff("peers", "order", show(g.Peers), show(j.Peers))
		case a.View != b.View:
			return diff("peers.view", where, a.View, b.View)
		case a.SV != b.SV:
			return diff("peers.sv", where, a.SV, b.SV)
		case a.Pending != b.Pending:
			return diff("peers.pending", where, fmt.Sprint(a.Pending), fmt.Sprint(b.Pending))
		case a.V1 != b.V1:
			return diff("peers.v1", where, a.V1, b.V1)
		case a.V2 != b.V2:
			return diff("peers.v2", where, a.V2, b.V2)
		case a.CanUndo != b.CanUndo || a.CanRedo != b.CanRedo:
			return diff("peers.undo", where, fmt.Sprint(a.CanUndo, a.CanRedo), fmt.Sprint(b.CanUndo, b.CanRedo))
		}
	}
	return nil
}

// Converged checks that the peers of one trace of s were together after
// its last final step.
func Converged(s Scenario, tr Trace) *Divergence {
	d := converged(s, tr)
	if d != nil {
		d.Step, d.Kind = finalStep(s), StepFinal
	}
	return d
}

func finalStep(s Scenario) int {
	for i := len(s.Steps) - 1; i >= 0; i-- {
		if s.Steps[i].K == StepFinal {
			return i
		}
	}
	return -1
}

// converged checks the final step: every peer with the same view and state
// vector, nothing pending.
func converged(s Scenario, tr Trace) *Divergence {
	f := finalStep(s)
	if f < 0 || f >= len(tr.Steps) {
		return nil
	}
	last := tr.Steps[f]
	if last.Err != "" || len(last.Peers) == 0 {
		return nil
	}
	first := last.Peers[0]
	for _, p := range last.Peers {
		switch {
		case p.Pending:
			return &Divergence{Field: "converge", Where: fmt.Sprintf("peer %d pending", p.Peer), Go: "", JS: ""}
		case p.View != first.View:
			return &Divergence{Field: "converge", Where: fmt.Sprintf("peer %d view vs peer %d", p.Peer, first.Peer), Go: p.View, JS: first.View}
		case p.SV != first.SV:
			return &Divergence{Field: "converge", Where: fmt.Sprintf("peer %d state vector vs peer %d", p.Peer, first.Peer), Go: p.SV, JS: first.SV}
		}
	}
	return nil
}

func show(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
