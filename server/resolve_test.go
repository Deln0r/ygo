package server

import "testing"

// TestMaxConnsPerDoc_Resolver locks the cap resolver's branches so a
// regression that made zero mean unlimited, or mishandled a negative
// (unlimited) value, is caught without a live server.
func TestMaxConnsPerDoc_Resolver(t *testing.T) {
	cases := []struct {
		opt  int
		want int
	}{
		{0, defaultMaxConnsPerDoc}, // zero -> safe default
		{1, 1},                     // smallest meaningful cap
		{5, 5},                     // positive -> passthrough
		{-1, -1},                   // negative -> unlimited sentinel
		{-42, -1},                  // any negative normalizes to -1
	}
	for _, tc := range cases {
		s := &Server{opts: Options{MaxConnsPerDoc: tc.opt}}
		if got := s.maxConnsPerDoc(); got != tc.want {
			t.Errorf("maxConnsPerDoc(opt=%d) = %d, want %d", tc.opt, got, tc.want)
		}
	}
}

// TestDocState_AddConn_Limit covers addConn's cap branches directly: a
// negative limit admits without bound, and a positive limit refuses once
// the room is full. The end-to-end test covers the live WS path; this
// pins the unit contract, including the negative=unlimited branch the
// integration test never exercises.
func TestDocState_AddConn_Limit(t *testing.T) {
	unlimited := &docState{conns: map[*conn]struct{}{}}
	for i := 0; i < 8; i++ {
		if !unlimited.addConn(&conn{}, -1) {
			t.Fatalf("limit=-1 (unlimited) rejected conn %d", i)
		}
	}

	capped := &docState{conns: map[*conn]struct{}{}}
	if !capped.addConn(&conn{}, 1) {
		t.Fatal("first conn under cap 1 was rejected")
	}
	if capped.addConn(&conn{}, 1) {
		t.Fatal("second conn exceeded cap 1")
	}
}
