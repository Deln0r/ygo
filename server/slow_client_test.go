package server_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
	"github.com/Deln0r/ygo/server/backplane"
)

// pipeListener serves each dial over a net.Pipe: a write on one end
// completes only when the other end reads it, with no kernel buffer in
// between, so a client that stops reading blocks the server's write at
// once and a slow reader paces it exactly. Loopback TCP would swallow
// hundreds of KiB first, differently on macOS and Linux.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	serverEnd, clientEnd := net.Pipe()
	select {
	case l.conns <- serverEnd:
		return clientEnd, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// startPipeServer runs a server over a pipeListener.
func startPipeServer(t *testing.T, opts server.Options) (*pipeListener, *server.Server) {
	t.Helper()
	srv := server.New(opts)
	l := newPipeListener()
	// net/http logs a recovered handler panic; the panic tests expect one.
	hs := &http.Server{Handler: srv.Handler(), ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() {
		_ = srv.Close(context.Background())
		_ = hs.Close()
		_ = l.Close()
	})
	return l, srv
}

// dialPipe connects a test client to the pipe server. The client reads
// messages of any size, so a document-sized SyncStep2 fits.
func dialPipe(t *testing.T, l *pipeListener, docName string, clientID uint64) *wsClient {
	t.Helper()
	return dialPipeHeader(t, l, docName, clientID, nil)
}

func dialPipeHeader(t *testing.T, l *pipeListener, docName string, clientID uint64, hdr http.Header) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	httpc := &http.Client{Transport: &http.Transport{DialContext: l.dial}}
	c, _, err := websocket.Dial(ctx, "ws://pipe/"+docName, &websocket.DialOptions{HTTPClient: httpc, HTTPHeader: hdr})
	if err != nil {
		cancel()
		t.Fatalf("pipe dial: %v", err)
	}
	c.SetReadLimit(-1)
	return &wsClient{
		conn:      c,
		ctx:       ctx,
		cancel:    cancel,
		doc:       doc.NewDocWithOptions(doc.Options{ClientID: clientID}),
		awareness: awareness.New(clientID),
	}
}

// pushEnvelope appends value to c's "items" array and returns a SyncUpdate
// envelope carrying only that transaction's update.
func pushEnvelope(c *wsClient, value string) []byte {
	arr := types.NewArray(c.doc.Branch("items"))
	var update []byte
	unsub := c.doc.OnUpdate(func(u []byte, _ any) { update = append([]byte(nil), u...) })
	txn := c.doc.WriteTxn()
	arr.Push(txn, value)
	txn.Commit()
	unsub()
	return syncpkg.EncodeSyncUpdate(update)
}

// readAll reads c on its own goroutine for the rest of the test, applying
// every document update to c.doc, and closes done once c's "items" array
// holds want entries. Over net.Pipe a client that stops reading blocks the
// server's write to it, so every client that is meant to keep up must read
// like this, never only between its own writes. Nothing else may touch c.doc.
func readAll(c *wsClient, want uint64) (done <-chan struct{}) {
	ch := make(chan struct{})
	go func() {
		arr := types.NewArray(c.doc.Branch("items"))
		signalled := false
		for {
			_, raw, err := c.conn.Read(c.ctx)
			if err != nil {
				return
			}
			f, _, err := syncpkg.DecodeEnvelope(raw)
			if err != nil || f.Type != syncpkg.MessageSync || (f.SyncSub != syncpkg.SyncUpdate && f.SyncSub != syncpkg.SyncStep2) {
				continue
			}
			_ = encoding.ApplyUpdate(c.doc, f.Payload)
			if !signalled && arr.Len() >= want {
				signalled = true
				close(ch)
			}
		}
	}()
	return ch
}

// drain reads and discards everything c receives, so a client whose own
// echoes are not under test never blocks the server's writes to it. It does
// not touch c.doc, which the test goroutine writes.
func drain(c *wsClient) {
	go func() {
		for {
			if _, _, err := c.conn.Read(c.ctx); err != nil {
				return
			}
		}
	}()
}

