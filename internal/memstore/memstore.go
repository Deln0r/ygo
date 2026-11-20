// Package memstore is an in-memory persist.VersionedStore with the
// behaviour of persist/sqlite: a document's updates in the order they were
// stored, compacted into one by Flush once there are two or more, versions
// numbered across documents, created at a whole second. It lets the core
// module's tests run a server, a client or the persist helpers on a store
// without SQLite, which lives in a module of its own.
//
// Open(path) keeps one state per path for the life of the process, so a
// test that closes a store and opens the same path again sees what it
// wrote, as with a SQLite file; ":memory:" and "" open a store of their
// own each time, as SQLite opens a private in-memory database for them.
package memstore

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Deln0r/ygo/persist"
)

// ErrClosed is returned by a Store's methods after Close.
var ErrClosed = errors.New("memstore: store is closed")

type version struct {
	id      int64
	label   string
	created time.Time
	state   []byte
}

type state struct {
	mu       sync.Mutex
	updates  map[string][][]byte
	versions map[string][]version
	nextID   int64
}

func newState() *state {
	return &state{updates: map[string][][]byte{}, versions: map[string][]version{}}
}

var (
	registryMu sync.Mutex
	registry   = map[string]*state{}
)

// Store is an in-memory persist.VersionedStore.
type Store struct {
	st       *state
	closedMu sync.Mutex
	closed   bool
}

var _ persist.VersionedStore = (*Store)(nil)

// New returns an empty store of its own.
func New() *Store { return &Store{st: newState()} }

// Open returns a store over the state kept for path in this process,
// created empty on the first Open.
func Open(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return New(), nil
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	st := registry[path]
	if st == nil {
		st = newState()
		registry[path] = st
	}
	return &Store{st: st}, nil
}

func (s *Store) lock() (*state, error) {
	s.closedMu.Lock()
	closed := s.closed
	s.closedMu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	s.st.mu.Lock()
	return s.st, nil
}

// StoreUpdate appends a copy of update to docName's log.
func (s *Store) StoreUpdate(_ context.Context, docName string, update []byte) error {
	if len(update) == 0 {
		return persist.ErrEmptyUpdate
	}
	st, err := s.lock()
	if err != nil {
		return err
	}
	defer st.mu.Unlock()
	st.updates[docName] = append(st.updates[docName], bytes.Clone(update))
	return nil
}

// GetUpdates returns copies of docName's updates in the order stored.
func (s *Store) GetUpdates(_ context.Context, docName string) ([][]byte, error) {
	st, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer st.mu.Unlock()
	var out [][]byte
	for _, u := range st.updates[docName] {
		out = append(out, bytes.Clone(u))
	}
	return out, nil
}

// Flush replaces docName's updates with their merge once there are two or
// more, as persist/sqlite does.
func (s *Store) Flush(_ context.Context, docName string) error {
	st, err := s.lock()
	if err != nil {
		return err
	}
	defer st.mu.Unlock()
	updates := st.updates[docName]
	if len(updates) < 2 {
		return nil
	}
	snapshot, err := persist.MergeUpdates(updates)
	if err != nil {
		return err
	}
	st.updates[docName] = [][]byte{snapshot}
	return nil
}

// DocumentExists reports whether docName has an update.
func (s *Store) DocumentExists(_ context.Context, docName string) (bool, error) {
	st, err := s.lock()
	if err != nil {
		return false, err
	}
	defer st.mu.Unlock()
	return len(st.updates[docName]) > 0, nil
}

// ListDocuments returns the names of the documents that have an update,
// sorted.
func (s *Store) ListDocuments(_ context.Context) ([]string, error) {
	st, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer st.mu.Unlock()
	var out []string
	for name, us := range st.updates {
		if len(us) > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// ClearDocument drops docName's updates; its versions stay.
func (s *Store) ClearDocument(_ context.Context, docName string) error {
	st, err := s.lock()
	if err != nil {
		return err
	}
	defer st.mu.Unlock()
	delete(st.updates, docName)
	return nil
}

// Close ends this handle; the state stays for another Open of its path.
func (s *Store) Close() error {
	s.closedMu.Lock()
	defer s.closedMu.Unlock()
	s.closed = true
	return nil
}

// SaveVersion stores a copy of state as a version of docName.
func (s *Store) SaveVersion(_ context.Context, docName, label string, state []byte) (int64, error) {
	if len(state) == 0 {
		return 0, persist.ErrEmptyUpdate
	}
	st, err := s.lock()
	if err != nil {
		return 0, err
	}
	defer st.mu.Unlock()
	st.nextID++
	st.versions[docName] = append(st.versions[docName], version{
		id: st.nextID, label: label, created: time.Now().UTC().Truncate(time.Second), state: bytes.Clone(state),
	})
	return st.nextID, nil
}

// ListVersions returns docName's versions, oldest first.
func (s *Store) ListVersions(_ context.Context, docName string) ([]persist.VersionInfo, error) {
	st, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer st.mu.Unlock()
	var out []persist.VersionInfo
	for _, v := range st.versions[docName] {
		out = append(out, persist.VersionInfo{ID: v.id, Label: v.label, CreatedAt: v.created, Size: int64(len(v.state))})
	}
	return out, nil
}

func (st *state) find(docName string, id int64) (int, bool) {
	for i, v := range st.versions[docName] {
		if v.id == id {
			return i, true
		}
	}
	return 0, false
}

// GetVersionState returns a copy of a version's state.
func (s *Store) GetVersionState(_ context.Context, docName string, versionID int64) ([]byte, error) {
	st, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer st.mu.Unlock()
	i, ok := st.find(docName, versionID)
	if !ok {
		return nil, persist.ErrVersionNotFound
	}
	return bytes.Clone(st.versions[docName][i].state), nil
}

// DeleteVersion drops a version; an unknown one is not an error.
func (s *Store) DeleteVersion(_ context.Context, docName string, versionID int64) error {
	st, err := s.lock()
	if err != nil {
		return err
	}
	defer st.mu.Unlock()
	if i, ok := st.find(docName, versionID); ok {
		st.versions[docName] = slices.Delete(st.versions[docName], i, i+1)
	}
	return nil
}

// RestoreVersion replaces docName's updates with the version's state.
func (s *Store) RestoreVersion(_ context.Context, docName string, versionID int64) error {
	st, err := s.lock()
	if err != nil {
		return err
	}
	defer st.mu.Unlock()
	i, ok := st.find(docName, versionID)
	if !ok {
		return persist.ErrVersionNotFound
	}
	st.updates[docName] = [][]byte{bytes.Clone(st.versions[docName][i].state)}
	return nil
}
