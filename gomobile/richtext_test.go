package gomobile_test

import (
	"testing"

	"github.com/Deln0r/ygo/gomobile"
)

// TestMobile_ApplyDelta_InsertClassification locks the insert-value
// classification: a string is text, a JSON object is an embed, and a
// null insert is malformed (surfaced as an error, not silently dropped).
func TestMobile_ApplyDelta_InsertClassification(t *testing.T) {
	d := gomobile.NewDoc()
	tx := d.Text("note")

	// A null insert is malformed and must error.
	if err := tx.ApplyDelta([]byte(`[{"insert":null}]`)); err == nil {
		t.Error("ApplyDelta(insert:null) = nil error, want an error")
	}

	// A string insert is text.
	if err := tx.ApplyDelta([]byte(`[{"insert":"hi"}]`)); err != nil {
		t.Fatalf("ApplyDelta(string): %v", err)
	}
	if tx.String() != "hi" {
		t.Errorf("text = %q, want hi", tx.String())
	}

	// An object insert is an embed, advancing length by one unit.
	if err := tx.ApplyDelta([]byte(`[{"retain":2},{"insert":{"image":"x.png"}}]`)); err != nil {
		t.Fatalf("ApplyDelta(embed): %v", err)
	}
	if tx.Length() != 3 {
		t.Errorf("length after embed = %d, want 3", tx.Length())
	}
}