// drainer reads and discards everything c receives, like drain, and lets the
// test wait until the server has handled every frame c sent before: barrier
// sends a SyncStep1 and waits for its SyncStep2 reply, which the server's
// read loop for c makes only after it has finished c's earlier frames, their
// broadcasts to every peer included.
type drainer struct {
	c     *wsClient
	step2 chan struct{}
}

func newDrainer(c *wsClient) *drainer {
	d := &drainer{c: c, step2: make(chan struct{}, 16)}
	go func() {
		for {
			_, raw, err := c.conn.Read(c.ctx)
			if err != nil {
				return
			}
			if f, _, err := syncpkg.DecodeEnvelope(raw); err == nil && f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2 {
				d.step2 <- struct{}{}
			}
		}
	}()
	return d
}

func (d *drainer) barrier(t *testing.T) {
	t.Helper()
	d.c.write(t, emptyStep1())
	select {
	case <-d.step2:
	case <-time.After(10 * time.Second):
		t.Fatal("barrier: no SyncStep2 reply")
	}
}

// pushSettled pushes one update from d's client, waits until the server has
// handled it, and then until the room holds exactly want queued messages, so
// that no client that keeps up has anything left in its queue when the next
// update comes: a small MaxSendQueue then trips only for the client under
// test, however slowly the test's goroutines are scheduled.
func pushSettled(t *testing.T, srv *server.Server, d *drainer, value string, want int) []byte {
	t.Helper()
	env := push(t, d.c, value)
	d.barrier(t)
	waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == want })
	return env
}

// push writes one update from c and fails the test on a write error.
func push(t *testing.T, c *wsClient, value string) []byte {
	t.Helper()
	env := pushEnvelope(c, value)
	c.write(t, env)
	return env
}

// readToClose reads c until the server closes it, returning the messages
// read on the way and the close frame.
func readToClose(t *testing.T, c *wsClient) ([][]byte, websocket.CloseError) {
	t.Helper()
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	var msgs [][]byte
	for {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) {
				t.Fatalf("read ended without a close frame after %d messages: %v", len(msgs), err)
			}
			return msgs, ce
		}
		msgs = append(msgs, raw)
	}
}

// queued returns the messages queued in docName's room, or -1 when the room
// is not resident.
func queued(srv *server.Server, docName string) int {
	for _, d := range srv.Stats().Docs {
		if d.Name == docName {
			return d.QueuedMessages
		}
	}
	return -1
}

// writers counts the server's connection writer goroutines in a dump of
// every goroutine's stack.
func writers() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "server.(*conn).writeLoop(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// emptyStep1 is a SyncStep1 with an empty state vector: "send me everything".
func emptyStep1() []byte {
	return syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil))
}

// rawUpgrade opens a pipe connection and does the WebSocket handshake by
// hand, for a client that sends frames no WebSocket library would. It
// returns the connection and a reader positioned after the 101 response.
func rawUpgrade(t *testing.T, l *pipeListener, docName string) (net.Conn, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := l.dial(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req := "GET /" + docName + " HTTP/1.1\r\nHost: pipe\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	return conn, br
}

// readServerFrame reads one unmasked frame the server sent and returns its
// opcode.
func readServerFrame(br *bufio.Reader) (byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return 0, err
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, err
		}
		n = uint64(ext[0])<<8 | uint64(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, err
		}
		n = 0
		for _, b := range ext {
			n = n<<8 | uint64(b)
		}
	}
	if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
		return 0, err
	}
	return h[0] & 0x0f, nil
}

