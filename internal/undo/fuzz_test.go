package undo_test

import (
	"testing"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/undo"
)

// FuzzApplyUndoRedo applies arbitrary bytes to a doc that has an
// UndoManager attached, then undoes and redoes. ApplyUpdate runs under
// a nil-origin transaction, which a default UndoManager tracks, so this
// drives the manager's capture / undo / redo clock-range scans over
// whatever (possibly adversarial) structure the update integrated. The
// invariant is the usual one: never panic, hang, or OOM.
func FuzzApplyUndoRedo(f *testing.F) {
	// A valid update for a baseline.
	src := doc.NewDoc()
	src.Branch("t") // touch a root so the seed integrates something
	f.Add(encoding.EncodeStateAsUpdate(src))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	// The ApplyUpdate fuzz crasher: a Skip block at a multi-billion
	// clock. Before the fix the capture scan walked it clock by clock
	// (or wrapped to 0) and hung; this keeps that regression covered
	// through the undo path too.
	f.Add([]byte{0x01, 0x02, 0x30, 0x80, 0xff, 0xff, 0xff, 0x30, 0x00, 0x80, 0xde, 0x01, 0x00, 0x00, 0x00, 0xde, 0xde, 0xde})

	f.Fuzz(func(t *testing.T, data []byte) {
		d := doc.NewDoc()
		// Scope to a few common roots so the capture pass records, then
		// replays, real ranges.
		scope := []*block.Branch{d.Branch("t"), d.Branch("m"), d.Branch("a")}
		um := undo.NewUndoManager(d, scope)
		defer um.Close()

		_ = encoding.ApplyUpdate(d, data)
		um.Undo()
		um.Redo()
	})
}
