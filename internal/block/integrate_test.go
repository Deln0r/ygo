package block

import "testing"

// testCtx is a minimal IntegrateContext implementation for unit tests
// that does not need a real TransactionMut + BlockStore. The map-based
// item lookup is sufficient for tests where origin/right_origin
// resolution is pre-arranged by the test setup.
type testCtx struct {
	items   map[ID]*Item
	deleted []*Item
	changed []*Branch
}

func newTestCtx() *testCtx {
	return &testCtx{items: map[ID]*Item{}}
}

func (c *testCtx) register(it *Item) {
	c.items[it.ID] = it
}

func (c *testCtx) GetItem(id ID) *Item               { return c.items[id] }
func (c *testCtx) MaterializeCleanStart(id ID) *Item { return c.items[id] }
func (c *testCtx) MaterializeCleanEnd(id ID) *Item   { return c.items[id] }
func (c *testCtx) Delete(item *Item) {
	if item == nil || item.IsDeleted() {
		return
	}
	item.SetDeleted(true)
	c.deleted = append(c.deleted, item)
}
func (c *testCtx) AddChangedType(p *Branch, _ *string) {
	c.changed = append(c.changed, p)
}

func (c *testCtx) AddedSubdoc(*Item) {}

func (c *testCtx) GetOrCreateBranch(_ string) *Branch {
	// Tests don't exercise root-name resolution; return nil to force
	// callers to pre-resolve their ParentBranch.
	return nil
}

func mkItem(client, clock, length uint64, contentStr string, parent *Branch) *Item {
	it := &Item{
		ID:      ID{Client: client, Clock: clock},
		Len:     length,
		Content: Content{Kind: KindString, Str: contentStr},
		Parent:  Parent{Kind: ParentBranch, Branch: parent},
		Flags:   FlagCountable,
	}
	return it
}

// --- Integrate tests --------------------------------------------------

func TestIntegrate_FirstInsertIntoEmptyParent(t *testing.T) {
	parent := &Branch{}
	ctx := newTestCtx()
	a := mkItem(1, 0, 5, "hello", parent)
	ctx.register(a)

	if dropped := a.Integrate(ctx, 0); dropped {
		t.Fatal("first insert into empty parent should not drop")
	}
	if parent.Start != a {
		t.Errorf("parent.Start = %v, want %v", parent.Start, a)
	}
	if a.Left != nil || a.Right != nil {
		t.Errorf("a.Left/Right = %v/%v, want nil/nil", a.Left, a.Right)
	}
	if parent.BlockLen != 5 || parent.ContentLen != 5 {
		t.Errorf("parent.BlockLen/ContentLen = %d/%d, want 5/5", parent.BlockLen, parent.ContentLen)
	}
	if len(ctx.changed) != 1 || ctx.changed[0] != parent {
		t.Errorf("expected one AddChangedType for parent, got %v", ctx.changed)
	}
}

func TestIntegrate_AppendAfterExisting(t *testing.T) {
	parent := &Branch{}
	ctx := newTestCtx()

	a := mkItem(1, 0, 1, "a", parent)
	ctx.register(a)
	a.Integrate(ctx, 0)

	// b inserted after a: pre-resolve Origin and Left.
	originID := a.LastID()
	b := mkItem(1, 1, 1, "b", parent)
	b.Origin = &originID
	b.Left = a
	ctx.register(b)

	if dropped := b.Integrate(ctx, 0); dropped {
		t.Fatal("simple append should not drop")
	}
	if a.Right != b {
		t.Errorf("a.Right = %v, want b", a.Right)
	}
	if b.Left != a {
		t.Errorf("b.Left = %v, want a", b.Left)
	}
	if b.Right != nil {
		t.Errorf("b.Right = %v, want nil", b.Right)
	}
	if parent.Start != a {
		t.Errorf("parent.Start = %v, want a", parent.Start)
	}
	if parent.BlockLen != 2 {
		t.Errorf("parent.BlockLen = %d, want 2", parent.BlockLen)
	}
}