// A client that has stopped reading does not hold back the rest of its room:
// ten updates reach a reading client while the stuck one has read nothing.
// The stuck client stays admitted, its updates wait in its queue, and once
// it reads again it gets each of them in the order they were sent, byte for
// byte. WriteTimeout is far longer than the test, so a server that writes to
// its peers in turn cannot get past the stuck client by timing it out.
func TestServer_StuckClientDoesNotDelayRoom(t *testing.T) {
	l, srv := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute})

	stuck := dialPipe(t, l, "room", 3)
	defer stuck.close()
	stuck.read(t) // the initial SyncStep1; after this it reads nothing until told
	fast := dialPipe(t, l, "room", 2)
	defer fast.close()
	fast.read(t)
	writer := dialPipe(t, l, "room", 1)
	defer writer.close()
	writer.read(t)

	got := readAll(fast, 10)
	drain(writer)

	sent := make([][]byte, 10)
	for i := range sent {
		sent[i] = pushEnvelope(writer, fmt.Sprint(i))
	}
	go func() {
		for _, env := range sent {
			if writer.conn.Write(writer.ctx, websocket.MessageBinary, env) != nil {
				return
			}
		}
	}()

	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the reading client did not get 10 updates while another client of the room was not reading")
	}
	// One update is being written to the stuck client; nine wait for it.
	waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == 9 })
	if st := srv.Stats(); st.Connections != 3 || st.SlowClientsClosed != 0 {
		t.Fatalf("stats = %+v; want the stuck client still admitted and nobody closed", st)
	}
	for i, want := range sent {
		_, raw, err := stuck.conn.Read(stuck.ctx)
		if err != nil {
			t.Fatalf("stuck client read %d: %v", i, err)
		}
		if !bytes.Equal(raw, want) {
			t.Fatalf("stuck client's message %d is not update %d as sent", i, i)
		}
	}
}

// A stuck client whose queue reaches MaxSendQueue is chosen for closing once
// and its backlog is dropped at once. Reading again before its write
// deadline, it gets the update that was being written, then 1013, and
// nothing of the dropped backlog; the reading client is unaffected; the
// stuck client reconnects with a fresh document and converges.
func TestServer_StuckClientOverflow(t *testing.T) {
	l, srv := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute, MaxSendQueue: 4})

	stuck := dialPipe(t, l, "room", 3)
	defer stuck.close()
	stuck.read(t)
	fast := dialPipe(t, l, "room", 2)
	defer fast.close()
	fast.read(t)
	writer := dialPipe(t, l, "room", 1)
	defer writer.close()
	writer.read(t)
	got := readAll(fast, 10)
	wd := newDrainer(writer)

	// The stuck client's writer takes the first update and blocks on it; the
	// next four wait in its queue, up to the bound; the fifth overflows it and
	// its backlog goes.
	first := pushSettled(t, srv, wd, "0", 0)
	for i := 1; i <= 4; i++ {
		pushSettled(t, srv, wd, fmt.Sprint(i), i)
	}
	pushSettled(t, srv, wd, "5", 0)
	if n := srv.Stats().SlowClientsClosed; n != 1 {
		t.Fatalf("SlowClientsClosed = %d after the overflow, want 1", n)
	}
	for i := 6; i < 10; i++ {
		pushSettled(t, srv, wd, fmt.Sprint(i), 0)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the reading client did not get every update")
	}

	msgs, ce := readToClose(t, stuck)
	if len(msgs) != 1 || !bytes.Equal(msgs[0], first) {
		t.Fatalf("stuck client got %d messages before the close, want only the update being written", len(msgs))
	}
	if ce.Code != websocket.StatusTryAgainLater || ce.Reason != "send queue full" {
		t.Fatalf("close = %d %q, want 1013 \"send queue full\"", ce.Code, ce.Reason)
	}
	if n := srv.Stats().SlowClientsClosed; n != 1 {
		t.Fatalf("SlowClientsClosed = %d, want 1", n)
	}

	again := dialPipe(t, l, "room", 4)
	defer again.close()
	again.read(t)
	again.write(t, emptyStep1())
	readUntilItems(t, "reconnected client", again, 10)
}

// A client that keeps reading, but slower than its room sends, is closed with
// 1013 "send queue stalled" once an update has waited in its queue longer
// than WriteTimeout, though every single write to it completes in time.
func TestServer_StalledClientClosed(t *testing.T) {
	l, srv := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Second})

	slow := dialPipe(t, l, "room", 3)
	defer slow.close()
	slow.read(t)
	writer := dialPipe(t, l, "room", 1)
	defer writer.close()
	writer.read(t)
	drain(writer)

	const n = 40
	for i := 0; i < n; i++ {
		push(t, writer, fmt.Sprint(i))
	}
	// One update per 100 ms: each write completes well inside WriteTimeout,
	// but the tenth or so update has waited longer than it.
	read := 0
	for {
		time.Sleep(100 * time.Millisecond)
		_, _, err := slow.conn.Read(slow.ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != websocket.StatusTryAgainLater || ce.Reason != "send queue stalled" {
				t.Fatalf("slow client ended with %v after %d updates, want 1013 \"send queue stalled\"", err, read)
			}
			break
		}
		read++
	}
	if read == 0 || read >= n {
		t.Fatalf("slow client read %d of %d updates before the close", read, n)
	}
	waitFor(t, 5*time.Second, func() bool { return srv.Stats().SlowClientsClosed == 1 })
}

