package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestServer_MaxDocs_CapsNewDocs verifies the global document cap: new
// documents are admitted up to the cap, a further distinct docName is
// refused with errServerDocsFull, and an already-resident document is
// always admitted (the cap bounds concurrently-resident rooms, not
// lookups of existing ones).
func TestServer_MaxDocs_CapsNewDocs(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxDocs: 2}}
	ctx := context.Background()

	for _, name := range []string{"a", "b"} {
		if _, _, ok, err := s.admitConn(ctx, name, nil); !ok || err != nil {
			t.Fatalf("admit %q: ok=%v err=%v", name, ok, err)
		}
	}

	// A third distinct doc would exceed the cap.
	_, _, ok, err := s.admitConn(ctx, "c", nil)
	if ok {
		t.Fatal("admitted a new doc past MaxDocs=2")
	}
	if !errors.Is(err, errServerDocsFull) {
		t.Fatalf("err = %v, want errServerDocsFull", err)
	}

	// An already-resident doc is still admitted at the cap.
	if _, _, ok, err := s.admitConn(ctx, "a", nil); !ok || err != nil {
		t.Fatalf("re-admit resident doc: ok=%v err=%v", ok, err)
	}
}

// TestServer_MaxDocs_ZeroUnlimited confirms the zero value imposes no
// cap (the opt-in default), admitting far more distinct docs than any
// positive cap test would.
func TestServer_MaxDocs_ZeroUnlimited(t *testing.T) {
	s := &Server{docs: map[string]*docState{}} // MaxDocs 0 = unlimited
	ctx := context.Background()
	for i := 0; i < 64; i++ {
		if _, _, ok, err := s.admitConn(ctx, fmt.Sprintf("doc-%d", i), nil); !ok || err != nil {
			t.Fatalf("admit doc-%d under unlimited: ok=%v err=%v", i, ok, err)
		}
	}
	s.docsMu.Lock()
	n := len(s.docs)
	s.docsMu.Unlock()
	if n != 64 {
		t.Fatalf("resident docs = %d, want 64", n)
	}
}

// TestServer_MaxDocs_NegativeUnlimited confirms a negative value is also
// treated as unlimited, matching the option's documented semantics.
func TestServer_MaxDocs_NegativeUnlimited(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxDocs: -1}}
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if _, _, ok, err := s.admitConn(ctx, fmt.Sprintf("d%d", i), nil); !ok || err != nil {
			t.Fatalf("admit d%d under MaxDocs=-1: ok=%v err=%v", i, ok, err)
		}
	}
}

// TestServer_MaxDocs_EvictionFreesSlot locks the load-bearing invariant
// that makes MaxDocs safe to express as a concurrently-resident bound: a
// document evicts when its last connection departs, so releasing a
// resident room frees a global slot and a new distinct docName can then
// be admitted at the cap. Without this, the cap would be a lifetime
// ceiling rather than a live count.
func TestServer_MaxDocs_EvictionFreesSlot(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxDocs: 1}}
	ctx := context.Background()

	c, state, ok, err := s.admitConn(ctx, "a", nil)
	if !ok || err != nil {
		t.Fatalf("admit a: ok=%v err=%v", ok, err)
	}

	// At the cap, a new distinct doc is refused.
	if _, _, ok, err := s.admitConn(ctx, "b", nil); ok || !errors.Is(err, errServerDocsFull) {
		t.Fatalf("admit b at cap: ok=%v err=%v, want refused with errServerDocsFull", ok, err)
	}

	// Releasing a's only connection evicts the document and frees the slot.
	s.releaseConn(ctx, state, c)

	if _, _, ok, err := s.admitConn(ctx, "b", nil); !ok || err != nil {
		t.Fatalf("admit b after eviction: ok=%v err=%v, want admitted", ok, err)
	}
}
