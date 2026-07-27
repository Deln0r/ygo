package nats

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ygo "github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/server"
)

// TestBackplane_ServerConvergenceOverNATS is the end-to-end proof: two ygo
// servers, each with its own NATS backplane on a shared NATS server, and a
// client on each. An edit on one client reaches the other, so the update
// travels client -> serverA -> NATS -> serverB -> client.
func TestBackplane_ServerConvergenceOverNATS(t *testing.T) {
	natsURL := runServer(t)

	bpA, err := New(dial(t, natsURL))
	if err != nil {
		t.Fatal(err)
	}
	bpB, err := New(dial(t, natsURL))
	if err != nil {
		t.Fatal(err)
	}

	srvA := server.New(server.Options{OriginPatterns: []string{"*"}, Backplane: bpA})
	srvB := server.New(server.Options{OriginPatterns: []string{"*"}, Backplane: bpB})
	httpA := httptest.NewServer(srvA.Handler())
	httpB := httptest.NewServer(srvB.Handler())
	t.Cleanup(func() { httpA.Close(); _ = srvA.Close(context.Background()) })
	t.Cleanup(func() { httpB.Close(); _ = srvB.Close(context.Background()) })

	wsA := "ws" + strings.TrimPrefix(httpA.URL, "http")
	wsB := "ws" + strings.TrimPrefix(httpB.URL, "http")

	docA := ygo.NewDoc()
	c1, err := client.New(client.Options{URL: wsA, DocName: "shared", Doc: docA})
	if err != nil {
		t.Fatal(err)
	}
	docB := ygo.NewDoc()
	c2, err := client.New(client.Options{URL: wsB, DocName: "shared", Doc: docB})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c1.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	if err := c2.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	waitSynced(t, c1)
	waitSynced(t, c2)

	// Edit on client 1; the client sends it to server A, which publishes over
	// NATS to server B, which broadcasts it to client 2.
	m := ygo.NewMap(docA, "m")
	txn := docA.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()

	// Poll client 2's document until the edit lands (reads under a ReadTxn,
	// which is required off a live client's apply goroutine).
	mB := ygo.NewMap(docB, "m")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rtxn := docB.ReadTxn()
		got := mB.Get("k")
		rtxn.Close()
		if got == "v" {
			return // converged across the two instances via NATS
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("client 2 never converged on client 1's edit over the NATS backplane")
}

// TestBackplane_ServerConvergenceOverJetStream is the end-to-end proof for the
// JetStream adapter: two ygo servers, each with its own JetStream backplane on
// a shared JetStream-enabled NATS server, converge on an edit. The update
// travels client -> serverA -> JetStream stream -> serverB -> client.
func TestBackplane_ServerConvergenceOverJetStream(t *testing.T) {
	natsURL := runJetStreamServer(t)

	ctx0, cancel0 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel0()
	bpA, err := NewJetStream(ctx0, dial(t, natsURL))
	if err != nil {
		t.Fatal(err)
	}
	bpB, err := NewJetStream(ctx0, dial(t, natsURL))
	if err != nil {
		t.Fatal(err)
	}

	srvA := server.New(server.Options{OriginPatterns: []string{"*"}, Backplane: bpA})
	srvB := server.New(server.Options{OriginPatterns: []string{"*"}, Backplane: bpB})
	httpA := httptest.NewServer(srvA.Handler())
	httpB := httptest.NewServer(srvB.Handler())
	t.Cleanup(func() { httpA.Close(); _ = srvA.Close(context.Background()) })
	t.Cleanup(func() { httpB.Close(); _ = srvB.Close(context.Background()) })

	wsA := "ws" + strings.TrimPrefix(httpA.URL, "http")
	wsB := "ws" + strings.TrimPrefix(httpB.URL, "http")

	docA := ygo.NewDoc()
	c1, err := client.New(client.Options{URL: wsA, DocName: "shared", Doc: docA})
	if err != nil {
		t.Fatal(err)
	}
	docB := ygo.NewDoc()
	c2, err := client.New(client.Options{URL: wsB, DocName: "shared", Doc: docB})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c1.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	if err := c2.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	waitSynced(t, c1)
	waitSynced(t, c2)

	m := ygo.NewMap(docA, "m")
	txn := docA.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()

	mB := ygo.NewMap(docB, "m")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rtxn := docB.ReadTxn()
		got := mB.Get("k")
		rtxn.Close()
		if got == "v" {
			return // converged across the two instances via JetStream
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("client 2 never converged on client 1's edit over the JetStream backplane")
}

func waitSynced(t *testing.T, c *client.Client) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.Synced() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("client never reached synced state")
}
