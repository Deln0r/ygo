// yload is a WebSocket load generator for a ygo/yserve deployment, built on
// the public ygo client. It opens rooms×conns real sync clients (full
// handshake), designates the first W conns of each room as writers that push
// a timestamp string every interval, and measures write→observe propagation
// latency on every connected client (each observation is a real server
// round-trip: the broadcast includes the writer itself).
//
// Usage:
//
//	yload -url ws://127.0.0.1:8080 -rooms 10 -conns 20 -writers 2 \
//	      -interval 500ms -duration 30s -connrate 100
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	ygo "github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/client"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080", "server base URL (ws:// or wss://, path prefix included if any)")
	rooms := flag.Int("rooms", 10, "number of distinct documents")
	conns := flag.Int("conns", 20, "connections per room")
	writers := flag.Int("writers", 2, "writer connections per room (rest are readers)")
	interval := flag.Duration("interval", 500*time.Millisecond, "write interval per writer")
	duration := flag.Duration("duration", 30*time.Second, "writing-phase duration")
	connrate := flag.Int("connrate", 100, "connection ramp rate per second")
	prefix := flag.String("prefix", "load", "docName prefix (rooms are <prefix>-<n>)")
	flag.Parse()

	var (
		attempted    atomic.Int64
		connected    atomic.Int64
		syncedCnt    atomic.Int64
		connectErrs  atomic.Int64
		runtimeErrs  atomic.Int64
		writesSent   atomic.Int64
		observations atomic.Int64

		latMu sync.Mutex
		lats  []time.Duration
	)

	total := *rooms * *conns
	fmt.Printf("yload: %d rooms x %d conns = %d clients -> %s (writers %d/room, every %v for %v)\n",
		*rooms, *conns, total, *url, *writers, *interval, *duration)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gap := time.Second / time.Duration(*connrate)
	var wg sync.WaitGroup
	clients := make(chan *client.Client, total)

	for r := 0; r < *rooms; r++ {
		for i := 0; i < *conns; i++ {
			r, i := r, i
			attempted.Add(1)
			wg.Add(1)
			time.Sleep(gap) // ramp
			go func() {
				defer wg.Done()
				room := fmt.Sprintf("%s-%d", *prefix, r)
				d := ygo.NewDoc()
				arr := ygo.NewArray(d, "load")
				// Observe before connecting; runs under the doc write lock,
				// so it only parses and records.
				arr.Observe(func(e *ygo.ArrayEvent) {
					now := time.Now()
					for _, op := range e.Delta {
						for _, v := range op.Insert {
							s, ok := v.(string)
							if !ok {
								continue
							}
							ns, err := strconv.ParseInt(s, 10, 64)
							if err != nil {
								continue
							}
							lat := now.Sub(time.Unix(0, ns))
							if lat < 0 || lat > time.Minute {
								continue // clock nonsense; drop
							}
							observations.Add(1)
							latMu.Lock()
							lats = append(lats, lat)
							latMu.Unlock()
						}
					}
				})

				c, err := client.New(client.Options{
					URL:     *url,
					DocName: room,
					Doc:     d,
					OnError: func(error) { runtimeErrs.Add(1) },
				})
				if err != nil {
					connectErrs.Add(1)
					return
				}
				// ctx governs the whole session lifetime (dial, handshake,
				// reconnect loop), so pass the long-lived run ctx.
				if err := c.Connect(ctx); err != nil {
					connectErrs.Add(1)
					return
				}
				connected.Add(1)
				clients <- c

				// Wait for the initial sync (bounded).
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) && !c.Synced() {
					time.Sleep(25 * time.Millisecond)
				}
				if c.Synced() {
					syncedCnt.Add(1)
				}

				if i < *writers {
					tick := time.NewTicker(*interval)
					defer tick.Stop()
					stop := time.After(*duration)
					for {
						select {
						case <-tick.C:
							txn := d.WriteTxn()
							arr.Push(txn, strconv.FormatInt(time.Now().UnixNano(), 10))
							txn.Commit()
							writesSent.Add(1)
						case <-stop:
							return
						case <-ctx.Done():
							return
						}
					}
				}
				// Readers just hold the connection through the writing phase.
				select {
				case <-time.After(*duration + 5*time.Second):
				case <-ctx.Done():
				}
			}()
		}
	}

	wg.Wait()
	// Grace period for the last broadcasts to land.
	time.Sleep(2 * time.Second)
	cancel()
	close(clients)
	for c := range clients {
		_ = c.Close()
	}

	latMu.Lock()
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	pct := func(p float64) time.Duration {
		if len(lats) == 0 {
			return 0
		}
		idx := int(p * float64(len(lats)-1))
		return lats[idx]
	}
	p50, p90, p99 := pct(0.50), pct(0.90), pct(0.99)
	var max time.Duration
	if len(lats) > 0 {
		max = lats[len(lats)-1]
	}
	latMu.Unlock()

	fmt.Printf("\n=== RESULTS ===\n")
	fmt.Printf("clients: attempted %d, connected %d, synced %d\n", attempted.Load(), connected.Load(), syncedCnt.Load())
	fmt.Printf("errors: connect %d, runtime %d\n", connectErrs.Load(), runtimeErrs.Load())
	fmt.Printf("writes sent: %d, observations: %d\n", writesSent.Load(), observations.Load())
	fmt.Printf("propagation latency: p50 %v, p90 %v, p99 %v, max %v\n", p50, p90, p99, max)

	if connectErrs.Load() > 0 || syncedCnt.Load() < connected.Load() {
		os.Exit(1)
	}
}