// An overflow while the connection's read loop waits for one of its replies
// keeps the overflow's 1013: the reply fails, and neither the read loop's
// 1011 (a failed SyncStep2) nor the auth denial's 4401 competes with it.
// The initial sync, a SyncStep2 reply and the auth denial's sends are the
// three places a reply waits.
func TestServer_OverflowWhileReplyWaits(t *testing.T) {
	type room struct {
		srv    *server.Server
		writer *wsClient
		wd     *drainer
		first  []byte
	}
	// start pushes the first update and returns once the target's writer has
	// taken it and is blocked on it (the target does not read).
	start := func(t *testing.T, r *room) {
		t.Helper()
		r.first = pushSettled(t, r.srv, r.wd, "0", 0)
	}
	// fill queues two more updates on top of base queued messages, which
	// brings the target's fan-out to its bound; the next one overflows it.
	fill := func(t *testing.T, r room, base int) {
		t.Helper()
		pushSettled(t, r.srv, r.wd, "1", base+1)
		pushSettled(t, r.srv, r.wd, "2", base+2)
	}
	expect1013 := func(t *testing.T, r room, target *wsClient) {
		t.Helper()
		pushSettled(t, r.srv, r.wd, "3", 0)
		if n := r.srv.Stats().SlowClientsClosed; n != 1 {
			t.Fatalf("SlowClientsClosed = %d after the overflow, want 1", n)
		}
		msgs, ce := readToClose(t, target)
		if len(msgs) != 1 || !bytes.Equal(msgs[0], r.first) {
			t.Fatalf("target got %d messages before the close, want only the update being written", len(msgs))
		}
		if ce.Code != websocket.StatusTryAgainLater || ce.Reason != "send queue full" {
			t.Fatalf("close = %d %q, want 1013 \"send queue full\"", ce.Code, ce.Reason)
		}
		waitFor(t, 5*time.Second, func() bool { return r.srv.Stats().Connections == 1 })
	}
	opts := func() (server.Options, room) {
		return server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute, MaxSendQueue: 2}, room{}
	}

	t.Run("initial sync", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		gate := make(chan struct{})
		o, r := opts()
		o.OnConnect = func(_, _ string, req *http.Request) error {
			if req.Header.Get("X-Gate") != "" {
				entered <- struct{}{}
				<-gate
			}
			return nil
		}
		l, srv := startPipeServer(t, o)
		writer := dialPipe(t, l, "room", 1)
		defer writer.close()
		writer.read(t)
		target := dialPipeHeader(t, l, "room", 2, http.Header{"X-Gate": {"1"}})
		defer target.close()
		<-entered // admitted, its writer running, held before the initial sync
		r.srv, r.writer, r.wd = srv, writer, newDrainer(writer)
		start(t, &r)
		fill(t, r, 0)
		close(gate) // the initial SyncStep1 queues behind the two and waits
		waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == 3 })
		expect1013(t, r, target)
	})

	t.Run("SyncStep2", func(t *testing.T) {
		o, r := opts()
		l, srv := startPipeServer(t, o)
		writer := dialPipe(t, l, "room", 1)
		defer writer.close()
		writer.read(t)
		target := dialPipe(t, l, "room", 2)
		defer target.close()
		target.read(t)
		r.srv, r.writer, r.wd = srv, writer, newDrainer(writer)
		start(t, &r)
		target.write(t, emptyStep1()) // its SyncStep2 reply queues and waits
		waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == 1 })
		fill(t, r, 1)
		expect1013(t, r, target)
	})

	t.Run("auth denial", func(t *testing.T) {
		o, r := opts()
		o.OnAuthenticate = func(_, _ string) error { return errors.New("denied") }
		l, srv := startPipeServer(t, o)
		writer := dialPipe(t, l, "room", 1)
		defer writer.close()
		writer.read(t)
		target := dialPipe(t, l, "room", 2)
		defer target.close()
		target.read(t)
		r.srv, r.writer, r.wd = srv, writer, newDrainer(writer)
		start(t, &r)
		target.write(t, syncpkg.EncodeAuthToken("bad")) // AuthPermissionDenied queues and waits
		waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == 1 })
		fill(t, r, 1)
		expect1013(t, r, target)
	})
}

