package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Deln0r/ygo/server"
)

// TestExample_StatsEndpoint verifies the example wires up: the in-memory
// server constructs (running buildWelcomeSeed), the mux serves /stats,
// and it returns a valid Stats snapshot (zero on a fresh server).
func TestExample_StatsEndpoint(t *testing.T) {
	srv, mux := newServer(nil) // nil store = in-memory
	defer srv.Close(context.Background())

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stats status = %d, want 200", resp.StatusCode)
	}
	var st server.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if st.Documents != 0 || st.Connections != 0 {
		t.Errorf("fresh server stats = %+v, want {0 0}", st)
	}
}

// TestExample_WelcomeSeed checks the seed builder returns a non-empty
// update (the OnLoadDocument content applied to every fresh document).
func TestExample_WelcomeSeed(t *testing.T) {
	if len(buildWelcomeSeed()) == 0 {
		t.Fatal("welcome seed is empty")
	}
}
