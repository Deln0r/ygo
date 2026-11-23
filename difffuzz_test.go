package ygo_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/difffuzz"
)

// The differential fuzzer (internal/difffuzz): seeded scenarios of several
// peers editing, undoing and exchanging updates, replayed through ygo and
// through yjs (testdata/gen/difffuzz-oracle.mjs), compared step by step:
// every update event byte for byte, what a sync, merge or diff sends, and
// each touched peer's state, state vector, pending queue, view and undo
// stacks.
//
//	YGO_DIFFFUZZ_SEEDS=n     seeds 1..n (default 50)
//	YGO_DIFFFUZZ_SEED=s      one seed
//	YGO_DIFFFUZZ_FROM=f      the first seed (default 1)
//	YGO_DIFFFUZZ_REQUIRE=1   fail, not skip, without node or the oracle
//	YGO_DIFFFUZZ_OUT=dir     write each shrunk failing scenario there

const oracleScript = "testdata/gen/difffuzz-oracle.mjs"

func startOracle(t *testing.T) *difffuzz.Oracle {
	t.Helper()
	o, err := difffuzz.StartOracle(oracleScript)
	if errors.Is(err, difffuzz.ErrNoOracle) && os.Getenv("YGO_DIFFFUZZ_REQUIRE") != "1" {
		t.Skip("node or the yjs oracle (npm install in testdata/gen) is not available")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Error(err)
		}
	})
	return o
}

func envInt(name string, def uint64) uint64 {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err == nil {
			return n
		}
	}
	return def
}

func TestDifferential_Seeds(t *testing.T) {
	o := startOracle(t)
	from, n := envInt("YGO_DIFFFUZZ_FROM", 1), envInt("YGO_DIFFFUZZ_SEEDS", 50)
	if s := os.Getenv("YGO_DIFFFUZZ_SEED"); s != "" {
		from, n = envInt("YGO_DIFFFUZZ_SEED", 1), 1
	}
	check := func(s difffuzz.Scenario) (*difffuzz.Divergence, error) {
		js, err := o.Run(s, time.Minute)
		if err != nil {
			return nil, err
		}
		return difffuzz.Compare(s, difffuzz.Run(s, difffuzz.RunOptions{}), js), nil
	}
	failed := 0
	save := func(seed uint64, suffix string, sc difffuzz.Scenario, d *difffuzz.Divergence) {
		dir := os.Getenv("YGO_DIFFFUZZ_OUT")
		if dir == "" {
			return
		}
		b, err := json.MarshalIndent(map[string]any{"note": fmt.Sprintf("seed %d: %v", seed, d), "scenario": sc}, "", " ")
		if err == nil {
			err = os.MkdirAll(dir, 0o755)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("seed-%d%s.json", seed, suffix)), append(b, '\n'), 0o644)
		}
		if err != nil {
			t.Errorf("seed %d: saving the scenario: %v", seed, err)
		}
	}
	for seed := from; seed < from+n; seed++ {
		s := difffuzz.Generate(seed)
		d, err := check(s)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if d == nil {
			continue
		}
		// The original first: shrinking needs the oracle again and may fail.
		save(seed, "", s, d)
		var shrinkErr error
		small := difffuzz.Shrink(s, d.Class(), 400, func(c difffuzz.Scenario) *difffuzz.Divergence {
			if shrinkErr != nil {
				return nil
			}
			d, err := check(c)
			if err != nil {
				shrinkErr = err
				return nil
			}
			return d
		})
		if shrinkErr != nil {
			raw, _ := json.Marshal(s)
			t.Fatalf("seed %d diverges: %v\nshrinking failed: %v\n%s", seed, d, shrinkErr, raw)
		}
		sd, err := check(small)
		if err != nil {
			t.Fatalf("seed %d: rechecking the shrunk scenario: %v", seed, err)
		}
		save(seed, "-shrunk", small, sd)
		b, _ := json.Marshal(small)
		t.Errorf("seed %d diverges: %v\nshrunk to %d steps: %v\n%s", seed, d, len(small.Steps), sd, b)
		if failed++; failed >= 3 {
			t.Fatal("stopping after 3 diverging seeds")
		}
	}
}

// TestDifferential_Corpus: the scenarios in testdata/difffuzz (shrunk
// reproducers of divergences the fuzzer found) replay in ygo to the trace
// yjs gives them (testdata/difffuzz-fixtures.json, gen-difffuzz.mjs),
// without Node.
func TestDifferential_Corpus(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "difffuzz-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Scenarios []struct {
			Name     string            `json:"name"`
			Scenario difffuzz.Scenario `json:"scenario"`
			Trace    difffuzz.Trace    `json:"trace"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "difffuzz", "*.json"))
	if len(fx.Scenarios) == 0 || len(fx.Scenarios) != len(files) {
		t.Fatalf("%d fixtures for %d corpus files: regenerate with gen-difffuzz.mjs", len(fx.Scenarios), len(files))
	}
	for _, sc := range fx.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if d := difffuzz.Compare(sc.Scenario, difffuzz.Run(sc.Scenario, difffuzz.RunOptions{}), sc.Trace); d != nil {
				t.Fatal(d)
			}
		})
	}
}