// A reply is not counted against MaxSendQueueBytes: a document many times
// the threshold, queued behind a blocked write, does not make the next
// update overflow, and the client gets the whole document and stays.
func TestServer_LargeReplyNotCounted(t *testing.T) {
	seed := doc.NewDocWithOptions(doc.Options{ClientID: 999})
	arr := types.NewArray(seed.Branch("items")) // before the transaction: Branch takes the doc lock
	txn := seed.WriteTxn()
	for i := 0; i < 32; i++ {
		arr.Push(txn, strings.Repeat("x", 2048))
	}
	txn.Commit()
	seedUpdate := encoding.EncodeStateAsUpdate(seed)

	l, srv := startPipeServer(t, server.Options{
		OriginPatterns:    []string{"*"},
		WriteTimeout:      time.Minute,
		MaxSendQueueBytes: 1024,
		OnLoadDocument:    func(string) ([]byte, error) { return seedUpdate, nil },
	})
	writer := dialPipe(t, l, "room", 1)
	defer writer.close()
	writer.read(t)
	wd := newDrainer(writer)
	target := dialPipe(t, l, "room", 2)
	defer target.close()
	target.read(t)

	first := push(t, writer, "a")
	wd.barrier(t)
	waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == 0 })
	target.write(t, emptyStep1()) // a SyncStep2 of about 64 KiB queues behind the blocked write
	waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == 1 })
	second := push(t, writer, "b")
	wd.barrier(t)
	if n := queued(srv, "room"); n != 2 {
		t.Fatalf("queued = %d, want the reply and the second update", n)
	}
	if n := srv.Stats().SlowClientsClosed; n != 0 {
		t.Fatalf("SlowClientsClosed = %d: the reply counted against the byte threshold", n)
	}

	for i, want := range []string{"first update", "SyncStep2", "second update"} {
		_, raw, err := target.conn.Read(target.ctx)
		if err != nil {
			t.Fatalf("target read %d (%s): %v", i, want, err)
		}
		switch i {
		case 0:
			if !bytes.Equal(raw, first) {
				t.Fatalf("message %d is not the %s", i, want)
			}
		case 1:
			f, _, err := syncpkg.DecodeEnvelope(raw)
			if err != nil || f.Type != syncpkg.MessageSync || f.SyncSub != syncpkg.SyncStep2 || len(raw) < 64<<10 {
				t.Fatalf("message %d is not the document-sized SyncStep2 (%d bytes)", i, len(raw))
			}
		case 2:
			if !bytes.Equal(raw, second) {
				t.Fatalf("message %d is not the %s", i, want)
			}
		}
	}
	if st := srv.Stats(); st.SlowClientsClosed != 0 || st.Connections != 2 {
		t.Fatalf("stats = %+v, want both clients connected", st)
	}
}

