package benchmarks

import (
	"testing"

	"github.com/Deln0r/ygo"
)

// Typing into formatted text. The document has 300 runs of 20 characters,
// alternately bold and plain (about 900 items: an opening marker, the
// text and a closing marker per bold run). Each benchmark types 600
// characters, one transaction per keystroke, starting at the end of the
// 150th run, as an editor does with a cursor in the middle of a document.
// yjs walks the text from its start for every insert into formatted text,
// so these measure the cost of that walk.

func richTextDoc() (*ygo.Doc, *ygo.Text) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	t := ygo.NewText(d, "doc")
	txn := d.WriteTxn()
	var idx uint64
	for run := 0; run < 300; run++ {
		attrs := ygo.Attrs{}
		if run%2 == 0 {
			attrs["bold"] = true
		}
		_ = t.InsertWithAttributes(txn, idx, "abcdefghijklmnopqrst", attrs)
		idx += 20
	}
	txn.Commit()
	return d, t
}

const richTextCursor = 150 * 20

func BenchmarkRichText_TypePlain(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		d, t := richTextDoc()
		b.StartTimer()
		for k := uint64(0); k < 600; k++ {
			txn := d.WriteTxn()
			_ = t.Insert(txn, richTextCursor+k, "x")
			txn.Commit()
		}
	}
}

func BenchmarkRichText_TypeWithAttributes(b *testing.B) {
	attrs := ygo.Attrs{"italic": true}
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		d, t := richTextDoc()
		b.StartTimer()
		for k := uint64(0); k < 600; k++ {
			txn := d.WriteTxn()
			_ = t.InsertWithAttributes(txn, richTextCursor+k, "x", attrs)
			txn.Commit()
		}
	}
}
