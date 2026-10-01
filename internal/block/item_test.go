package block

import "testing"

func TestItem_LastID(t *testing.T) {
	cases := []struct {
		name string
		item Item
		want ID
	}{
		{"single element", Item{ID: ID{1, 5}, Len: 1}, ID{1, 5}},
		{"three elements", Item{ID: ID{1, 5}, Len: 3}, ID{1, 7}},
		{"len 100", Item{ID: ID{42, 0}, Len: 100}, ID{42, 99}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.LastID(); !got.Equal(tc.want) {
				t.Errorf("LastID=%v want %v", got, tc.want)
			}
		})
	}
}

func TestItem_Info(t *testing.T) {
	originID := ID{1, 0}
	rightOriginID := ID{1, 5}
	subKey := "color"

	cases := []struct {
		name string
		item Item
		want uint8
	}{
		{
			"string content, no flags",
			Item{
				ID:      ID{1, 0},
				Len:     5,
				Content: Content{Kind: KindString, Str: "hello"},
			},
			uint8(KindString), // 0b0000_0100 = 4
		},
		{
			"string content with origin",
			Item{
				ID:      ID{1, 0},
				Len:     5,
				Origin:  &originID,
				Content: Content{Kind: KindString},
			},
			InfoHasOrigin | uint8(KindString), // 0b1000_0100
		},
		{
			"all three presence flags + Any content",
			Item{
				ID:          ID{1, 0},
				Len:         3,
				Origin:      &originID,
				RightOrigin: &rightOriginID,
				ParentSub:   &subKey,
				Content:     Content{Kind: KindAny, Anys: []Any{1, 2, 3}},
			},
			InfoHasOrigin | InfoHasRightOrigin | InfoHasParentSub | uint8(KindAny), // 0b1110_1000
		},
		{
			"deleted content, no presence",
			Item{
				ID:      ID{1, 0},
				Len:     2,
				Content: Content{Kind: KindDeleted, DeletedLen: 2},
			},
			uint8(KindDeleted), // 0b0000_0001
		},
		{
			"max content kind (Move=11) + all presence",
			Item{
				ID:          ID{1, 0},
				Len:         1,
				Origin:      &originID,
				RightOrigin: &rightOriginID,
				ParentSub:   &subKey,
				Content:     Content{Kind: KindMove, Move: &Move{}},
			},
			InfoHasOrigin | InfoHasRightOrigin | InfoHasParentSub | 0b1011, // 0b1110_1011
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.Info(); got != tc.want {
				t.Errorf("Info=0b%08b (0x%02x) want 0b%08b (0x%02x)",
					got, got, tc.want, tc.want)
			}
		})
	}
}

func TestItem_FlagAccessors(t *testing.T) {
	it := &Item{}

	// All flags start clear.
	if it.IsDeleted() || it.IsCountable() || it.IsKeep() || it.IsLinked() {
		t.Fatal("fresh item must have all flags clear")
	}

	// Set each, verify each, leave others alone.
	it.SetDeleted(true)
	if !it.IsDeleted() || it.IsCountable() || it.IsKeep() || it.IsLinked() {
		t.Fatalf("after SetDeleted(true): flags=0b%016b", it.Flags)
	}

	it.SetCountable(true)
	if !it.IsDeleted() || !it.IsCountable() || it.IsKeep() || it.IsLinked() {
		t.Fatalf("after SetCountable(true): flags=0b%016b", it.Flags)
	}

	it.SetKeep(true)
	it.SetLinked(true)
	if !it.IsDeleted() || !it.IsCountable() || !it.IsKeep() || !it.IsLinked() {
		t.Fatalf("after all set: flags=0b%016b", it.Flags)
	}

	// Bit positions match yrs constants.
	want := FlagDeleted | FlagCountable | FlagKeep | FlagLinked
	if it.Flags != want {
		t.Errorf("flags=0b%016b want 0b%016b", it.Flags, want)
	}

	// Clear each, verify others remain.
	it.SetDeleted(false)
	if it.IsDeleted() {
		t.Error("SetDeleted(false) did not clear")
	}
	if !it.IsCountable() || !it.IsKeep() || !it.IsLinked() {
		t.Error("SetDeleted(false) cleared other flags")
	}
}

func TestItem_FlagBitsDoNotOverlap(t *testing.T) {
	all := []uint16{FlagKeep, FlagCountable, FlagDeleted, FlagMarked, FlagLinked}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if a&b != 0 {
				t.Errorf("flag bits overlap: 0b%016b & 0b%016b = 0b%016b", a, b, a&b)
			}
		}
	}
}