// MaxSendQueueBytes is a backlog threshold, not a message limit: one update
// larger than it reaches a peer whose queue is empty. A large update the
// writer has not yet taken does count, so a stuck peer that gets a large and
// then a small update overflows, as documented.
func TestServer_LargeEnvelope(t *testing.T) {
	big := strings.Repeat("y", 4096)

	t.Run("reaches a peer with an empty queue", func(t *testing.T) {
		l, srv := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, MaxSendQueueBytes: 1024, ReadLimit: 1 << 20})
		reader := dialPipe(t, l, "room", 2)
		defer reader.close()
		reader.read(t)
		got := readAll(reader, 1)
		writer := dialPipe(t, l, "room", 1)
		defer writer.close()
		writer.read(t)
		drain(writer)
		push(t, writer, big)
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			t.Fatal("the large update did not arrive")
		}
		if n := srv.Stats().SlowClientsClosed; n != 0 {
			t.Fatalf("SlowClientsClosed = %d, want 0", n)
		}
	})

	t.Run("a large then a small update overflow a stuck peer", func(t *testing.T) {
		l, srv := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute, MaxSendQueueBytes: 1024, ReadLimit: 1 << 20})
		stuck := dialPipe(t, l, "room", 3)
		defer stuck.close()
		stuck.read(t)
		writer := dialPipe(t, l, "room", 1)
		defer writer.close()
		writer.read(t)
		wd := newDrainer(writer)
		first := push(t, writer, "a")
		wd.barrier(t)
		waitFor(t, 5*time.Second, func() bool { return queued(srv, "room") == 0 })
		push(t, writer, big)
		push(t, writer, "b")
		waitFor(t, 5*time.Second, func() bool { return srv.Stats().SlowClientsClosed >= 1 })
		msgs, ce := readToClose(t, stuck)
		if len(msgs) != 1 || !bytes.Equal(msgs[0], first) || ce.Code != websocket.StatusTryAgainLater {
			t.Fatalf("stuck peer got %d messages and close %d %q, want the in-flight update then 1013", len(msgs), ce.Code, ce.Reason)
		}
	})
}

// A client that writes requests and never reads stalls only its own read
// loop: the server reads one SyncStep1, waits for its reply to be written,
// and reads no further frame from it, while the room goes on.
func TestServer_ReplyBackpressure(t *testing.T) {
	l, _ := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute})

	flood := dialPipe(t, l, "room", 5)
	defer func() { _ = flood.conn.CloseNow(); flood.cancel() }()
	flood.read(t)
	var mu sync.Mutex
	wrote := 0
	go func() {
		for i := 0; i < 5; i++ {
			if flood.conn.Write(flood.ctx, websocket.MessageBinary, emptyStep1()) != nil {
				return
			}
			mu.Lock()
			wrote++
			mu.Unlock()
		}
	}()

	reader := dialPipe(t, l, "room", 2)
	defer reader.close()
	reader.read(t)
	got := readAll(reader, 3)
	writer := dialPipe(t, l, "room", 1)
	defer writer.close()
	writer.read(t)
	drain(writer)
	for i := 0; i < 3; i++ {
		push(t, writer, fmt.Sprint(i))
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the room stopped while one client flooded requests without reading")
	}
	time.Sleep(200 * time.Millisecond) // give a server that reads ahead the time to do it
	mu.Lock()
	n := wrote
	mu.Unlock()
	if n > 2 {
		t.Fatalf("the server read %d requests from a client that reads nothing, want it to stop after one reply", n)
	}
}

// The awareness sweep does not wait for a stuck client: room A's tombstone is
// due one sweep before room B's, and B's must still arrive, which needs the
// earlier sweep, the one that met the stuck client, to have completed.
func TestServer_SweepNotHeldByStuckClient(t *testing.T) {
	l, _ := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute, AwarenessTimeout: time.Second})

	stuck := dialPipe(t, l, "a", 3)
	defer stuck.close()
	stuck.read(t)
	p := dialPipe(t, l, "a", 4)
	defer p.close()
	p.read(t)
	drain(p)
	q := dialPipe(t, l, "b", 5)
	defer q.close()
	q.read(t)
	drain(q)
	watcher := dialPipe(t, l, "b", 6)
	defer watcher.close()
	watcher.read(t)

	p.awareness.SetLocalState([]byte(`{"room":"a"}`))
	p.write(t, syncpkg.EncodeAwareness(p.awareness.Encode([]uint64{4})))
	time.Sleep(2500 * time.Millisecond) // A's entry is swept first
	q.awareness.SetLocalState([]byte(`{"room":"b"}`))
	q.write(t, syncpkg.EncodeAwareness(q.awareness.Encode([]uint64{5})))

	removed := make(chan struct{})
	go func() {
		seen := false
		for {
			_, raw, err := watcher.conn.Read(watcher.ctx)
			if err != nil {
				return
			}
			f, _, err := syncpkg.DecodeEnvelope(raw)
			if err != nil || f.Type != syncpkg.MessageAwareness {
				continue
			}
			if _, err := watcher.awareness.Apply(f.Payload, "server"); err != nil {
				continue
			}
			_, present := watcher.awareness.States()[5]
			if present {
				seen = true
			} else if seen {
				close(removed)
				return
			}
		}
	}()
	select {
	case <-removed:
	case <-time.After(10 * time.Second):
		t.Fatal("room B's swept presence never reached its watcher while room A had a stuck client")
	}
}

