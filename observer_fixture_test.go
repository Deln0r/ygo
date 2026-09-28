package ygo_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// The fixtures in testdata/observer-fixtures.json record which observers yjs
// 13.6.33 calls for a transaction, in what order, and with which events (kind
// and path only). The same edits are replayed here and the call logs compared.

type observerFixtureFile struct {
	Generator string             `json:"generator"`
	Scenarios []observerScenario `json:"scenarios"`
}

type observerScenario struct {
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	Remote           bool           `json:"remote"`
	Setup            []observerOp   `json:"setup"`
	Observers        []observerSpec `json:"observers"`
	AfterTransaction bool           `json:"after_transaction"`
	Txns             [][]observerOp `json:"txns"`
	ExpectedCalls    []string       `json:"expected_calls"`
}

type observerOp struct {
	Op    string `json:"op"`
	Path  []any  `json:"path"`
	Key   string `json:"key"`
	Index uint64 `json:"index"`
	Value any    `json:"value"`
	Text  string `json:"text"`
}

type observerSpec struct {
	Kind string `json:"kind"`
	Path []any  `json:"path"`
	Name string `json:"name"`
}

// knownObserverDivergences pins the scenarios where ygo does not yet call its
// observers the way yjs does, with the exact call log ygo produces today. A
// scenario that starts to match, or diverges in another way, fails the test.
var knownObserverDivergences = map[string]struct {
	reason string
	calls  []string
}{
	"grandchild-written-then-child-deleted": {
		reason: "deleting a nested type does not delete the types inside it (txn.Delete has no recursive delete), so the grandchild still reports its change",
		calls:  []string{"observe grand", "deep root [map[] map[child grand]]"},
	},
}

func TestObservers_Fixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "observer-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f observerFixtureFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no observer scenarios; the fixture file is empty or its shape changed")
	}
	seen := map[string]bool{}
	for _, sc := range f.Scenarios {
		seen[sc.Name] = true
		t.Run(sc.Name, func(t *testing.T) {
			got, err := replayObservers(sc)
			if err != nil {
				t.Fatalf("%s: %v", sc.Description, err)
			}
			known, isKnown := knownObserverDivergences[sc.Name]
			matches := strings.Join(got, "\n") == strings.Join(sc.ExpectedCalls, "\n")
			switch {
			case matches && isKnown:
				t.Errorf("now matches yjs; remove it from knownObserverDivergences (listed as: %s)", known.reason)
			case !matches && !isKnown:
				t.Errorf("%s\n ygo: %q\n yjs: %q", sc.Description, got, sc.ExpectedCalls)
			case !matches && strings.Join(got, "\n") != strings.Join(known.calls, "\n"):
				t.Errorf("known divergence (%s) now diverges differently\n ygo:    %q\n pinned: %q", known.reason, got, known.calls)
			}
		})
	}
	for name := range knownObserverDivergences {
		if !seen[name] {
			t.Errorf("knownObserverDivergences names %q, which is not a scenario", name)
		}
	}
}

// replayObservers runs a scenario through ygo and returns the call log. A
// remote scenario makes its edits on a second document (client 2) and
// applies each transaction to the observed one as an update.
func replayObservers(sc observerScenario) ([]string, error) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	roots := observerRoots(d, sc)
	var src *ygo.Doc
	var srcRoots map[string]*ygo.Map
	if sc.Remote {
		src = ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
		srcRoots = observerRoots(src, sc)
		if err := runObserverOps(src, srcRoots, sc.Setup); err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		if err := ygo.ApplyUpdate(d, ygo.EncodeStateAsUpdate(src)); err != nil {
			return nil, fmt.Errorf("setup update: %w", err)
		}
	} else if err := runObserverOps(d, roots, sc.Setup); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	var calls []string
	for _, o := range sc.Observers {
		target, err := resolveObserverPath(roots, o.Path)
		if err != nil {
			return nil, err
		}
		name := o.Name
		switch o.Kind {
		case "observe":
			log := func() { calls = append(calls, "observe "+name) }
			switch v := target.(type) {
			case *ygo.Map:
				v.Observe(func(*ygo.MapEvent) { log() })
			case *ygo.Array:
				v.Observe(func(*ygo.ArrayEvent) { log() })
			case *ygo.Text:
				v.Observe(func(*ygo.TextEvent) { log() })
			default:
				return nil, fmt.Errorf("cannot observe %T", target)
			}
		case "deep":
			fn := func(events []any) {
				parts := make([]string, 0, len(events))
				for _, e := range events {
					parts = append(parts, observedEventString(e))
				}
				calls = append(calls, fmt.Sprintf("deep %s [%s]", name, strings.Join(parts, " ")))
			}
			switch v := target.(type) {
			case *ygo.Map:
				v.ObserveDeep(fn)
			case *ygo.Array:
				v.ObserveDeep(fn)
			default:
				return nil, fmt.Errorf("no deep observer on %T", target)
			}
		default:
			return nil, fmt.Errorf("unknown observer kind %q", o.Kind)
		}
	}
	if sc.AfterTransaction {
		d.OnAfterTransaction(func(*ygo.TransactionMut) { calls = append(calls, "afterTransaction") })
	}
	for i, ops := range sc.Txns {
		if !sc.Remote {
			if err := runObserverOps(d, roots, ops); err != nil {
				return nil, fmt.Errorf("transaction %d: %w", i, err)
			}
			continue
		}
		if err := runObserverOps(src, srcRoots, ops); err != nil {
			return nil, fmt.Errorf("transaction %d: %w", i, err)
		}
		diff, err := ygo.EncodeDiff(src, ygo.EncodeStateVector(d))
		if err != nil {
			return nil, fmt.Errorf("transaction %d diff: %w", i, err)
		}
		if err := ygo.ApplyUpdate(d, diff); err != nil {
			return nil, fmt.Errorf("transaction %d update: %w", i, err)
		}
	}
	return calls, nil
}

