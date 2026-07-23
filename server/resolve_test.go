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

// TestForeignAwarenessCap_Resolver locks resolveForeignAwarenessCap's branches:
// the 1/8 reservation default, the negative-disables case, clamping above the
// total cap, and the no-op when the total presence cap is unlimited. maxOpt is
// MaxAwarenessClients after its own zero-default is applied (so 0 never reaches
// here; negative still means unlimited).
func TestForeignAwarenessCap_Resolver(t *testing.T) {
	cases := []struct {
		maxOpt, foreignOpt, want int
		note                     string
	}{
		{4096, 0, 3584, "default reserves 1/8 (512) for local"},
		{16, 0, 14, "default reserves 1/8 (2) for local"},
		{100, 0, 88, "default reserves 1/8 (12) for local"},
		{8, 0, 7, "1/8 rounds to 1 at the boundary; floor 1"},
		{7, 0, 6, "1/8 rounds to 0 for a small cap; still reserve one local slot"},
		{2, 0, 1, "smallest cap that can spare a slot; floor 1"},
		{1, 0, 0, "single-slot cap cannot reserve; floor 0"},
		{4096, -1, 0, "negative disables the partition"},
		{4096, 40, 40, "positive passthrough below the cap"},
		{100, 250, 100, "positive above the cap clamps to it (no reservation)"},
		{-1, 0, 0, "unlimited total, zero default -> no foreign sub-cap"},
		{-1, 50, 50, "positive value is honored even under an unlimited total"},
	}
	for _, tc := range cases {
		if got := resolveForeignAwarenessCap(tc.maxOpt, tc.foreignOpt); got != tc.want {
			t.Errorf("resolveForeignAwarenessCap(max=%d, foreign=%d) = %d, want %d (%s)",
				tc.maxOpt, tc.foreignOpt, got, tc.want, tc.note)
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