func TestInfoBits_NoOverlapWithContentNibble(t *testing.T) {
	// Wire info byte presence flags must live in bits 5-7 only.
	// Content kinds are 0..11 = 4 bits, occupying bits 0-3.
	// Bit 4 is currently unused. Verify presence flags don't leak.
	for _, presence := range []uint8{InfoHasOrigin, InfoHasRightOrigin, InfoHasParentSub} {
		if presence&InfoContentMask != 0 {
			t.Errorf("presence flag 0b%08b overlaps content nibble", presence)
		}
		if presence&0b0001_0000 != 0 {
			t.Errorf("presence flag 0b%08b uses reserved bit 4", presence)
		}
	}
}

func TestItem_EqualByID(t *testing.T) {
	a := &Item{ID: ID{1, 5}, Len: 3, Content: Content{Kind: KindString, Str: "abc"}}
	b := &Item{ID: ID{1, 5}, Len: 99, Content: Content{Kind: KindAny}} // same ID, different everything else
	c := &Item{ID: ID{1, 6}, Len: 3, Content: Content{Kind: KindString, Str: "abc"}}

	if !a.EqualByID(b) {
		t.Error("same ID, different content: should be EqualByID")
	}
	if a.EqualByID(c) {
		t.Error("different clock: should not be EqualByID")
	}
	if !((*Item)(nil)).EqualByID(nil) {
		t.Error("nil.EqualByID(nil) should be true")
	}
	if a.EqualByID(nil) || ((*Item)(nil)).EqualByID(a) {
		t.Error("nil EqualByID non-nil should be false")
	}
}

func TestItem_Splice_String(t *testing.T) {
	parent := Parent{Kind: ParentNamed, Named: "doc"}
	subKey := "key"
	originID := ID{Client: 7, Clock: 99}
	rightOriginID := ID{Client: 8, Clock: 0}
	it := &Item{
		ID:          ID{Client: 1, Clock: 100},
		Len:         5,
		Origin:      &originID,
		RightOrigin: &rightOriginID,
		Content:     Content{Kind: KindString, Str: "hello"},
		Parent:      parent,
		ParentSub:   &subKey,
		Flags:       FlagCountable,
	}

	right := it.Splice(2)
	if right == nil {
		t.Fatal("Splice returned nil")
	}

	// Left half mutated in place.
	if it.Len != 2 {
		t.Errorf("left.Len = %d, want 2", it.Len)
	}
	if it.Content.Str != "he" {
		t.Errorf("left.Content.Str = %q, want %q", it.Content.Str, "he")
	}
	if it.Right != right {
		t.Errorf("left.Right not set to new right item")
	}
	if it.Origin != &originID {
		t.Errorf("left.Origin must remain unchanged")
	}
	if it.RightOrigin != &rightOriginID {
		t.Errorf("left.RightOrigin must remain unchanged (immutable per YATA)")
	}

	// Right half: id, len, origin, content.
	wantRightID := ID{Client: 1, Clock: 102}
	if !right.ID.Equal(wantRightID) {
		t.Errorf("right.ID = %v, want %v", right.ID, wantRightID)
	}
	if right.Len != 3 {
		t.Errorf("right.Len = %d, want 3", right.Len)
	}
	if right.Content.Str != "llo" {
		t.Errorf("right.Content.Str = %q, want %q", right.Content.Str, "llo")
	}
	wantRightOriginID := ID{Client: 1, Clock: 101}
	if right.Origin == nil || !right.Origin.Equal(wantRightOriginID) {
		t.Errorf("right.Origin = %v, want %v (last clock of left after truncation)", right.Origin, wantRightOriginID)
	}
	if right.RightOrigin != it.RightOrigin {
		t.Errorf("right.RightOrigin must inherit from left's RightOrigin")
	}
	if right.Left != it {
		t.Errorf("right.Left must point at left half")
	}
	if right.Right != nil {
		t.Errorf("right.Right should inherit nil from left's original Right")
	}
	if right.Parent != parent {
		t.Errorf("right.Parent must inherit from left")
	}
	if right.ParentSub != it.ParentSub {
		t.Errorf("right.ParentSub must inherit from left")
	}
	if right.Flags != it.Flags {
		t.Errorf("right.Flags = %b, want %b", right.Flags, it.Flags)
	}
}

