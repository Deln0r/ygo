package difffuzz

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ErrNoOracle is returned by StartOracle when node, the worker script or its
// dependencies are not there; anything that goes wrong after that is a
// failure, not an absence.
var ErrNoOracle = errors.New("difffuzz: node or the yjs oracle is not available")

// Oracle is a running yjs worker (testdata/gen/difffuzz-oracle.mjs).
type Oracle struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	lines   chan []byte
	readErr chan error
	stderr  bytes.Buffer
	next    int
	dead    bool
}

// StartOracle starts the worker script (its directory must hold
// node_modules with yjs and lib0).
func StartOracle(script string) (*Oracle, error) {
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, ErrNoOracle
	}
	script, err = filepath.Abs(script)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(script)
	for _, f := range []string{script, filepath.Join(dir, "node_modules", "yjs", "package.json"), filepath.Join(dir, "node_modules", "lib0", "package.json")} {
		if _, err := os.Stat(f); err != nil {
			return nil, ErrNoOracle
		}
	}
	o := &Oracle{lines: make(chan []byte, 1), readErr: make(chan error, 1)}
	// Node 26 warns that localStorage needs a file; any other output on
	// stderr fails Close.
	o.cmd = exec.Command(node, "--disable-warning=ExperimentalWarning", script)
	o.cmd.Dir = dir
	o.cmd.Stderr = &o.stderr
	if o.in, err = o.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := o.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := o.cmd.Start(); err != nil {
		return nil, fmt.Errorf("difffuzz: start the oracle: %w", err)
	}
	go func() {
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				o.lines <- line
			}
			if err != nil {
				o.readErr <- err
				close(o.lines)
				return
			}
		}
	}()
	return o, nil
}

type request struct {
	ID       int      `json:"id"`
	Scenario Scenario `json:"scenario"`
}

type reply struct {
	ID     *int   `json:"id"`
	Schema int    `json:"schema"`
	Trace  *Trace `json:"trace"`
	Error  string `json:"error"`
}

// Run replays s in yjs. Every reply must carry the request's id and the
// schema version, and come within timeout; after a failure the worker is
// killed and every later call fails.
func (o *Oracle) Run(s Scenario, timeout time.Duration) (Trace, error) {
	if o.dead {
		return Trace{}, errors.New("difffuzz: the oracle stopped earlier")
	}
	o.next++
	id := o.next
	b, err := json.Marshal(request{ID: id, Scenario: s})
	if err != nil {
		return Trace{}, err
	}
	if _, err := o.in.Write(append(b, '\n')); err != nil {
		return Trace{}, o.fail(fmt.Errorf("difffuzz: write to the oracle: %w", err))
	}
	select {
	case line, ok := <-o.lines:
		if !ok {
			return Trace{}, o.fail(fmt.Errorf("difffuzz: the oracle closed its output: %v", <-o.readErr))
		}
		var rep reply
		if err := json.Unmarshal(line, &rep); err != nil {
			return Trace{}, o.fail(fmt.Errorf("difffuzz: oracle reply: %w", err))
		}
		switch {
		case rep.ID == nil || *rep.ID != id:
			return Trace{}, o.fail(fmt.Errorf("difffuzz: oracle reply for id %v, want %d", rep.ID, id))
		case rep.Schema != Schema:
			return Trace{}, o.fail(fmt.Errorf("difffuzz: oracle schema %d, want %d", rep.Schema, Schema))
		case rep.Error != "":
			return Trace{}, fmt.Errorf("difffuzz: the oracle failed on seed %d: %s", s.Seed, rep.Error)
		case rep.Trace == nil || rep.Trace.Schema != Schema:
			return Trace{}, o.fail(fmt.Errorf("difffuzz: oracle reply without a trace of schema %d", Schema))
		}
		return *rep.Trace, nil
	case <-time.After(timeout):
		return Trace{}, o.fail(fmt.Errorf("difffuzz: the oracle took more than %v on seed %d", timeout, s.Seed))
	}
}

func (o *Oracle) fail(err error) error {
	o.dead = true
	_ = o.cmd.Process.Kill()
	_ = o.cmd.Wait()
	if o.stderr.Len() > 0 {
		return fmt.Errorf("%w (oracle stderr: %s)", err, o.stderr.String())
	}
	return err
}

// Close ends the worker; it must exit with status 0 and nothing on stderr.
func (o *Oracle) Close() error {
	if o.dead {
		return nil
	}
	o.dead = true
	_ = o.in.Close()
	done := make(chan error, 1)
	go func() { done <- o.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("difffuzz: the oracle exited: %w (stderr: %s)", err, o.stderr.String())
		}
		if o.stderr.Len() > 0 {
			return fmt.Errorf("difffuzz: the oracle wrote to stderr: %s", o.stderr.String())
		}
		return nil
	case <-time.After(30 * time.Second):
		_ = o.cmd.Process.Kill()
		return errors.New("difffuzz: the oracle did not exit")
	}
}
