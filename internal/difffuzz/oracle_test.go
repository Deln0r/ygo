package difffuzz

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOracle writes a worker script (and the package files StartOracle
// looks for) into a temporary directory.
func fakeOracle(t *testing.T, body string) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	dir := t.TempDir()
	for _, pkg := range []string{"yjs", "lib0"} {
		if err := os.MkdirAll(filepath.Join(dir, "node_modules", pkg), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "node_modules", pkg, "package.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "oracle.mjs")
	src := `import { createInterface } from "node:readline";
const rl = createInterface({ input: process.stdin });
for await (const line of rl) { const req = JSON.parse(line); ` + body + ` }
`
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestOracle_RefusesBadReplies: a reply for another id, of another schema,
// without a trace, or none in time is an error and stops the worker; a
// worker that writes to stderr or exits with a status fails Close.
func TestOracle_RefusesBadReplies(t *testing.T) {
	ok := `process.stdout.write(JSON.stringify({id: req.id, schema: 1, trace: {schema: 1, steps: []}}) + "\n");`
	cases := []struct {
		name, body, want string
		closeFails       bool
	}{
		{"another id", `process.stdout.write(JSON.stringify({id: req.id + 1, schema: 1, trace: {schema: 1, steps: []}}) + "\n");`, "reply for id", false},
		{"another schema", `process.stdout.write(JSON.stringify({id: req.id, schema: 2, trace: {schema: 2, steps: []}}) + "\n");`, "schema", false},
		{"no trace", `process.stdout.write(JSON.stringify({id: req.id, schema: 1}) + "\n");`, "without a trace", false},
		{"no reply", ``, "took more than", false},
		{"stderr", `process.stderr.write("boom\n"); ` + ok, "", true},
		{"exit status", ok + ` process.exitCode = 3;`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := StartOracle(fakeOracle(t, c.body))
			if err != nil {
				t.Fatal(err)
			}
			// A worker that answers gets time to start under a loaded CI
			// (race detector and coverage on every package at once); the
			// one that never answers is the case the deadline is for.
			timeout := 30 * time.Second
			if c.name == "no reply" {
				timeout = 2 * time.Second
			}
			_, err = o.Run(Generate(1), timeout)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("Run: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("Run: %v, want an error with %q", err, c.want)
			}
			cerr := o.Close()
			if c.closeFails != (cerr != nil) {
				t.Fatalf("Close: %v, want failure %v", cerr, c.closeFails)
			}
		})
	}
	// A worker that never reads: a request larger than the pipe's buffer
	// blocks the write, and the deadline still holds.
	t.Run("never reads", func(t *testing.T) {
		script := fakeOracle(t, "")
		src := "setInterval(() => {}, 1000);\n"
		if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		o, err := StartOracle(script)
		if err != nil {
			t.Fatal(err)
		}
		big := Generate(1)
		big.Steps = append([]Step{{K: StepTxn, Ops: []Op{{Root: "t", Want: WantText, O: OpIns, S: strings.Repeat("a", 1<<20)}}}}, big.Steps...)
		start := time.Now()
		if _, err := o.Run(big, time.Second); err == nil || !strings.Contains(err.Error(), "took more than") {
			t.Fatalf("Run: %v, want a timeout", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("Run returned after %v", d)
		}
		_ = o.Close()
	})
	if _, err := StartOracle(filepath.Join(t.TempDir(), "missing.mjs")); !errors.Is(err, ErrNoOracle) {
		t.Fatalf("a missing script: %v, want ErrNoOracle", err)
	}
}
