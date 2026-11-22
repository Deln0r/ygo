package doc

import "testing"

// After Commit the store records splits into no transaction. Encoding the
// committed transaction's update for an update handler, which runs after the
// merges, takes its Store again; that pointed the store at the committed
// transaction's list and kept the transaction alive while the document sat
// idle.
func TestCommit_StoreKeepsNoTransaction(t *testing.T) {
	prev := UpdateEncoder
	UpdateEncoder = func(tm *TransactionMut, _ bool) []byte {
		_ = tm.Store()
		return []byte{0, 0}
	}
	defer func() { UpdateEncoder = prev }()
	d := NewDoc()
	off := d.OnUpdate(func([]byte, any) {})
	defer off()
	txn := d.WriteTxn()
	txn.Commit()
	if !d.store.RecordingSplitsInto(nil) {
		t.Fatal("the store still records splits into a committed transaction")
	}
}