// A stuck client on one instance does not hold back a fast client of the
// same instance, nor the publishing client on another, under more edits
// than the memory hub buffers for a subscriber (256).
func TestServer_BackplaneNotHeldByStuckClient(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })
	l1, _ := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute, Backplane: hub.Conn()})
	l2, _ := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: time.Minute, Backplane: hub.Conn()})

	stuck := dialPipe(t, l1, "shared", 3)
	defer stuck.close()
	stuck.read(t)
	fast := dialPipe(t, l1, "shared", 2)
	defer fast.close()
	fast.read(t)
	const n = 300
	got := readAll(fast, n)
	editor := dialPipe(t, l2, "shared", 1)
	defer editor.close()
	editor.read(t)
	drain(editor)

	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			if editor.conn.Write(editor.ctx, websocket.MessageBinary, pushEnvelope(editor, fmt.Sprint(i))) != nil {
				return
			}
		}
		close(done)
	}()
	select {
	case <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("a fast client on instance 1 did not get the edits made on instance 2 while instance 1 had a stuck client")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the editing client on instance 2 was held back by a stuck client on instance 1")
	}
}

// Every released connection ends with its socket closed and its writer gone,
// also when the client does nothing to end it: an OnConnect or ReadOnly
// panic, and a frame coder/websocket refuses without closing (an unmasked
// client frame).
func TestServer_ReleaseClosesSocketWithoutClientHelp(t *testing.T) {
	t.Run("OnConnect panic", func(t *testing.T) {
		base := writers()
		l, _ := startPipeServer(t, server.Options{
			OriginPatterns: []string{"*"},
			OnConnect:      func(string, string, *http.Request) error { panic("boom in OnConnect") },
		})
		c := dialPipe(t, l, "room", 1) // never closed by the test
		defer c.cancel()
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		defer cancel()
		_, _, err := c.conn.Read(ctx)
		if err == nil || ctx.Err() != nil {
			t.Fatalf("read on a connection whose OnConnect panicked = %v; want the server to close the socket", err)
		}
		waitFor(t, 10*time.Second, func() bool { return writers() <= base })
	})

	t.Run("ReadOnly panic", func(t *testing.T) {
		base := writers()
		l, _ := startPipeServer(t, server.Options{
			OriginPatterns: []string{"*"},
			ReadOnly:       func(string, *http.Request) bool { panic("boom in ReadOnly") },
		})
		c := dialPipe(t, l, "room", 1) // never closed by the test
		defer c.cancel()
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		defer cancel()
		_, _, err := c.conn.Read(ctx)
		if err == nil || ctx.Err() != nil {
			t.Fatalf("read on a connection whose ReadOnly panicked = %v; want the server to close the socket", err)
		}
		waitFor(t, 10*time.Second, func() bool { return writers() <= base })
	})

	t.Run("unmasked client frame", func(t *testing.T) {
		base := writers()
		l, _ := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}})
		conn, br := rawUpgrade(t, l, "room")
		ended := make(chan struct{})
		go func() { // read everything the server sends until the socket closes
			_, _ = io.Copy(io.Discard, br)
			close(ended)
		}()
		if _, err := conn.Write([]byte{0x82, 0x01, 0x00}); err != nil { // FIN binary, unmasked
			t.Fatal(err)
		}
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("the server left the socket open after refusing an unmasked frame")
		}
		waitFor(t, 10*time.Second, func() bool { return writers() <= base })
	})
}