// observerRoots creates every root map the scenario names, up front:
// ygo.NewMap takes the document lock, which an open write transaction
// already holds.
func observerRoots(d *ygo.Doc, sc observerScenario) map[string]*ygo.Map {
	roots := map[string]*ygo.Map{}
	add := func(path []any) {
		if name, ok := path[0].(string); ok && roots[name] == nil {
			roots[name] = ygo.NewMap(d, name)
		}
	}
	for _, op := range sc.Setup {
		add(op.Path)
	}
	for _, o := range sc.Observers {
		add(o.Path)
	}
	for _, ops := range sc.Txns {
		for _, op := range ops {
			add(op.Path)
		}
	}
	return roots
}

func observedEventString(e any) string {
	var kind string
	var path []any
	switch ev := e.(type) {
	case *ygo.MapEvent:
		kind, path = "map", ev.Path
	case *ygo.ArrayEvent:
		kind, path = "array", ev.Path
	case *ygo.TextEvent:
		kind, path = "text", ev.Path
	default:
		kind = fmt.Sprintf("%T", e)
	}
	segs := make([]string, len(path))
	for i, p := range path {
		segs[i] = fmt.Sprint(p)
	}
	return kind + "[" + strings.Join(segs, " ") + "]"
}

// runObserverOps applies ops in one transaction.
func runObserverOps(d *ygo.Doc, roots map[string]*ygo.Map, ops []observerOp) error {
	txn := d.WriteTxn()
	defer txn.Commit()
	for j, op := range ops {
		target, err := resolveObserverPath(roots, op.Path)
		if err != nil {
			return fmt.Errorf("op %d: %w", j, err)
		}
		if err := applyObserverOp(txn, target, op); err != nil {
			return fmt.Errorf("op %d (%s): %w", j, op.Op, err)
		}
	}
	return nil
}

func applyObserverOp(txn *ygo.TransactionMut, target any, op observerOp) error {
	switch v := target.(type) {
	case *ygo.Map:
		switch op.Op {
		case "setMap":
			v.SetMap(txn, op.Key)
		case "setArray":
			v.SetArray(txn, op.Key)
		case "setText":
			v.SetText(txn, op.Key)
		case "set":
			v.Set(txn, op.Key, op.Value)
		case "delete":
			v.Delete(txn, op.Key)
		default:
			return fmt.Errorf("%s on a map", op.Op)
		}
	case *ygo.Array:
		switch op.Op {
		case "insertMap":
			v.InsertMap(txn, op.Index)
		case "push":
			v.Push(txn, op.Value)
		case "insert":
			v.Insert(txn, op.Index, op.Value)
		default:
			return fmt.Errorf("%s on an array", op.Op)
		}
	case *ygo.Text:
		if op.Op != "textInsert" {
			return fmt.Errorf("%s on a text", op.Op)
		}
		return v.Insert(txn, op.Index, op.Text)
	default:
		return fmt.Errorf("%s on %T", op.Op, target)
	}
	return nil
}

// resolveObserverPath walks a root map name, then map keys and array indices.
func resolveObserverPath(roots map[string]*ygo.Map, path []any) (any, error) {
	name, _ := path[0].(string)
	root := roots[name]
	if root == nil {
		return nil, fmt.Errorf("path %v does not start with a known root name", path)
	}
	var cur any = root
	for _, seg := range path[1:] {
		switch c := cur.(type) {
		case *ygo.Map:
			key, ok := seg.(string)
			if !ok {
				return nil, fmt.Errorf("map segment %v in %v is not a key", seg, path)
			}
			cur = c.Get(key)
		case *ygo.Array:
			idx, ok := seg.(float64)
			if !ok {
				return nil, fmt.Errorf("array segment %v in %v is not an index", seg, path)
			}
			cur = c.Get(uint64(idx))
		default:
			return nil, fmt.Errorf("cannot descend into %T at %v", cur, path)
		}
		if cur == nil {
			return nil, fmt.Errorf("nothing at %v", path)
		}
	}
	return cur, nil
}
