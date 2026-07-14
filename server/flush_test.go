package server_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/server"
)

// recordingStore is a persist.Store that records the docName of each Flush
// and returns a preset result, so a test can assert Server.Flush delegates
// to Store.Flush and propagates its outcome. The other methods are inert.
type recordingStore struct {
	mu       sync.Mutex
	flushed  []string
	flushErr error
}

func (r *recordingStore) StoreUpdate(context.Context, string, []byte) error    { return nil }
func (r *recordingStore) GetUpdates(context.Context, string) ([][]byte, error) { return nil, nil }
func (r *recordingStore) Flush(_ context.Context, docName string) error {
	r.mu.Lock()
	r.flushed = append(r.flushed, docName)
	err := r.flushErr
	r.mu.Unlock()
	return err
}
func (r *recordingStore) DocumentExists(context.Context, string) (bool, error) { return false, nil }
func (r *recordingStore) ListDocuments(context.Context) ([]string, error)      { return nil, nil }
func (r *recordingStore) ClearDocument(context.Context, string) error          { return nil }
func (r *recordingStore) Close() error                                         { return nil }

// TestServer_Flush_NoStore: with no Store configured Flush is a no-op that
// returns nil, matching Close's silent skip.
func TestServer_Flush_NoStore(t *testing.T) {
	srv := server.New(server.Options{})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })

	if err := srv.Flush(context.Background(), "anything"); err != nil {
		t.Fatalf("Flush with no Store = %v, want nil", err)
	}
}

// TestServer_Flush_DelegatesToStore: Server.Flush forwards the docName to
// Store.Flush and returns its result unchanged. Compaction correctness
// itself lives in the persist package's Flush tests; this pins the wiring.
func TestServer_Flush_DelegatesToStore(t *testing.T) {
	rec := &recordingStore{}
	srv := server.New(server.Options{Store: rec})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })

	if err := srv.Flush(context.Background(), "doc-a"); err != nil {
		t.Fatalf("Flush = %v, want nil", err)
	}
	rec.mu.Lock()
	got := append([]string(nil), rec.flushed...)
	rec.mu.Unlock()
	if len(got) != 1 || got[0] != "doc-a" {
		t.Fatalf("Store.Flush calls = %v, want [doc-a]", got)
	}

	// A store error propagates unchanged.
	sentinel := errors.New("store flush failed")
	rec.mu.Lock()
	rec.flushErr = sentinel
	rec.mu.Unlock()
	if err := srv.Flush(context.Background(), "doc-b"); !errors.Is(err, sentinel) {
		t.Fatalf("Flush error = %v, want %v", err, sentinel)
	}
}
