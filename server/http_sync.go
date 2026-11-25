package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/store"
)

// Sync over HTTP: HTTPSyncHandler serves the server's own documents, the
// ones its WebSocket clients edit, to clients that pull and push updates
// with plain requests.

// StateVectorHeader is the response header that carries the server's state
// vector (base64) at the moment of the response's snapshot. A client that
// applied the response can POST its own changes as a diff against it.
const StateVectorHeader = "Ygo-State-Vector"

const (
	// maxSVParam bounds the sv query parameter's text.
	maxSVParam = 64 << 10
	// maxHTTPDocName bounds a document name an HTTP request may use.
	maxHTTPDocName = 1024
)

// HTTPOrigin is the origin of the transaction an HTTP POST's update is
// applied in.
type HTTPOrigin struct {
	DocName string
	Request *http.Request
}

// HTTPError is an error Options.OnHTTPRequest returns to choose the
// response's status (and message) of a refused request.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

// HTTPSyncHandler serves the server's documents over HTTP, so clients that
// cannot keep a WebSocket open pull and push updates with requests, and see
// and are seen by the WebSocket clients of the same document at once. The
// document's name comes from Options.DocNameFn, as for WebSocket requests.
//
//	GET  <doc>?sv=<base64 state vector>[&v=2]
//	POST <doc>[?sv=<base64 state vector>]   body: one V1 update
//
// GET returns the document's update for a client at sv (the whole document
// without sv), with its pending updates, as a sync step 2 at that state
// vector carries it; v=2 returns it in V2. POST applies its body as a
// client's update is applied: queued to every connection, stored, published
// and reported to OnChange, in a transaction whose origin is a new
// *HTTPOrigin; with sv, the response carries the document's update for sv
// after the write (push and pull in one request), else it is 204. The sv
// parameter is base64, standard or URL-safe, padded or not.
//
// Every response that reads the document carries StateVectorHeader and an
// ETag naming the document's revision; a GET whose If-None-Match holds the
// current one returns 304 with no body: the document has not changed since
// the response that carried it.
//
// Requests are refused, before their body is read or their document
// loaded, by Options.OnHTTPRequest, which must be set (without it every
// request is 403), by Options.ReadOnly for POST (403), and when a
// browser's Origin is neither the request's host nor in
// Options.OriginPatterns (403). POST needs Content-Type
// application/octet-stream (415), which a cross-site form cannot send, and
// a body within Options.ReadLimit (413). Each request holds one of
// Options.MaxConns while it runs (503 past it, as at Options.MaxDocs).
// A GET of a document that is not resident and holds nothing does not keep
// it resident.
func (s *Server) HTTPSyncHandler() http.Handler {
	return http.HandlerFunc(s.serveHTTPSync)
}

