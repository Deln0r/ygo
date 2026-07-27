package nats

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// startJetStreamServerOn starts an embedded NATS server with JetStream enabled
// on the given port (-1 for random) and store dir; it is shut down at test end.
// Shutdown is idempotent, so a test may also stop it early (e.g. to restart it).
func startJetStreamServerOn(t *testing.T, port int, storeDir string) *natsserver.Server {
	t.Helper()
	opts := natstest.DefaultTestOptions
	opts.Port = port
	opts.JetStream = true
	opts.StoreDir = storeDir
	s := natstest.RunServer(&opts)
	t.Cleanup(s.Shutdown)
	return s
}

// runJetStreamServer starts an embedded JetStream server on a random port with
// a per-test store dir and returns its client URL.
func runJetStreamServer(t *testing.T) string {
	t.Helper()
	return startJetStreamServerOn(t, -1, t.TempDir()).ClientURL()
}

// dialReconnect connects with an aggressive reconnect policy so a test that
// restarts the server sees the client re-attach quickly.
func dialReconnect(t *testing.T, url string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url, nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// awaitPayload polls a collector until it has recorded a delivery equal to want,
// or the timeout elapses.
func awaitPayload(t *testing.T, c *collector, want []byte, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for i, n := 0, c.count(); i < n; i++ {
			if bytes.Equal(c.at(i), want) {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func newJetStream(t *testing.T, url string, opts ...JSOption) *JetStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	j, err := NewJetStream(ctx, dial(t, url), opts...)
	if err != nil {
		t.Fatalf("NewJetStream: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// TestJetStream_PublishSkipsSelfDeliversToOthers: a publish reaches other
// instances' consumers but never the publisher's own (origin-filtered), just
// like the core adapter.
func TestJetStream_PublishSkipsSelfDeliversToOthers(t *testing.T) {
	url := runJetStreamServer(t)
	a := newJetStream(t, url)
	b := newJetStream(t, url)

	ca, cb := newCollector(), newCollector()
	unsubA, err := a.Subscribe("doc", ca.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubA()
	if _, err := b.Subscribe("doc", cb.onUpdate); err != nil {
		t.Fatal(err)
	}

	if err := a.Publish(context.Background(), "doc", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1)
	if got := cb.at(0); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("b got %v, want [1 2 3]", got)
	}
	time.Sleep(200 * time.Millisecond) // let any wrong self-delivery arrive
	if n := ca.count(); n != 0 {
		t.Fatalf("publisher received its own publish (%d deliveries)", n)
	}
}

// TestJetStream_DeliversInReceiveOrder: a subscription's delivery goroutine
// invokes onUpdate in stream order.
func TestJetStream_DeliversInReceiveOrder(t *testing.T) {
	url := runJetStreamServer(t)
	a := newJetStream(t, url)
	b := newJetStream(t, url)
	cb := newCollector()
	if _, err := b.Subscribe("d", cb.onUpdate); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := a.Publish(context.Background(), "d", []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	cb.await(t, 20)
	for i := 0; i < 20; i++ {
		if got := cb.at(i)[0]; got != byte(i) {
			t.Fatalf("delivery %d = %d, want %d (out of order)", i, got, i)
		}
	}
}

// TestJetStream_IsolatesDocs: a subscription only sees its own docName's
// publishes (per-consumer FilterSubject).
func TestJetStream_IsolatesDocs(t *testing.T) {
	url := runJetStreamServer(t)
	a := newJetStream(t, url)
	b := newJetStream(t, url)
	cb := newCollector()
	if _, err := b.Subscribe("doc1", cb.onUpdate); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "doc2", []byte{9}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := cb.count(); n != 0 {
		t.Fatalf("doc1 subscriber received a doc2 publish (%d)", n)
	}
}

// TestJetStream_ResumesAfterServerRestart is the regression test for the
// ordered-consumer design: after the JetStream server restarts (a routine
// operational event), the subscriber's consumer must recreate itself and resume
// delivering, rather than dying silently and permanently as a plain ephemeral
// consumer would. The stream uses file storage and the server is restarted in
// place (same port, same store dir), so the stream and its data survive.
func TestJetStream_ResumesAfterServerRestart(t *testing.T) {
	storeDir := t.TempDir()
	s := startJetStreamServerOn(t, -1, storeDir)
	port := s.Addr().(*net.TCPAddr).Port
	url := s.ClientURL()

	ncA := dialReconnect(t, url)
	ncB := dialReconnect(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := NewJetStream(ctx, ncA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := NewJetStream(ctx, ncB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	cb := newCollector()
	if _, err := b.Subscribe("d", cb.onUpdate); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "d", []byte{1}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1)

	// Restart the server in place; the file-storage stream survives.
	s.Shutdown()
	s.WaitForShutdown()
	startJetStreamServerOn(t, port, storeDir)

	// Once the clients reconnect, a publish succeeds again; retry until the
	// reconnect settles.
	deadline := time.Now().Add(15 * time.Second)
	published := false
	for time.Now().Before(deadline) {
		pctx, pcancel := context.WithTimeout(context.Background(), 1*time.Second)
		err := a.Publish(pctx, "d", []byte{2})
		pcancel()
		if err == nil {
			published = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !published {
		t.Fatal("publish never succeeded after server restart")
	}

	// b's ordered consumer must have recovered and delivered the post-restart
	// message; a plain ephemeral consumer would have died silently on the
	// reconnect and never delivered it.
	if !awaitPayload(t, cb, []byte{2}, 15*time.Second) {
		t.Fatal("subscriber never received the post-restart message; the consumer did not resume")
	}
}

// TestJetStream_ResumesAfterRestartBeforeFirstDelivery exercises the reset path
// while the ordered consumer's resume cursor is still unset (no message
// delivered yet). Because the start sequence is pinned at Subscribe (rather than
// a bare "deliver new"), the consumer recreated on the restart resumes from that
// pinned point instead of dying or jumping past pending messages, so a publish
// after the restart is still delivered.
func TestJetStream_ResumesAfterRestartBeforeFirstDelivery(t *testing.T) {
	storeDir := t.TempDir()
	s := startJetStreamServerOn(t, -1, storeDir)
	port := s.Addr().(*net.TCPAddr).Port
	url := s.ClientURL()

	ncA := dialReconnect(t, url)
	ncB := dialReconnect(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := NewJetStream(ctx, ncA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := NewJetStream(ctx, ncB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	cb := newCollector()
	// Subscribe but do not await any delivery: the resume cursor stays unset.
	if _, err := b.Subscribe("d", cb.onUpdate); err != nil {
		t.Fatal(err)
	}

	// Restart before the first delivery, forcing a reset with the cursor unset.
	s.Shutdown()
	s.WaitForShutdown()
	startJetStreamServerOn(t, port, storeDir)

	deadline := time.Now().Add(15 * time.Second)
	published := false
	for time.Now().Before(deadline) {
		pctx, pcancel := context.WithTimeout(context.Background(), 1*time.Second)
		err := a.Publish(pctx, "d", []byte{7})
		pcancel()
		if err == nil {
			published = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !published {
		t.Fatal("publish never succeeded after server restart")
	}
	if !awaitPayload(t, cb, []byte{7}, 15*time.Second) {
		t.Fatal("subscriber never received the message after a restart before first delivery")
	}
}

// TestJetStream_SeesOnlyPostSubscribeMessages: a subscription starts just after
// the stream's current end, so a message published BEFORE it subscribed is not
// replayed (the shared Store, not the stream, carries prior state), while a
// message published AFTER is delivered.
func TestJetStream_SeesOnlyPostSubscribeMessages(t *testing.T) {
	url := runJetStreamServer(t)
	a := newJetStream(t, url)
	b := newJetStream(t, url)

	// A message published before any consumer exists is retained in the stream
	// but the pinned start sequence is past it, so it is not replayed.
	if err := a.Publish(context.Background(), "d", []byte{1}); err != nil {
		t.Fatal(err)
	}
	cb := newCollector()
	if _, err := b.Subscribe("d", cb.onUpdate); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "d", []byte{2}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1)
	if got := cb.at(0); !bytes.Equal(got, []byte{2}) {
		t.Fatalf("first delivery = %v, want [2] (pre-subscribe message must not replay)", got)
	}
	time.Sleep(200 * time.Millisecond)
	if n := cb.count(); n != 1 {
		t.Fatalf("got %d deliveries, want 1 (only the post-subscribe publish)", n)
	}
}

// TestJetStream_UnsubStopsDelivery: after unsub the subscription receives
// nothing; unsub is idempotent.
func TestJetStream_UnsubStopsDelivery(t *testing.T) {
	url := runJetStreamServer(t)
	a := newJetStream(t, url)
	b := newJetStream(t, url)
	cb := newCollector()
	unsub, err := b.Subscribe("d", cb.onUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "d", []byte{1}); err != nil {
		t.Fatal(err)
	}
	cb.await(t, 1)

	unsub()
	if err := a.Publish(context.Background(), "d", []byte{2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := cb.count(); n != 1 {
		t.Fatalf("delivery after unsub: got %d, want 1", n)
	}
	unsub() // idempotent
}

// TestJetStream_ClosedReturnsErr: after Close, Publish and Subscribe return
// ErrClosed; Close is idempotent.
func TestJetStream_ClosedReturnsErr(t *testing.T) {
	url := runJetStreamServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	j, err := NewJetStream(ctx, dial(t, url))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := j.Subscribe("d", func([]byte) {}); err != ErrClosed {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
	if err := j.Publish(context.Background(), "d", []byte{1}); err != ErrClosed {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
}

// TestJetStream_PublishHonorsContext: a cancelled context returns before any
// network work.
func TestJetStream_PublishHonorsContext(t *testing.T) {
	url := runJetStreamServer(t)
	j := newJetStream(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := j.Publish(ctx, "d", []byte{1}); err == nil {
		t.Fatal("Publish with cancelled ctx returned nil, want ctx error")
	}
}

// TestJetStream_RejectsEmptyPrefix: fail-fast validation in the constructor,
// matching the core adapter's empty-prefix check. An empty WithStreamName is
// NOT an error — it is the not-set value, so the prefix-derived default name is
// used instead (exercised by every other test, which pass no WithStreamName).
func TestJetStream_RejectsEmptyPrefix(t *testing.T) {
	url := runJetStreamServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := NewJetStream(ctx, dial(t, url), WithJSPrefix("")); err == nil {
		t.Fatal("NewJetStream with empty prefix returned nil error, want rejection")
	}
}

// TestJetStream_SubjectSafeForSpecialDocNames guards the shared base64url
// subject encoding on the JetStream path: metacharacter docNames route to
// distinct consumers.
func TestJetStream_SubjectSafeForSpecialDocNames(t *testing.T) {
	url := runJetStreamServer(t)
	a := newJetStream(t, url)
	b := newJetStream(t, url)

	cWild := newCollector()
	if _, err := b.Subscribe(">", cWild.onUpdate); err != nil {
		t.Fatal(err)
	}
	cExact := newCollector()
	if _, err := b.Subscribe("a.b", cExact.onUpdate); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(context.Background(), "a.b", []byte{7}); err != nil {
		t.Fatal(err)
	}
	cExact.await(t, 1)
	time.Sleep(200 * time.Millisecond)
	if n := cWild.count(); n != 0 {
		t.Fatalf("wildcard-named docName leaked %d cross-doc messages", n)
	}
}