func TestItem_Splice_NeighbourRelinking(t *testing.T) {
	// Setup: A <- B(len=5) -> C
	a := &Item{ID: ID{Client: 1, Clock: 0}, Len: 1}
	b := &Item{
		ID:      ID{Client: 1, Clock: 1},
		Len:     5,
		Left:    a,
		Content: Content{Kind: KindString, Str: "abcde"},
	}
	c := &Item{ID: ID{Client: 1, Clock: 6}, Len: 1, Left: b}
	a.Right = b
	b.Right = c

	// Splice B at offset 2.
	newR := b.Splice(2)
	if newR == nil {
		t.Fatal("Splice returned nil")
	}

	// Expected: A <- B(2) <- newR(3) -> C
	if a.Right != b {
		t.Error("A.Right should still point at B (left half)")
	}
	if b.Left != a {
		t.Error("B.Left must remain A")
	}
	if b.Right != newR {
		t.Error("B.Right must point at newR")
	}
	if newR.Left != b {
		t.Error("newR.Left must point at B")
	}
	if newR.Right != c {
		t.Error("newR.Right must inherit B's original Right (C)")
	}
	if c.Left != newR {
		t.Error("C.Left must be re-pointed to newR")
	}
}

func TestItem_Splice_RejectedOffsets(t *testing.T) {
	mk := func() *Item {
		return &Item{
			ID:      ID{Client: 1, Clock: 0},
			Len:     5,
			Content: Content{Kind: KindString, Str: "hello"},
		}
	}
	cases := []struct {
		name   string
		offset uint64
	}{
		{"offset 0", 0},
		{"offset == Len", 5},
		{"offset > Len", 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := mk()
			before := it.Len
			beforeStr := it.Content.Str
			if got := it.Splice(tc.offset); got != nil {
				t.Errorf("Splice(%d) = %+v, want nil", tc.offset, got)
			}
			if it.Len != before || it.Content.Str != beforeStr {
				t.Errorf("Splice(%d) mutated rejected item", tc.offset)
			}
		})
	}
}

func TestItem_Splice_NonSplittableContent(t *testing.T) {
	it := &Item{
		ID:      ID{Client: 1, Clock: 0},
		Len:     1,
		Content: Content{Kind: KindBinary, Bytes: []byte{1, 2, 3}},
	}
	// Splice on Len=1 returns nil from the offset guard before content
	// is inspected. Try an artificial Len=3 with non-splittable content
	// to drive the Content.Split error path.
	it.Len = 3
	if got := it.Splice(1); got != nil {
		t.Errorf("Splice on KindBinary should return nil, got %+v", got)
	}
}

func TestParent_IsResolved(t *testing.T) {
	cases := []struct {
		name string
		p    Parent
		want bool
	}{
		{"unknown", Parent{Kind: ParentUnknown}, false},
		{"named", Parent{Kind: ParentNamed, Named: "root"}, false},
		{"id", Parent{Kind: ParentID, ID: ID{1, 5}}, false},
		{"branch nil", Parent{Kind: ParentBranch, Branch: nil}, false},
		{"branch resolved", Parent{Kind: ParentBranch, Branch: &Branch{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.IsResolved(); got != tc.want {
				t.Errorf("IsResolved=%v want %v", got, tc.want)
			}
		})
	}
}

// Splitting an item that was redone keeps both halves linked to the matching
// halves of the copy it was redone into, as yjs splitItem does. Undo maps a
// position through this link; without it the right half of a split tombstone
// looks like it was never redone.
func TestItem_Splice_KeepsRedoneOffset(t *testing.T) {
	redone := ID{Client: 1, Clock: 500}
	it := &Item{
		ID:      ID{Client: 1, Clock: 100},
		Len:     5,
		Content: Content{Kind: KindString, Str: "hello"},
		Redone:  &redone,
	}
	right := it.Splice(2)
	if right == nil {
		t.Fatal("Splice returned nil")
	}
	if it.Redone == nil || *it.Redone != (ID{Client: 1, Clock: 500}) {
		t.Errorf("left.Redone = %v, want 1:500", it.Redone)
	}
	if right.Redone == nil || *right.Redone != (ID{Client: 1, Clock: 502}) {
		t.Errorf("right.Redone = %v, want 1:502", right.Redone)
	}
	if right.Redone == it.Redone {
		t.Error("right.Redone aliases left.Redone; the halves must not share one pointer")
	}
}
