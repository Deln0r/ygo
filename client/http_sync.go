package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/store"
)

// HTTPSync syncs a document with a ygo server's HTTPSyncHandler by plain
// requests, for clients that cannot keep a WebSocket open: each Sync pulls
// what the server has that the document lacks, then pushes what the
// document has that the server lacks.
//
// It watches the document for local changes from the first Sync on (Close
// stops that), so it pushes only when there is something to push: a local
// change since the last push, a pending buffer other than the one last
// pushed (updates that wait for something, which no change event reports),
// or structs the server's state vector does not cover. It keeps the ETag of
// the last response it applied and sends it, so a pull of an unchanged
// document costs a 304. A Sync with another document, or after URL changed,
// starts afresh. An HTTPSync is safe for concurrent use; Syncs run one at a
// time.
type HTTPSync struct {
	// URL is the document's URL: where HTTPSyncHandler is mounted, with the
	// document's name.
	URL string
	// Client sends the requests; nil is http.DefaultClient.
	Client *http.Client
	// Header is added to every request (an Authorization header, say).
	Header http.Header
	// V2 pulls V2 updates (POST bodies are V1).
	V2 bool

	mu       sync.Mutex
	doc      *doc.Doc
	syncURL  string
	unsub    func()
	etag     string
	serverSV store.StateVector
	// pushedPending is the fingerprint of the pending buffer the last push
	// carried (encoding.PendingFingerprint).
	pushedPending []byte
	// current is the origin of the attached document's sync transactions.
	current atomic.Pointer[HTTPSyncOrigin]
	dirty   atomic.Bool
	applied atomic.Int64
}

// HTTPSyncOrigin is the origin of the transactions HTTPSync applies the
// server's updates in.
type HTTPSyncOrigin struct{ URL string }

// errHTTPStatus is an unexpected response.
type errHTTPStatus struct {
	status int
	body   string
}

func (e *errHTTPStatus) Error() string {
	return fmt.Sprintf("client: HTTP sync: %d %s: %s", e.status, http.StatusText(e.status), e.body)
}

// Sync pulls then pushes. changed reports whether what the server sent
// changed d.
func (h *HTTPSync) Sync(ctx context.Context, d *doc.Doc) (changed bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	origin := h.attach(d)
	before := h.applied.Load()
	defer func() { changed = h.applied.Load() != before }()
	// Checked before the pull, which may integrate what is pending under
	// this HTTPSync's origin.
	if !bytes.Equal(pendingFingerprint(d), h.pushedPending) {
		h.dirty.Store(true)
	}

	if err := h.pull(ctx, d, origin); err != nil {
		return false, err
	}
	if !h.dirty.Swap(false) && !ahead(stateVectorOf(d), h.serverSV) {
		return false, nil
	}
	if err := h.push(ctx, d, origin); err != nil {
		h.dirty.Store(true)
		return false, err
	}
	return false, nil
}

// Close stops watching the document.
func (h *HTTPSync) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unsub != nil {
		h.unsub()
	}
	h.doc, h.unsub, h.etag, h.serverSV, h.pushedPending = nil, nil, "", nil, nil
}

// attach starts watching d, afresh when it is not the document of the last
// Sync, and returns the origin HTTPSync applies the server's updates in.
func (h *HTTPSync) attach(d *doc.Doc) *HTTPSyncOrigin {
	if h.doc == d && h.syncURL == h.URL && h.unsub != nil {
		return h.current.Load()
	}
	if h.unsub != nil {
		h.unsub()
	}
	origin := &HTTPSyncOrigin{URL: h.URL}
	h.doc, h.syncURL, h.etag, h.serverSV, h.pushedPending = d, h.URL, "", nil, nil
	// What the document already holds is pushed; an empty one has nothing.
	h.dirty.Store(len(stateVectorOf(d)) > 0)
	h.current.Store(origin)
	h.unsub = d.OnUpdate(func(_ []byte, o any) {
		if o == any(origin) {
			h.applied.Add(1)
			return
		}
		h.dirty.Store(true)
	})
	return origin
}

func (h *HTTPSync) pull(ctx context.Context, d *doc.Doc, origin *HTTPSyncOrigin) error {
	u, err := h.url(stateVectorOf(d), h.V2)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	h.headers(req)
	if h.etag != "" {
		req.Header.Set("If-None-Match", h.etag)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	default:
		return statusError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return h.applyResponse(resp, d, body, h.V2, origin)
}

func (h *HTTPSync) push(ctx context.Context, d *doc.Doc, origin *HTTPSyncOrigin) error {
	t := d.ReadTxn()
	diff := encoding.EncodeDiffWithPending(d, t, h.serverSV)
	local := t.Store().GetStateVector()
	pending := encoding.PendingFingerprint(t)
	t.Close()
	u, err := h.url(local, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(diff))
	if err != nil {
		return err
	}
	h.headers(req)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := h.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := h.applyResponse(resp, d, body, false, origin); err != nil {
		return err
	}
	h.pushedPending = pending
	return nil
}

// applyResponse applies a response's update, then takes its ETag and state
// vector, so they advance only once the update is in.
func (h *HTTPSync) applyResponse(resp *http.Response, d *doc.Doc, body []byte, v2 bool, origin *HTTPSyncOrigin) error {
	var err error
	if v2 {
		err = encoding.ApplyUpdateV2WithOrigin(d, body, origin)
	} else {
		err = encoding.ApplyUpdateWithOrigin(d, body, origin)
	}
	if err != nil {
		return fmt.Errorf("client: HTTP sync: apply the server's update: %w", err)
	}
	if raw, err := base64.StdEncoding.DecodeString(resp.Header.Get("Ygo-State-Vector")); err == nil {
		if sv, _, err := encoding.DecodeStateVector(raw); err == nil {
			h.serverSV = sv
		}
	}
	h.etag = resp.Header.Get("ETag")
	return nil
}

func (h *HTTPSync) url(sv store.StateVector, v2 bool) (string, error) {
	u, err := url.Parse(h.URL)
	if err != nil {
		return "", fmt.Errorf("client: HTTP sync: %w", err)
	}
	q := u.Query()
	q.Set("sv", base64.RawURLEncoding.EncodeToString(encoding.EncodeStateVector(sv, nil)))
	if v2 {
		q.Set("v", "2")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (h *HTTPSync) headers(req *http.Request) {
	for k, vs := range h.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

func (h *HTTPSync) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return http.DefaultClient
}

func statusError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &errHTTPStatus{status: resp.StatusCode, body: string(bytes.TrimSpace(msg))}
}

func pendingFingerprint(d *doc.Doc) []byte {
	t := d.ReadTxn()
	defer t.Close()
	return encoding.PendingFingerprint(t)
}

func stateVectorOf(d *doc.Doc) store.StateVector {
	t := d.ReadTxn()
	defer t.Close()
	return t.Store().GetStateVector()
}

// ahead reports whether local holds a clock remote does not cover.
func ahead(local, remote store.StateVector) bool {
	for c, clock := range local {
		if clock > remote[c] {
			return true
		}
	}
	return false
}