// Server.Close still leaves live connections working, as before; they close
// normally afterwards and their writers exit.
func TestServer_CloseKeepsLiveConnections(t *testing.T) {
	base := writers()
	l, srv := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}})
	reader := dialPipe(t, l, "room", 2)
	reader.read(t)
	got := readAll(reader, 1)
	writer := dialPipe(t, l, "room", 1)
	writer.read(t)
	drain(writer)

	if err := srv.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	push(t, writer, "after close")
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("a live connection stopped working after Server.Close")
	}
	reader.close()
	writer.close()
	waitFor(t, 10*time.Second, func() bool { return writers() <= base })
}

// Connections that end every way the queue can end them leave no writer
// behind: a normal close, an overflow, and a write timeout.
func TestServer_NoWriterLeak(t *testing.T) {
	base := writers()
	l, srv := startPipeServer(t, server.Options{OriginPatterns: []string{"*"}, WriteTimeout: 200 * time.Millisecond, MaxSendQueue: 2})
	writer := dialPipe(t, l, "room", 1)
	defer writer.close()
	writer.read(t)
	wd := newDrainer(writer)

	for i := 0; i < 21; i++ {
		c := dialPipe(t, l, "room", uint64(10+i))
		c.read(t)
		switch i % 3 {
		case 0: // normal close
			c.close()
		case 1: // overflow: it never reads again, and its write deadline ends it
			for j := 0; j < 4; j++ {
				push(t, writer, fmt.Sprintf("%d-%d", i, j))
				wd.barrier(t) // the writer's own echoes never pile up to the bound
			}
			waitFor(t, 5*time.Second, func() bool { return srv.Stats().Connections == 1 })
			c.cancel()
		default: // a write timeout alone
			push(t, writer, fmt.Sprint(i))
			wd.barrier(t)
			waitFor(t, 5*time.Second, func() bool { return srv.Stats().Connections == 1 })
			c.cancel()
		}
		waitFor(t, 5*time.Second, func() bool { return srv.Stats().Connections == 1 })
	}
	waitFor(t, 10*time.Second, func() bool { return writers() <= base+1 }) // the writer client's own
}

// A close the server starts ends within its limit even when the peer has
// announced a data frame and withholds its payload: coder/websocket's close
// handshake reads that payload with no deadline, so without the limit the
// close, the read loop and the release behind it would wait forever. The
// peer is a raw socket; it sends an empty envelope (the server closes with
// 1002) and, in the same write, the header of a 1000-byte frame with none of
// the payload, so the condition exists before the server's close begins.
func TestServer_CloseBoundedWhenPeerWithholdsFrame(t *testing.T) {
	disconnected := make(chan struct{}, 1)
	l, srv := startPipeServer(t, server.Options{
		OriginPatterns: []string{"*"},
		OnDisconnect:   func(string, string) { disconnected <- struct{}{} },
	})
	server.SetCloseHandshakeLimit(srv, 300*time.Millisecond)

	conn, br := rawUpgrade(t, l, "room")
	closeSeen := make(chan struct{})
	ended := make(chan struct{})
	go func() { // read the server's frames, note its close frame, run to EOF
		defer close(ended)
		seen := false
		for {
			op, err := readServerFrame(br)
			if err != nil {
				return
			}
			if op == 0x8 && !seen {
				seen = true
				close(closeSeen)
			}
		}
	}()
	frames := []byte{
		0x82, 0x80, 1, 2, 3, 4, // a masked, empty binary frame
		0x82, 0x80 | 126, 0x03, 0xe8, 1, 2, 3, 4, // a masked binary frame announcing 1000 bytes, none of which follow
	}
	if _, err := conn.Write(frames); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not close a connection that sent an empty envelope")
	}
	select {
	case <-disconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the connection was never released: its close waited for a payload the peer withholds")
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the server left the socket open")
	}
}

// handlerSpy wraps a Backplane so a test sees when the wrapped server's
// subscription handler has returned for a message.
type handlerSpy struct {
	backplane.Backplane
	handled chan struct{}
}

func (h *handlerSpy) Subscribe(docName string, onUpdate func([]byte)) (func(), error) {
	return h.Backplane.Subscribe(docName, func(msg []byte) {
		onUpdate(msg)
		h.handled <- struct{}{}
	})
}