func (s *Server) serveHTTPSync(w http.ResponseWriter, r *http.Request) {
	var write bool
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		write = true
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A POST refused before its body is read closes the connection, so the
	// server does not read the body to keep the connection alive.
	refuse := func(msg string, status int) {
		if write {
			w.Header().Set("Connection", "close")
		}
		http.Error(w, msg, status)
	}
	name := s.opts.DocNameFn(r)
	if err := validHTTPDocName(name); err != nil {
		refuse(err.Error(), http.StatusBadRequest)
		return
	}
	if err := checkOrigin(r, s.opts.OriginPatterns); err != nil {
		refuse(err.Error(), http.StatusForbidden)
		return
	}
	if s.opts.OnHTTPRequest == nil {
		refuse("HTTP sync is not enabled (Options.OnHTTPRequest)", http.StatusForbidden)
		return
	}
	if err := s.opts.OnHTTPRequest(r, name, write); err != nil {
		status, msg := http.StatusForbidden, err.Error()
		var he *HTTPError
		if errors.As(err, &he) && he.Status >= 400 && he.Status < 600 {
			status, msg = he.Status, he.Message
		}
		if msg == "" {
			msg = http.StatusText(status)
		}
		refuse(msg, status)
		return
	}
	if write && s.opts.ReadOnly != nil && s.opts.ReadOnly(name, r) {
		refuse("read-only", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	v2 := false
	switch q.Get("v") {
	case "", "1":
	case "2":
		v2 = true
	default:
		refuse("v must be 1 or 2", http.StatusBadRequest)
		return
	}
	if write && v2 {
		refuse("POST takes a V1 update", http.StatusBadRequest)
		return
	}
	sv, hasSV, err := parseSVParam(q)
	if err != nil {
		refuse(err.Error(), http.StatusBadRequest)
		return
	}
	if write {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/octet-stream" {
			refuse("Content-Type must be application/octet-stream", http.StatusUnsupportedMediaType)
			return
		}
	}
	// The slot is held from before the body is read, so MaxConns bounds the
	// bodies being read too.
	if !s.reserveConn() {
		w.Header().Set("Retry-After", "1")
		refuse("server connection limit reached", http.StatusServiceUnavailable)
		return
	}
	defer s.totalConns.Add(-1)
	var body []byte
	if write {
		var status int
		if body, status, err = s.readHTTPUpdate(w, r); err != nil {
			http.Error(w, err.Error(), status)
			return
		}
	}
	if write {
		s.httpPost(w, r, name, body, sv, hasSV)
		return
	}
	s.httpGet(w, r, name, sv, v2)
}

// readHTTPUpdate reads a POST body within ReadLimit and checks that it is
// one V1 update with nothing after it.
func (s *Server) readHTTPUpdate(w http.ResponseWriter, r *http.Request) ([]byte, int, error) {
	limit := s.opts.ReadLimit
	if limit == 0 {
		limit = defaultReadLimit
	}
	src := r.Body
	if limit > 0 {
		src = http.MaxBytesReader(w, r.Body, limit)
	}
	body, err := io.ReadAll(src)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("the update is larger than %d bytes", limit)
		}
		return nil, http.StatusBadRequest, errors.New("failed to read the body")
	}
	if _, rest, err := encoding.DecodeUpdate(body); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid update: %v", err)
	} else if len(rest) > 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid update: %d bytes after one complete update", len(rest))
	}
	return body, 0, nil
}

func (s *Server) httpGet(w http.ResponseWriter, r *http.Request, name string, sv store.StateVector, v2 bool) {
	st, err := s.pinRead(r.Context(), name)
	if err != nil {
		writeOpenError(w, err)
		return
	}
	tag := st.etag()
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, tag) {
		s.unpinRead(st)
		w.Header().Set("ETag", tag)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body, svb := st.snapshot(sv, v2)
	// The hold ends before the response goes out.
	s.unpinRead(st)
	writeSnapshot(w, http.StatusOK, tag, body, svb)
}

func (s *Server) httpPost(w http.ResponseWriter, r *http.Request, name string, update []byte, sv store.StateVector, hasSV bool) {
	var tag string
	var body, svb []byte
	// The document is let go (deferred, so a panicking OnChange does not
	// keep it) before the response is written.
	applyErr, openErr := func() (applyErr, openErr error) {
		d, err := s.OpenDocument(r.Context(), name)
		if err != nil {
			return nil, err
		}
		defer d.Close()
		if err := d.begin(); err != nil {
			return nil, err
		}
		defer d.end()
		applyErr = d.applyUpdate(update, &HTTPOrigin{DocName: name, Request: r})
		if applyErr == nil || errors.Is(applyErr, ErrNotStored) {
			tag = d.st.etag()
			if hasSV {
				body, svb = d.st.snapshot(sv, false)
			} else {
				svb = d.st.stateVector()
			}
		}
		return applyErr, nil
	}()
	if openErr != nil {
		writeOpenError(w, openErr)
		return
	}
	switch {
	case errors.Is(applyErr, ErrNotStored):
		w.Header().Set("ETag", tag)
		w.Header().Set(StateVectorHeader, base64.StdEncoding.EncodeToString(svb))
		http.Error(w, "the update was applied and sent but not stored", http.StatusInternalServerError)
	case applyErr != nil:
		http.Error(w, "invalid update", http.StatusBadRequest)
	case hasSV:
		writeSnapshot(w, http.StatusOK, tag, body, svb)
	default:
		w.Header().Set("ETag", tag)
		w.Header().Set(StateVectorHeader, base64.StdEncoding.EncodeToString(svb))
		w.WriteHeader(http.StatusNoContent)
	}
}

