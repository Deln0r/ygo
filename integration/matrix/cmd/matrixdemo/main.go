// Command matrixdemo proves two ygo peers converge through a Matrix room.
//
// It is a check, not a printout: two peers edit while they cannot see each
// other, publish in the inconvenient order, read the room back, and the
// program exits non-zero unless both documents match AND both edits survived.
// Convergence on an empty document is also convergence, so "not empty" and
// "both edits present" are separate assertions.
//
// Usage (with testdata/up.sh running):
//
//	go run ./cmd/matrixdemo
//	go run ./cmd/matrixdemo -homeserver http://localhost:8008
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"maunium.net/go/mautrix"

	ygo "github.com/Deln0r/ygo"
	ymatrix "github.com/Deln0r/ygo/integration/matrix"
)

func main() {
	hs := flag.String("homeserver", "http://localhost:8008", "Matrix homeserver base URL")
	flag.Parse()

	if err := run(*hs); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("OK: two peers converged through a Matrix room, both edits intact")
}

func run(hs string) error {
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	alice, err := account(ctx, hs, fmt.Sprintf("demo_a%d", stamp))
	if err != nil {
		return fmt.Errorf("register alice: %w", err)
	}
	bob, err := account(ctx, hs, fmt.Sprintf("demo_b%d", stamp))
	if err != nil {
		return fmt.Errorf("register bob: %w", err)
	}

	created, err := alice.CreateRoom(ctx, &mautrix.ReqCreateRoom{Preset: "public_chat", Name: "ygo demo"})
	if err != nil {
		return fmt.Errorf("create room: %w", err)
	}
	if _, err := bob.JoinRoomByID(ctx, created.RoomID); err != nil {
		return fmt.Errorf("bob join: %w", err)
	}
	fmt.Printf("room: %s\n", created.RoomID)

	// Each peer edits offline - neither has seen the other.
	docA, err := edit(101, 0, "hello ")
	if err != nil {
		return err
	}
	docB, err := edit(202, 0, "world")
	if err != nil {
		return err
	}

	trA, err := ymatrix.New(alice, created.RoomID)
	if err != nil {
		return err
	}
	trB, err := ymatrix.New(bob, created.RoomID)
	if err != nil {
		return err
	}

	// Inconvenient order on purpose: the second peer publishes first.
	if _, err := trB.PublishDoc(ctx, docB); err != nil {
		return fmt.Errorf("bob publish: %w", err)
	}
	if _, err := trA.PublishDoc(ctx, docA); err != nil {
		return fmt.Errorf("alice publish: %w", err)
	}
	fmt.Println("published: bob first, then alice")

	want := len("hello world")
	gotA, err := syncUntil(ctx, trA, want)
	if err != nil {
		return fmt.Errorf("alice sync: %w", err)
	}
	gotB, err := syncUntil(ctx, trB, want)
	if err != nil {
		return fmt.Errorf("bob sync: %w", err)
	}
	fmt.Printf("alice reads: %q\nbob reads:   %q\n", gotA, gotB)

	if gotA != gotB {
		return fmt.Errorf("peers diverged: %q vs %q", gotA, gotB)
	}
	if gotA == "" {
		return fmt.Errorf("both peers converged on an empty document, which proves nothing")
	}
	if len(gotA) != want {
		return fmt.Errorf("converged on %q, but both edits must survive (%d chars expected)", gotA, want)
	}
	return nil
}

func account(ctx context.Context, hs, localpart string) (*mautrix.Client, error) {
	anon, err := mautrix.NewClient(hs, "", "")
	if err != nil {
		return nil, err
	}
	resp, err := anon.RegisterDummy(ctx, &mautrix.ReqRegister[any]{
		Username: localpart,
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		return nil, err
	}
	return mautrix.NewClient(hs, resp.UserID, resp.AccessToken)
}

func edit(clientID uint64, at int, s string) (*ygo.Doc, error) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	txt := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := txt.Insert(txn, uint64(at), s); err != nil {
		return nil, err
	}
	txn.Commit()
	return d, nil
}

// syncUntil polls until the document is complete or the deadline passes -
// Matrix is eventually consistent, so one round trip is not a guarantee.
//
// Two things it deliberately does not do. It does not swallow sync errors: a
// demo that prints its own errors to stderr and then reports OK is a demo that
// passes while the transport is broken. And it does not accumulate into one
// document across attempts - each attempt reads into a fresh one, so reaching
// the target means a SINGLE sync read the whole room rather than two partial
// ones adding up.
func syncUntil(ctx context.Context, tr *ymatrix.Transport, wantLen int) (string, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		doc := ygo.NewDoc()
		if _, err := tr.Sync(ctx, doc); err != nil {
			// A room joined a moment ago may not be in the very next /sync;
			// the transport is right to call it unavailable, and a caller is
			// right to poll through it. Every other error is fatal.
			if errors.Is(err, ymatrix.ErrRoomUnavailable) && time.Now().Before(deadline) {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return "", err
		}
		txt := ygo.NewText(doc, "t")
		rt := doc.ReadTxn()
		got := txt.String()
		rt.Close()
		if len(got) >= wantLen || time.Now().After(deadline) {
			return got, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}
