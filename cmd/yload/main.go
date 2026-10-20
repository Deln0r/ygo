// yload is a WebSocket load generator for a ygo/yserve deployment, built on
// the public ygo client. It opens rooms×conns real sync clients (full
// handshake), designates the first W conns of each room as writers that push
// a timestamp string every interval, and measures write→observe propagation
// latency on the room's other clients. Each write carries the run's id and
// its writer's id, so a client counts neither its own insert nor content an
// earlier run left in the room: every sample is a delivery through the
// server. Writing starts only when every client is connected and synced (a
// run where that fails stops before writing), and the report gives the
// deliveries expected, made and missing next to the latency figures.
//
// With -slow N the last N connections of each room are slow readers instead:
// raw WebSocket connections that consume -slowrate bytes per second, a little
// every 10 ms (0 stops reading after the first message). They
// are left out of the latency figures, which then show what the room's other
// clients see while a slow client is connected. -payload pads every write, so
// a slow reader fills the socket buffers sooner.
//
// Usage:
//
//	yload -url ws://127.0.0.1:8080 -rooms 10 -conns 20 -writers 2 \
//	      -interval 500ms -duration 30s -connrate 100
//	yload -rooms 1 -conns 10 -slow 1 -slowrate 0 -payload 16384 -interval 50ms
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

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
	slow := flag.Int("slow", 0, "slow readers per room (the last conns of each room; left out of the latency figures)")
	slowrate := flag.Int("slowrate", 0, "bytes per second a slow reader consumes; 0 stops reading after the first message")
	payload := flag.Int("payload", 0, "bytes of padding added to every write")
	grace := flag.Duration("grace", 15*time.Second, "how long to wait after writing for the last deliveries")
	flag.Parse()
	if *slow < 0 || *slow > *conns-*writers {
		fmt.Fprintf(os.Stderr, "yload: -slow must be between 0 and conns-writers (%d)\n", *conns-*writers)
		os.Exit(2)
	}
	padding := ""
	if *payload > 0 {
		padding = ":" + strings.Repeat("x", *payload)
	}
	runID := strconv.FormatInt(time.Now().UnixNano(), 36) // tells this run's writes from earlier ones

	var (
		attempted   atomic.Int64
		connected   atomic.Int64
		syncedCnt   atomic.Int64
		connectErrs atomic.Int64
		runtimeErrs atomic.Int64
		writesSent  atomic.Int64
		delivered   atomic.Int64

		latMu sync.Mutex
		lats  []time.Duration

		errMu      sync.Mutex
		errSamples = map[string]int{}

		slowConnected atomic.Int64
		slowFailed    atomic.Int64 // slow readers that could not connect or read their first message
		slowMu        sync.Mutex
		slowCloses    = map[string]int{} // how the server ended each slow reader

		roomWrites = make([]atomic.Int64, *rooms) // writes made in each room
		roomSynced = make([]atomic.Int64, *rooms) // synced (non-slow) clients in each room

		fastMu sync.Mutex
		fast   []*client.Client // every connected non-slow client, for the barrier's live check
	)
	noteErr := func(err error) {
		runtimeErrs.Add(1)
		msg := err.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		errMu.Lock()
		errSamples[msg]++
		errMu.Unlock()
	}

	total := *rooms * *conns
	fmt.Printf("yload: %d rooms x %d conns = %d clients -> %s (writers %d/room, every %v for %v)\n",
		*rooms, *conns, total, *url, *writers, *interval, *duration)
	if *slow > 0 {
		fmt.Printf("slow readers: %d/room at %d B/s, payload %d B\n", *slow, *slowrate, *payload)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Writing starts when every client is connected and synced, so no write
	// races a client's initial sync; the main goroutine opens start once the
	// last client is ready, or stops the run if one is not.
	var ready sync.WaitGroup
	ready.Add(total)
	start := make(chan struct{})

	gap := time.Second / time.Duration(*connrate)
	var wg, writersWG sync.WaitGroup
	clients := make(chan *client.Client, total)

	for r := 0; r < *rooms; r++ {
		for i := 0; i < *conns; i++ {
			r, i := r, i
			attempted.Add(1)
			wg.Add(1)
			if i < *writers {
				writersWG.Add(1)
			}
			time.Sleep(gap) // ramp
			go func() {
				defer wg.Done()
				writingDone := func() {}
				if i < *writers {
					writingDone = sync.OnceFunc(writersWG.Done)
					defer writingDone()
				}
				readyDone := sync.OnceFunc(ready.Done)
				defer readyDone()
				room := fmt.Sprintf("%s-%d", *prefix, r)
				if i >= *conns-*slow {
					how := slowReader(ctx, strings.TrimRight(*url, "/")+"/"+room, *slowrate, readyDone, &slowConnected, &slowFailed)
					slowMu.Lock()
					slowCloses[how]++
					slowMu.Unlock()
					return
				}
				id := runID + "-" + fmt.Sprintf("%d-%d", r, i)
				d := ygo.NewDoc()
				arr := ygo.NewArray(d, "load")
				// Observe before connecting; runs under the doc write lock,
				// so it only parses and records. A write is "<ns>|<id>",
				// optionally followed by ":" and padding.
				arr.Observe(func(e *ygo.ArrayEvent) {
					now := time.Now()
					for _, op := range e.Delta {
						for _, v := range op.Insert {
							s, ok := v.(string)
							if !ok {
								continue
							}
							if j := strings.IndexByte(s, ':'); j >= 0 {
								s = s[:j]
							}
							ts, writer, ok := strings.Cut(s, "|")
							if !ok || writer == id || !strings.HasPrefix(writer, runID+"-") {
								continue // this client's own insert, or not this run's write
							}
							delivered.Add(1) // counted however late it is
							ns, err := strconv.ParseInt(ts, 10, 64)
							if err != nil {
								continue
							}
							if lat := now.Sub(time.Unix(0, ns)); lat >= 0 { // a negative one is clock skew
								latMu.Lock()
								lats = append(lats, lat)
								latMu.Unlock()
							}
						}
					}
				})

				c, err := client.New(client.Options{
					URL:     *url,
					DocName: room,
					Doc:     d,
					OnError: noteErr,
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
				fastMu.Lock()
				fast = append(fast, c)
				fastMu.Unlock()

				// Wait for the initial sync (bounded).
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) && !c.Synced() {
					time.Sleep(25 * time.Millisecond)
				}
				if c.Synced() {
					syncedCnt.Add(1)
					roomSynced[r].Add(1)
				}
				readyDone()
				select {
				case <-start:
				case <-ctx.Done():
					return
				}

				if i < *writers {
					tick := time.NewTicker(*interval)
					defer tick.Stop()
					stop := time.After(*duration)
				writing:
					for {
						select {
						case <-tick.C:
							txn := d.WriteTxn()
							arr.Push(txn, strconv.FormatInt(time.Now().UnixNano(), 10)+"|"+id+padding)
							txn.Commit()
							writesSent.Add(1)
							roomWrites[r].Add(1)
						case <-stop:
							break writing
						case <-ctx.Done():
							return
						}
					}
					writingDone()
				}
				// Every client holds its connection until the run ends.
				<-ctx.Done()
			}()
		}
	}

	// The barrier's clock starts once every client is launched, so a slow
	// ramp does not eat into it.
	readyCh := make(chan struct{})
	go func() { ready.Wait(); close(readyCh) }()
	select {
	case <-readyCh:
	case <-time.After(60 * time.Second):
		fmt.Fprintf(os.Stderr, "yload: not every client was ready 60s after the last one started; stopping before writing\n")
		cancel()
		os.Exit(1)
	}
	// Synced now, not merely once: a client that lost its connection during
	// a long ramp would miss the start.
	live := 0
	fastMu.Lock()
	for _, c := range fast {
		if c.Synced() {
			live++
		}
	}
	fastMu.Unlock()
	if want := *rooms * (*conns - *slow); connectErrs.Load() > 0 || live < want || slowFailed.Load() > 0 {
		fmt.Fprintf(os.Stderr, "yload: %d of %d clients synced, %d connect errors, %d slow readers failed; stopping before writing\n",
			live, want, connectErrs.Load(), slowFailed.Load())
		cancel()
		os.Exit(1)
	}
	close(start)
	writersWG.Wait()
	// A write reaches every synced client of its room but its writer.
	expected := func() int64 {
		var n int64
		for r := range roomWrites {
			if s := roomSynced[r].Load(); s > 1 {
				n += roomWrites[r].Load() * (s - 1)
			}
		}
		return n
	}
	deadline := time.Now().Add(*grace)
	for time.Now().Before(deadline) && delivered.Load() < expected() {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	wg.Wait()
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

	exp, got := expected(), delivered.Load()
	fmt.Printf("\n=== RESULTS ===\n")
	fmt.Printf("clients: attempted %d, connected %d, synced %d\n", attempted.Load(), connected.Load(), syncedCnt.Load())
	fmt.Printf("errors: connect %d, runtime %d\n", connectErrs.Load(), runtimeErrs.Load())
	errMu.Lock()
	for msg, n := range errSamples {
		fmt.Printf("  %dx %s\n", n, msg)
	}
	errMu.Unlock()
	fmt.Printf("writes sent: %d; deliveries: expected %d, delivered %d, missing %d\n", writesSent.Load(), exp, got, exp-got)
	fmt.Printf("propagation latency (deliveries through the server): p50 %v, p90 %v, p99 %v, max %v\n", p50, p90, p99, max)
	if *slow > 0 {
		slowMu.Lock()
		hows := make([]string, 0, len(slowCloses))
		for how, n := range slowCloses {
			hows = append(hows, fmt.Sprintf("%s %d", how, n))
		}
		slowMu.Unlock()
		sort.Strings(hows)
		fmt.Printf("slow readers: connected %d; ended: %s\n", slowConnected.Load(), strings.Join(hows, ", "))
	}

	if connectErrs.Load() > 0 || syncedCnt.Load() < connected.Load() || got < exp {
		os.Exit(1)
	}
}

// slowReader holds one slow connection to roomURL until ctx ends. While the
// run lasts it consumes rate bytes per second, a little every 10 ms and
// carrying fractions of a byte over, so the server meets a client that
// really reads at that rate; with rate 0 it reads only the first message.
// ready is called once the first message is read or the connection failed;
// a failure is counted in failed before that. Reads run on their own context,
// which outlives the run by 5 s: at the end the reader reads at full speed,
// finishing the message it is on, to learn whether the server closed the
// connection meanwhile. It reports "held" when the connection lasted the run,
// "close <code>" when the server closed it, "dial error" or "first read error"
// when it never got going, and "read error" otherwise.
func slowReader(ctx context.Context, roomURL string, rate int, ready func(), connected, failed *atomic.Int64) string {
	c, _, err := websocket.Dial(ctx, roomURL, nil)
	if err != nil {
		failed.Add(1)
		ready()
		return "dial error"
	}
	defer func() { _ = c.CloseNow() }()
	connected.Add(1)
	c.SetReadLimit(-1)
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(5*time.Second, rcancel) })
	defer stop()

	if _, _, err := c.Read(rctx); err != nil {
		failed.Add(1)
		ready()
		return "first read error"
	}
	ready()
	if rate <= 0 {
		<-ctx.Done()
		for {
			if _, _, err := c.Read(rctx); err != nil {
				return ended(rctx, err)
			}
		}
	}
	const tick = 10 * time.Millisecond
	perTick := float64(rate) * tick.Seconds()
	budget := 0.0 // bytes this reader may still consume; fractions carry over
	buf := make([]byte, int(perTick)+2)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		_, r, err := c.Reader(rctx)
		if err != nil {
			return ended(rctx, err)
		}
		for {
			want := len(buf)
			if ctx.Err() == nil { // the run is on: consume at the rate
				select {
				case <-ticker.C:
				case <-ctx.Done():
				}
			}
			if ctx.Err() == nil {
				budget += perTick
				if budget > float64(len(buf)) {
					budget = float64(len(buf)) // unused budget (after a short read) does not pile up into a burst
				}
				if budget < 1 {
					continue
				}
				want = int(budget)
			}
			n, err := r.Read(buf[:want])
			if ctx.Err() == nil {
				budget -= float64(n)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break // the message is consumed; take the next one
				}
				return ended(rctx, err)
			}
		}
	}
}

// ended names how a slow reader's read failed.
func ended(ctx context.Context, err error) string {
	if code := websocket.CloseStatus(err); code != -1 {
		return fmt.Sprintf("close %d", code)
	}
	if ctx.Err() != nil {
		return "held"
	}
	return "read error"
}
