package nats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Deln0r/ygo/server/backplane"
)

// Compile-time guarantee that the JetStream adapter satisfies the ygo
// Backplane contract, like the core-NATS adapter.
var _ backplane.Backplane = (*JetStream)(nil)

// DefaultStreamName is the base of the JetStream stream name. When
// WithStreamName is not given, the stream is named DefaultStreamName + "_" +
// the (sanitized) subject prefix, so deployments that use different prefixes on
// one NATS system do not collide on a single stream.
const DefaultStreamName = "YGO_BACKPLANE"

const (
	// defaultJSMaxAge bounds how long a relayed delta is retained in the
	// stream. A reconnect or server restart within this window resumes with no
	// loss; beyond it, pruned deltas are backstopped by the shared Store when a
	// document next loads.
	defaultJSMaxAge = 10 * time.Minute
	// streamOpTimeout bounds the JetStream control-plane calls (stream ensure,
	// consumer create) made from the ctx-less Subscribe path.
	streamOpTimeout = 10 * time.Second
)

// JetStream is a JetStream-backed Backplane for the ygo server. Unlike the
// core-NATS Backplane (at-most-once, fire-and-forget), it publishes each update
// into a persistent (file-backed) stream and consumes it with a JetStream
// ordered consumer, which the client automatically recreates and RESUMES from
// where it left off after a NATS reconnect or a JetStream server restart (from
// the pinned start sequence before the first delivery, then from the last
// delivered message). So a transient NATS outage does not silently and
// permanently stop delivery — the failure the core adapter leaves you exposed to
// (there a dropped delta parks every later edit from a client on a hot,
// never-reloaded document).
//
// No-loss is bounded by stream retention (WithJSMaxAge, default 10m): a
// reconnect or restart within that window redelivers everything published
// during the outage; a longer outage prunes older deltas from the stream, and
// those are backstopped by the shared Store when the document next loads (as
// with the core adapter). A consumer reset may redeliver from its resume point,
// so a message can arrive more than once; this is safe because applying a ygo
// update is idempotent and commutative (a duplicate is a no-op), as is an
// awareness update (last-writer-wins by clock).
//
// Construct with NewJetStream; it satisfies backplane.Backplane and is safe for
// concurrent use. A shared Store is still required, exactly as for the core
// adapter (foreign updates are applied in memory only, not re-persisted).
type JetStream struct {
	js         jetstream.JetStream
	streamName string
	prefix     string
	origin     string // unique per instance; filters this instance's own publishes
	maxAge     time.Duration

	mu     sync.Mutex
	subs   map[*jsSub]struct{}
	closed bool
}

type jsSub struct {
	cc jetstream.ConsumeContext
}

// JSOption configures a JetStream backplane.
type JSOption func(*JetStream)

// WithJSPrefix sets the NATS subject prefix (default "ygo"). Every instance
// sharing documents must use the same prefix, and the stream must capture it.
func WithJSPrefix(p string) JSOption { return func(j *JetStream) { j.prefix = p } }

// WithStreamName overrides the stream name (default DefaultStreamName + "_" +
// the sanitized prefix). Instances sharing documents must resolve to the same
// stream name; independent deployments on one NATS system must use different
// names (or different prefixes, which the default name already distinguishes).
func WithStreamName(name string) JSOption { return func(j *JetStream) { j.streamName = name } }

// WithJSMaxAge bounds how long the stream retains a delta (default 10m). It
// bounds both storage and the loss-free reconnect window. This is a stream-wide
// setting fixed when the stream is first created; see NewJetStream.
func WithJSMaxAge(d time.Duration) JSOption { return func(j *JetStream) { j.maxAge = d } }

