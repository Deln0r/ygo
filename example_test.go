package ygo_test

import (
	"fmt"

	"github.com/Deln0r/ygo"
)

// Example is the basic shape: create a document, edit a shared type
// inside a write transaction, and read the value back.
func Example() {
	doc := ygo.NewDoc()
	settings := ygo.NewMap(doc, "settings")

	txn := doc.WriteTxn()
	settings.Set(txn, "theme", "dark")
	settings.Set(txn, "fontSize", int64(14))
	txn.Commit()

	fmt.Println(settings.Get("theme"))
	fmt.Println(settings.Get("fontSize"))
	// Output:
	// dark
	// 14
}

// Example_array shows the shared Array: an ordered sequence you append to
// and read positionally.
func Example_array() {
	doc := ygo.NewDoc()
	todo := ygo.NewArray(doc, "todo")

	txn := doc.WriteTxn()
	todo.Push(txn, "buy milk", "write code")
	txn.Commit()

	fmt.Println(todo.Len())
	fmt.Println(todo.Get(0))
	fmt.Println(todo.Get(1))
	// Output:
	// 2
	// buy milk
	// write code
}

// Example_text shows the shared Text: a collaborative string edited by
// index. Inserts commute so concurrent edits converge.
func Example_text() {
	doc := ygo.NewDoc()
	note := ygo.NewText(doc, "note")

	txn := doc.WriteTxn()
	_ = note.Insert(txn, 0, "world")
	_ = note.Insert(txn, 0, "hello ")
	txn.Commit()

	fmt.Println(note.String())
	// Output:
	// hello world
}

// Example_sync shows the core CRDT property: two documents that never
// talked to each other each apply the other's update and converge on the
// same merged state, independent of order.
func Example_sync() {
	// Peer A sets a title on its copy.
	a := ygo.NewDoc()
	am := ygo.NewMap(a, "doc")
	wa := a.WriteTxn()
	am.Set(wa, "title", "Hello")
	wa.Commit()

	// Peer B sets a different key on its own copy.
	b := ygo.NewDoc()
	bm := ygo.NewMap(b, "doc")
	wb := b.WriteTxn()
	bm.Set(wb, "author", "Ada")
	wb.Commit()

	// Exchange full-state updates and apply each to the other.
	if err := ygo.ApplyUpdate(a, ygo.EncodeStateAsUpdate(b)); err != nil {
		panic(err)
	}
	if err := ygo.ApplyUpdate(b, ygo.EncodeStateAsUpdate(a)); err != nil {
		panic(err)
	}

	// Both converge to the same merged state.
	fmt.Println(am.Get("title"), am.Get("author"))
	fmt.Println(bm.Get("title"), bm.Get("author"))
	// Output:
	// Hello Ada
	// Hello Ada
}

// Example_undo shows the built-in UndoManager: track a shared type, edit
// it, then step backward and forward through the edit history.
func Example_undo() {
	doc := ygo.NewDoc()
	m := ygo.NewMap(doc, "doc")
	undo := ygo.NewUndoManager(doc, m)

	txn := doc.WriteTxn()
	m.Set(txn, "title", "draft")
	txn.Commit()
	fmt.Println(m.Get("title"))

	undo.Undo()
	fmt.Println(m.Has("title"))

	undo.Redo()
	fmt.Println(m.Get("title"))
	// Output:
	// draft
	// false
	// draft
}