func TestIntegrate_ConcurrentInsertsTiebreakerByClient(t *testing.T) {
	// Setup: two replicas insert items at the same origin (no left).
	// YATA tiebreaker: lower client ID stays leftmost. Verify final
	// document order is deterministic regardless of insertion order
	// at the local replica.
	for _, scenario := range []struct {
		name          string
		first, second uint64 // client IDs in insertion order at this replica
	}{
		{"client1 first", 1, 2},
		{"client2 first", 2, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			parent := &Branch{}
			ctx := newTestCtx()

			// Both items have Origin=nil (anchored at parent start)
			// and RightOrigin=nil (anchored at parent end). They
			// race for the same position.
			itFirst := mkItem(scenario.first, 0, 1, "x", parent)
			itSecond := mkItem(scenario.second, 0, 1, "y", parent)
			ctx.register(itFirst)
			ctx.register(itSecond)

			itFirst.Integrate(ctx, 0)
			itSecond.Integrate(ctx, 0)

			// Walk the linked list from parent.Start. Lower client
			// must come first.
			head := parent.Start
			if head == nil {
				t.Fatal("parent.Start is nil after integration")
			}
			if head.Right == nil {
				t.Fatal("only one item in parent after second integrate")
			}
			if head.ID.Client > head.Right.ID.Client {
				t.Errorf("ordering violated: %d should not precede %d", head.ID.Client, head.Right.ID.Client)
			}
			tail := head.Right
			if tail.Right != nil {
				t.Errorf("expected exactly two items, got tail.Right=%v", tail.Right)
			}
			if tail.Left != head {
				t.Error("doubly-linked list invariant: tail.Left != head")
			}
		})
	}
}

func TestIntegrate_MapKeyTombstonesPredecessor(t *testing.T) {
	parent := &Branch{Map: map[string]*Item{}}
	ctx := newTestCtx()

	key := "color"

	// Existing winner under key.
	a := mkItem(1, 0, 1, "red", parent)
	a.ParentSub = &key
	ctx.register(a)
	a.Integrate(ctx, 0)
	if parent.Map[key] != a {
		t.Fatalf("setup: parent.Map[%q] = %v, want a", key, parent.Map[key])
	}

	// New write at the same key by client 2, with a as Origin/Left.
	originID := a.LastID()
	b := mkItem(2, 0, 1, "blue", parent)
	b.ParentSub = &key
	b.Origin = &originID
	b.Left = a
	ctx.register(b)

	if dropped := b.Integrate(ctx, 0); dropped {
		t.Fatal("new map-key writer should not be dropped")
	}
	if parent.Map[key] != b {
		t.Errorf("parent.Map[%q] = %v, want b", key, parent.Map[key])
	}
	if !a.IsDeleted() {
		t.Error("predecessor a was not tombstoned")
	}
	found := false
	for _, d := range ctx.deleted {
		if d == a {
			found = true
			break
		}
	}
	if !found {
		t.Error("ctx.Delete was not called on predecessor")
	}
}

func TestIntegrate_ParentDeletedAutoDeletes(t *testing.T) {
	owningItem := &Item{ID: ID{Client: 1, Clock: 0}, Len: 1, Flags: FlagDeleted}
	parent := &Branch{Item: owningItem}
	ctx := newTestCtx()

	a := mkItem(2, 0, 1, "x", parent)
	ctx.register(a)
	dropped := a.Integrate(ctx, 0)
	if !dropped {
		t.Fatal("integrate into deleted-parent must return true (drop)")
	}
}

func TestIntegrate_UnresolvedParentReturnsTrue(t *testing.T) {
	ctx := newTestCtx()
	a := mkItem(1, 0, 1, "x", nil)
	a.Parent = Parent{Kind: ParentNamed, Named: "root"}
	ctx.register(a)
	if !a.Integrate(ctx, 0) {
		t.Error("ParentNamed (unresolved) should return true (drop) until types layer ports root-type resolution")
	}
}

func TestIntegrate_OffsetGreaterThanZeroNotImplemented(t *testing.T) {
	parent := &Branch{}
	ctx := newTestCtx()
	a := mkItem(1, 0, 5, "hello", parent)
	ctx.register(a)
	// Should not panic; returns false (caller responsible for the
	// offset>0 path until we port Update.integrate).
	if dropped := a.Integrate(ctx, 2); dropped {
		t.Error("offset > 0 path is stubbed to return false")
	}
}

// --- TrySquash tests --------------------------------------------------

func TestTrySquash_AdjacentSameClientStrings(t *testing.T) {
	a := &Item{
		ID:      ID{Client: 1, Clock: 0},
		Len:     2,
		Content: Content{Kind: KindString, Str: "ab"},
		Flags:   FlagCountable,
	}
	bOrigin := a.LastID()
	b := &Item{
		ID:      ID{Client: 1, Clock: 2},
		Len:     3,
		Origin:  &bOrigin,
		Content: Content{Kind: KindString, Str: "cde"},
		Flags:   FlagCountable,
		Left:    a,
	}
	a.Right = b

	if !a.TrySquash(b) {
		t.Fatal("adjacent same-client strings should squash")
	}
	if a.Len != 5 {
		t.Errorf("a.Len = %d, want 5", a.Len)
	}
	if a.Content.Str != "abcde" {
		t.Errorf("a.Content.Str = %q, want abcde", a.Content.Str)
	}
	if a.Right != nil {
		t.Errorf("a.Right = %v, want nil (b was the rightmost)", a.Right)
	}
}