// NewJetStream returns a JetStream-backed Backplane over nc. The caller owns nc
// (its authentication, TLS, reconnect policy, and lifetime); Close releases
// only this Backplane's consumers and does NOT close nc. NewJetStream ensures
// the backing stream exists — creating it (file-backed) to capture "<prefix>.>"
// if absent, or reusing an existing stream after checking it already carries
// those subjects (it never rewrites a shared stream's config) — which requires
// JetStream to be enabled on the NATS server; ctx bounds that setup. Because the
// stream is created once and never updated, WithJSMaxAge takes effect only on
// the instance that first creates the stream; later instances reuse whatever
// config it was created with. Each instance takes a fresh random identity so it
// never processes its own publishes.
func NewJetStream(ctx context.Context, nc *nats.Conn, opts ...JSOption) (*JetStream, error) {
	if nc == nil {
		return nil, errors.New("nats jetstream backplane: nil connection")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("nats jetstream backplane: %w", err)
	}
	j := &JetStream{
		js:     js,
		prefix: DefaultPrefix,
		origin: newOrigin(),
		maxAge: defaultJSMaxAge,
		subs:   map[*jsSub]struct{}{},
	}
	for _, o := range opts {
		o(j)
	}
	if j.prefix == "" {
		return nil, errors.New("nats jetstream backplane: empty prefix")
	}
	if j.streamName == "" {
		j.streamName = DefaultStreamName + "_" + sanitizeStreamToken(j.prefix)
	}
	if err := j.ensureStream(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

// ensureStream creates the backing (file-backed) stream, or reuses an existing
// one after verifying it carries this backplane's subjects, and records the
// handle. It deliberately never UPDATEs an existing stream: a shared stream must
// not be silently reconfigured by whichever instance happened to boot last (that
// would drift MaxAge for everyone, or, with a colliding name across deployments,
// rewrite another deployment's subjects out from under it).
func (j *JetStream) ensureStream(ctx context.Context) error {
	wantSubject := j.prefix + ".>"
	_, err := j.js.CreateStream(ctx, jetstream.StreamConfig{
		Name:      j.streamName,
		Subjects:  []string{wantSubject},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    j.maxAge,
		Storage:   jetstream.FileStorage,
		Discard:   jetstream.DiscardOld,
	})
	if err == nil {
		return nil
	}
	if !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		return fmt.Errorf("nats jetstream backplane: create stream %q: %w", j.streamName, err)
	}
	// The stream already exists (a peer instance, or a prior boot, created it).
	// Reuse it as-is if it can carry our subjects; otherwise fail fast so a
	// name collision across deployments surfaces instead of silently breaking.
	existing, gerr := j.js.Stream(ctx, j.streamName)
	if gerr != nil {
		return fmt.Errorf("nats jetstream backplane: inspect stream %q: %w", j.streamName, gerr)
	}
	if !streamCoversSubject(existing.CachedInfo().Config.Subjects, wantSubject) {
		return fmt.Errorf("nats jetstream backplane: stream %q exists with subjects %v that do not cover %q; use WithStreamName for an independent deployment",
			j.streamName, existing.CachedInfo().Config.Subjects, wantSubject)
	}
	return nil
}

// streamCoversSubject reports whether an existing stream's subjects carry want
// (either the exact subject or a catch-all ">").
func streamCoversSubject(existing []string, want string) bool {
	for _, s := range existing {
		if s == want || s == ">" {
			return true
		}
	}
	return false
}

// sanitizeStreamToken maps a subject prefix to the JetStream stream-name
// alphabet ([A-Za-z0-9_-]); any other byte becomes '_'. Distinct prefixes can
// still collide after sanitizing (e.g. "a.b" and "a_b"), which ensureStream's
// subject check then rejects rather than reusing the wrong stream.
func sanitizeStreamToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (j *JetStream) subject(docName string) string { return docSubject(j.prefix, docName) }

// Publish stores update on docName's subject in the stream, tagged with this
// instance's origin, and returns once JetStream has acknowledged the write
// (durability) or ctx is done. Every OTHER instance holding the document
// receives it; this instance's own consumer skips it by origin.
func (j *JetStream) Publish(ctx context.Context, docName string, update []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.mu.Lock()
	closed := j.closed
	j.mu.Unlock()
	if closed {
		return ErrClosed
	}
	msg := nats.NewMsg(j.subject(docName))
	msg.Data = update
	msg.Header.Set(originHeader, j.origin)
	_, err := j.js.PublishMsg(ctx, msg)
	return err
}

// Subscribe delivers foreign updates for docName (published by other instances)
// to onUpdate, returning an unsubscribe func. It uses a JetStream ordered
// consumer filtered to docName's subject, starting just after the stream's
// current end so only new messages are delivered (the stream already holds prior
// state via the shared Store, and the server subscribes before it loads, so an
// update arriving during the load is still captured by the stream and delivered
// here — no lost-during-load window). The start point is pinned to that
// sequence rather than "new", so the ordered consumer — which transparently
// recreates itself across a NATS reconnect or server restart — resumes from the
// pinned point even if the reset happens before the first delivery, not just
// after; delivery is therefore not silently lost on a transient outage (bounded
// by the stream's retention). onUpdate runs on the consumer's delivery
// goroutine, one call at a time in order; a reset may redeliver a message (safe,
// applies are idempotent). unsub is safe to call more than once.
func (j *JetStream) Subscribe(docName string, onUpdate func(update []byte)) (func(), error) {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return nil, ErrClosed
	}
	j.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), streamOpTimeout)
	defer cancel()
	// Pin the start to the message after the stream's current end. Ordered
	// consumers only resume from cursor.streamSeq+1 once a message has been
	// delivered; before that, a DeliverNew consumer that resets would jump to
	// the (new) stream end and skip anything published in between. Starting from
	// an explicit sequence closes that pre-first-delivery window. A fresh stream
	// handle is fetched per call so concurrent Subscribes never share (and race
	// on) one handle's cached info.
	stream, err := j.js.Stream(ctx, j.streamName)
	if err != nil {
		return nil, fmt.Errorf("nats jetstream backplane: stream lookup: %w", err)
	}
	cons, err := j.js.OrderedConsumer(ctx, j.streamName, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{j.subject(docName)},
		DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:    stream.CachedInfo().State.LastSeq + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("nats jetstream backplane: create consumer: %w", err)
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		// Ordered consumers do not ack; the library tracks the delivered
		// sequence itself, so skipping our own publish here does not disturb
		// its cursor.
		if m.Headers().Get(originHeader) == j.origin {
			return // our own publish echoed back
		}
		onUpdate(m.Data())
	})
	if err != nil {
		return nil, fmt.Errorf("nats jetstream backplane: consume: %w", err)
	}

	sub := &jsSub{cc: cc}
	j.mu.Lock()
	if j.closed {
		// Closed between the guard above and registration; do not leak.
		j.mu.Unlock()
		cc.Stop()
		return nil, ErrClosed
	}
	j.subs[sub] = struct{}{}
	j.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			j.mu.Lock()
			delete(j.subs, sub)
			j.mu.Unlock()
			cc.Stop()
		})
	}
	return unsub, nil
}

// Close stops every consumer this Backplane still holds and blocks further
// Publish/Subscribe (ErrClosed). It does NOT close the underlying NATS
// connection or delete the stream, both of which the caller owns. Idempotent.
func (j *JetStream) Close() error {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return nil
	}
	j.closed = true
	subs := make([]*jsSub, 0, len(j.subs))
	for s := range j.subs {
		subs = append(subs, s)
	}
	j.subs = map[*jsSub]struct{}{}
	j.mu.Unlock()

	for _, s := range subs {
		s.cc.Stop()
	}
	return nil
}
