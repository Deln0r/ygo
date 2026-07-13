package main

import (
	"net"
	"testing"

	"github.com/Deln0r/ygo/persist/sqlite"
)

// deadURL returns a ws:// URL nothing listens on (bound then freed), so
// the client exercises its offline path with no server reachable.
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "ws://" + ln.Addr().String()
	_ = ln.Close()
	return url
}

// TestOfflineFirst_PersistsAcrossRestarts runs the example's core twice
// against the same store with no server reachable: the second session
// loads the first session's entry from disk and appends to it, proving
// offline-first persistence with no network involved.
func TestOfflineFirst_PersistsAcrossRestarts(t *testing.T) {
	path := t.TempDir() + "/notes.db"
	url := deadURL(t)

	// Session 1 (offline): append "first".
	st1, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got1, err := appendAndList(st1, url, "notes", "first")
	if err != nil {
		t.Fatal(err)
	}
	_ = st1.Close()
	if len(got1) != 1 || got1[0] != "first" {
		t.Fatalf("session 1 log = %v, want [first]", got1)
	}

	// Session 2 (offline, same store): append "second"; the first entry
	// must be loaded from disk, so the log holds both in order.
	st2, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	got2, err := appendAndList(st2, url, "notes", "second")
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 2 || got2[0] != "first" || got2[1] != "second" {
		t.Fatalf("session 2 log = %v, want [first second]", got2)
	}
}