// etag names the document's revision. It is read before any snapshot
// (snapshot) is taken: a change between the two makes the tag older than
// the body, never newer.
func (st *docState) etag() string {
	return fmt.Sprintf("\"%016x-%x\"", st.instance, st.revision.Load())
}

// snapshot is the document's update for a client at sv, pending updates
// included, and its state vector, from one read transaction.
func (st *docState) snapshot(sv store.StateVector, v2 bool) (update, stateVector []byte) {
	t := st.doc.ReadTxn()
	defer t.Close()
	if v2 {
		update = encoding.EncodeDiffV2WithPending(st.doc, t, sv)
	} else {
		update = encoding.EncodeDiffWithPending(st.doc, t, sv)
	}
	return update, encoding.EncodeStateVector(t.Store().GetStateVector(), nil)
}

func (st *docState) stateVector() []byte {
	t := st.doc.ReadTxn()
	defer t.Close()
	return encoding.EncodeStateVector(t.Store().GetStateVector(), nil)
}

func writeSnapshot(w http.ResponseWriter, status int, tag string, body, sv []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", tag)
	h.Set(StateVectorHeader, base64.StdEncoding.EncodeToString(sv))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeOpenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errServerDocsFull):
		w.Header().Set("Retry-After", "1")
		http.Error(w, "server document limit reached", http.StatusServiceUnavailable)
	case errors.Is(err, ErrServerClosed):
		http.Error(w, "server closed", http.StatusServiceUnavailable)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		http.Error(w, "request ended", http.StatusServiceUnavailable)
	default:
		http.Error(w, "failed to load the document", http.StatusInternalServerError)
	}
}

// parseSVParam decodes the sv query parameter: base64 in any of its four
// forms, then a state vector with nothing after it. Absent or empty is the
// empty state vector.
func parseSVParam(q url.Values) (store.StateVector, bool, error) {
	text, ok := q["sv"]
	if !ok || len(text) == 0 || text[0] == "" {
		return nil, ok, nil
	}
	if len(text) > 1 {
		return nil, true, errors.New("sv given more than once")
	}
	if len(text[0]) > maxSVParam {
		return nil, true, errors.New("sv is too long")
	}
	raw, err := decodeBase64Any(text[0])
	if err != nil {
		return nil, true, errors.New("sv is not base64")
	}
	sv, rest, err := encoding.DecodeStateVector(raw)
	if err != nil || len(rest) > 0 {
		return nil, true, errors.New("sv is not a state vector")
	}
	return sv, true, nil
}

// decodeBase64Any decodes standard or URL-safe base64, padded or not. A
// query string may also have turned '+' into ' '.
func decodeBase64Any(s string) ([]byte, error) {
	s = strings.ReplaceAll(s, " ", "+")
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "-_") {
		return base64.RawURLEncoding.DecodeString(s)
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// etagMatches reports whether an If-None-Match value names tag (weak
// comparison, as RFC 9110 has it for If-None-Match), or is "*".
func etagMatches(header, tag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == tag {
			return true
		}
	}
	return false
}

// validHTTPDocName refuses names HTTP requests may not use: empty, longer
// than 1,024 bytes, invalid UTF-8 or holding control characters.
func validHTTPDocName(name string) error {
	switch {
	case name == "":
		return errors.New("no document name in the URL")
	case len(name) > maxHTTPDocName:
		return fmt.Errorf("the document name is longer than %d bytes", maxHTTPDocName)
	case !utf8.ValidString(name):
		return errors.New("the document name is not UTF-8")
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return errors.New("the document name holds a control character")
		}
	}
	return nil
}

// checkOrigin applies the WebSocket upgrade's origin rule (coder/websocket
// authenticateOrigin) to HTTP requests: a request without Origin passes, a
// browser's must be the request's host or match one of patterns, matched
// against the Origin's host, or its scheme and host when the pattern has
// "://".
func checkOrigin(r *http.Request, patterns []string) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("invalid Origin %q", origin)
	}
	if strings.EqualFold(r.Host, u.Host) {
		return nil
	}
	for _, p := range patterns {
		target := u.Host
		if strings.Contains(p, "://") {
			target = u.Scheme + "://" + u.Host
		}
		if ok, err := path.Match(strings.ToLower(p), strings.ToLower(target)); err == nil && ok {
			return nil
		}
	}
	return fmt.Errorf("origin %q is not allowed", origin)
}
