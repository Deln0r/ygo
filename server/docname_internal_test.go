package server

import (
	"net/http/httptest"
	"testing"
)

// TestDefaultDocName pins how the default DocNameFn maps a request to a
// document. Every segment below the mount point is kept: before v1.32.0
// the first of several segments was dropped, so "team-a/doc" and
// "team-b/doc" were one document.
func TestDefaultDocName(t *testing.T) {
	cases := []struct {
		pattern, path, want string
	}{
		// The handler used directly (no ServeMux pattern).
		{"", "/room", "room"},
		{"", "/team-a/doc", "team-a/doc"},
		{"", "/a/b/c", "a/b/c"},
		{"", "/", ""},
		// Mounted at the root of a ServeMux.
		{"/", "/room", "room"},
		{"/", "/team-a/doc", "team-a/doc"},
		// Mounted under a prefix: the prefix is not part of the name.
		{"/collab/", "/collab/room1", "room1"},
		{"/collab/", "/collab/team-a/doc", "team-a/doc"},
		{"/collab/", "/collab/", ""},
		{"/api/collab/", "/api/collab/room", "room"},
		// Patterns with a method, a host or wildcards.
		{"GET /collab/", "/collab/room", "room"},
		{"GET example.com/collab/{doc...}", "/collab/a/b", "a/b"},
		{"example.com/collab/", "/collab/x", "x"},
		{"/collab/{doc}", "/collab/x", "x"},
		{"/collab/{$}", "/collab/", ""},
		// Exact patterns keep the last segment as the name, as before.
		{"/ws", "/ws", "ws"},
		{"/ws/doc", "/ws/doc", "doc"},
		// An escaped pattern is compared with the unescaped path.
		{"/col%20lab/", "/col%20lab/room", "room"},
		// A path that does not start with the pattern (http.StripPrefix
		// in between) falls back to the whole path.
		{"/collab/", "/room", "room"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://example.com"+c.path, nil)
		r.Pattern = c.pattern
		if got := defaultDocName(r); got != c.want {
			t.Errorf("pattern %q path %q: docName %q, want %q", c.pattern, c.path, got, c.want)
		}
	}
}
