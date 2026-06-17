package gomobile

import (
	"context"
	"errors"
	"sync"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/persist/sqlite"
)

// errAlreadyConnected guards the one-shot Connect contract.
var errAlreadyConnected = errors.New("gomobile: already connected")

// Listener receives connection and document events from a Client.
// Implement it in Swift / Kotlin and pass to Client.SetListener; all
// methods are called from background goroutines, so dispatch to the
// main thread before touching UI.
type Listener interface {
	// OnSynced fires on synced-state transitions: true after each
	// completed handshake, false on every disconnect.
	OnSynced(synced bool)
	// OnDocChanged fires after any transaction commits on the local
	// doc, local or remote — the "refresh your views" signal.
	OnDocChanged()
	// OnError reports non-fatal connection errors (the client keeps
	// reconnecting).
	OnError(message string)
}

// Client is the gomobile-bindable sync provider: a live WebSocket
// session syncing one Doc with a yserve / Hocuspocus / y-websocket
// server. Construct with NewClient, optionally SetListener, start
// with Connect, stop with Close. The native app edits the Doc through
// Text / Map wrappers; sync happens in the background.
type Client struct {
	d         *Doc
	url       string
	docName   string
	storePath string

	mu             sync.Mutex
	listener       Listener
	inner          *client.Client
	store          *sqlite.Store
	cancelWatch    func()
	presence       PresenceListener
	cancelPresence func()
}

// NewClient prepares a sync client for the document. url is the
// server base ("wss://collab.example.com"); docName addresses the
// document on it.
func NewClient(url, docName string, d *Doc) *Client {
	return &Client{d: d, url: url, docName: docName}
}

// EnableOfflineStore makes the client offline-first, persisting the
// document to a pure-Go on-device SQLite database at dbPath. On the
// next Connect the document loads from that file (usable with no
// network), every change is persisted, and edits made offline sync up
// when a connection is next established. Call before Connect; a no-op
// after. Pass the app's document path, e.g.
// FileManager applicationSupportDirectory on iOS.
func (c *Client) EnableOfflineStore(dbPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inner == nil {
		c.storePath = dbPath
	}
}

// SetListener registers the event listener. Call before Connect;
// calls after Connect are ignored.
func (c *Client) SetListener(l Listener) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inner == nil {
		c.listener = l
	}
}

// Connect starts the background connection loop (handshake, update
// relay, reconnect with exponential backoff). Returns an error when
// already connected or the configuration is invalid.
func (c *Client) Connect() error {
	c.mu.Lock()
	if c.inner != nil {
		c.mu.Unlock()
		return errAlreadyConnected
	}
	l := c.listener
	opts := client.Options{
		URL:     c.url,
		DocName: c.docName,
		Doc:     c.d.inner,
	}
	if l != nil {
		opts.OnSynced = l.OnSynced
		opts.OnError = func(err error) { l.OnError(err.Error()) }
	}
	if c.storePath != "" {
		st, err := sqlite.Open(c.storePath)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		c.store = st
		opts.LocalStore = st
	}
	inner, err := client.New(opts)
	if err != nil {
		if c.store != nil {
			_ = c.store.Close()
			c.store = nil
		}
		c.mu.Unlock()
		return err
	}
	c.inner = inner
	if l != nil {
		c.cancelWatch = c.d.inner.OnAfterTransaction(func(*doc.TransactionMut) {
			l.OnDocChanged()
		})
	}
	if c.presence != nil {
		c.cancelPresence = c.startPresence(inner, c.presence)
	}
	c.mu.Unlock()
	return inner.Connect(context.Background())
}

// Close stops the connection loop and releases the listener hooks.
// Safe to call more than once.
func (c *Client) Close() error {
	c.mu.Lock()
	inner := c.inner
	cancelWatch := c.cancelWatch
	cancelPresence := c.cancelPresence
	store := c.store
	c.inner = nil
	c.cancelWatch = nil
	c.cancelPresence = nil
	c.store = nil
	c.mu.Unlock()
	if cancelWatch != nil {
		cancelWatch()
	}
	if cancelPresence != nil {
		cancelPresence()
	}
	if inner == nil {
		return nil
	}
	// Close the client first (it flushes the local store), then the
	// store itself.
	err := inner.Close()
	if store != nil {
		_ = store.Close()
	}
	return err
}

// Synced reports whether the last handshake completed and the
// connection is up.
func (c *Client) Synced() bool {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	return inner != nil && inner.Synced()
}

// SetAwarenessState sets and broadcasts the local awareness state
// (JSON bytes, e.g. {"name":"ian","cursor":"<base64 rpos>"}).
func (c *Client) SetAwarenessState(jsonState []byte) {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	if inner != nil {
		inner.SetAwarenessState(jsonState)
	}
}

// RemoveAwarenessState clears the local awareness entry (peers see
// this client leave).
func (c *Client) RemoveAwarenessState() {
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	if inner != nil {
		inner.RemoveAwarenessState()
	}
}