func TestTrySquash_RefuseDifferentClient(t *testing.T) {
	a := &Item{ID: ID{Client: 1, Clock: 0}, Len: 1, Content: Content{Kind: KindString, Str: "a"}}
	bOrigin := a.LastID()
	b := &Item{ID: ID{Client: 2, Clock: 1}, Len: 1, Origin: &bOrigin, Content: Content{Kind: KindString, Str: "b"}, Left: a}
	a.Right = b
	if a.TrySquash(b) {
		t.Error("different-client items must not squash")
	}
}

func TestTrySquash_RefuseNonAdjacentClocks(t *testing.T) {
	a := &Item{ID: ID{Client: 1, Clock: 0}, Len: 1, Content: Content{Kind: KindString, Str: "a"}}
	bOrigin := a.LastID()
	b := &Item{ID: ID{Client: 1, Clock: 5}, Len: 1, Origin: &bOrigin, Content: Content{Kind: KindString, Str: "b"}, Left: a}
	a.Right = b
	if a.TrySquash(b) {
		t.Error("non-adjacent clocks must not squash (clock 0+1 != 5)")
	}
}

func TestTrySquash_RefuseMismatchedDeleted(t *testing.T) {
	a := &Item{ID: ID{Client: 1, Clock: 0}, Len: 1, Content: Content{Kind: KindString, Str: "a"}}
	bOrigin := a.LastID()
	b := &Item{ID: ID{Client: 1, Clock: 1}, Len: 1, Origin: &bOrigin, Content: Content{Kind: KindString, Str: "b"}, Left: a}
	a.Right = b
	b.SetDeleted(true)
	if a.TrySquash(b) {
		t.Error("mismatched deleted state must not squash")
	}
}

func TestTrySquash_RefuseMissingOrigin(t *testing.T) {
	a := &Item{ID: ID{Client: 1, Clock: 0}, Len: 1, Content: Content{Kind: KindString, Str: "a"}}
	b := &Item{ID: ID{Client: 1, Clock: 1}, Len: 1, Content: Content{Kind: KindString, Str: "b"}, Left: a}
	a.Right = b
	if a.TrySquash(b) {
		t.Error("missing other.Origin must not squash")
	}
}

// --- Content.TrySquash tests ------------------------------------------

func TestContent_TrySquash_String(t *testing.T) {
	a := Content{Kind: KindString, Str: "ab"}
	b := Content{Kind: KindString, Str: "cd"}
	if !a.TrySquash(&b) {
		t.Fatal("strings must squash")
	}
	if a.Str != "abcd" {
		t.Errorf("Str = %q, want abcd", a.Str)
	}
}

func TestContent_TrySquash_Any(t *testing.T) {
	a := Content{Kind: KindAny, Anys: []Any{1, 2}}
	b := Content{Kind: KindAny, Anys: []Any{3, 4}}
	if !a.TrySquash(&b) {
		t.Fatal("any-slices must squash")
	}
	if len(a.Anys) != 4 {
		t.Errorf("len(Anys) = %d, want 4", len(a.Anys))
	}
}

func TestContent_TrySquash_Deleted(t *testing.T) {
	a := Content{Kind: KindDeleted, DeletedLen: 3}
	b := Content{Kind: KindDeleted, DeletedLen: 5}
	if !a.TrySquash(&b) {
		t.Fatal("deleted runs must squash")
	}
	if a.DeletedLen != 8 {
		t.Errorf("DeletedLen = %d, want 8", a.DeletedLen)
	}
}

func TestContent_TrySquash_RefuseDifferentKinds(t *testing.T) {
	a := Content{Kind: KindString, Str: "ab"}
	b := Content{Kind: KindAny, Anys: []Any{1}}
	if a.TrySquash(&b) {
		t.Error("different kinds must not squash")
	}
}

func TestContent_TrySquash_RefuseNonSquashable(t *testing.T) {
	for _, kind := range []ContentKind{KindBinary, KindEmbed, KindFormat, KindType, KindDoc, KindMove} {
		a := Content{Kind: kind}
		b := Content{Kind: kind}
		if a.TrySquash(&b) {
			t.Errorf("kind %d should not squash", kind)
		}
	}
}
