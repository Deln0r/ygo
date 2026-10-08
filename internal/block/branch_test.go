package block

import (
	"reflect"
	"testing"
)

// MapKeys returns the keys in the order they were first set, then any
// key of Map that MapOrder misses, sorted; the slice is the caller's.
func TestBranch_MapKeys(t *testing.T) {
	it := &Item{}
	b := &Branch{Map: map[string]*Item{"z": it, "a": it, "m": it}, MapOrder: []string{"z", "a", "m"}}
	got := b.MapKeys()
	if want := []string{"z", "a", "m"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MapKeys = %v, want %v", got, want)
	}
	got[0] = "changed"
	if b.MapOrder[0] != "z" {
		t.Fatal("MapKeys returned MapOrder itself")
	}

	b = &Branch{Map: map[string]*Item{"c": it, "b": it, "a": it}, MapOrder: []string{"c"}}
	if got, want := b.MapKeys(), []string{"c", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MapKeys with missing order = %v, want %v", got, want)
	}
}

// Integrate records a key in MapOrder the first time it is set, not
// when a later value overwrites it.
func TestIntegrate_RecordsMapOrder(t *testing.T) {
	b := &Branch{Map: map[string]*Item{}}
	ctx := newTestCtx()
	key := func(k string) *string { return &k }
	z1 := &Item{ID: ID{Client: 1, Clock: 0}, Len: 1, ParentSub: key("z"), Parent: Parent{Kind: ParentBranch, Branch: b}}
	a1 := &Item{ID: ID{Client: 1, Clock: 1}, Len: 1, ParentSub: key("a"), Parent: Parent{Kind: ParentBranch, Branch: b}}
	z2 := &Item{ID: ID{Client: 1, Clock: 2}, Len: 1, ParentSub: key("z"), Parent: Parent{Kind: ParentBranch, Branch: b}, Left: z1, Origin: &ID{Client: 1, Clock: 0}}
	for _, it := range []*Item{z1, a1, z2} {
		ctx.register(it)
		it.Integrate(ctx, 0)
	}
	if !z1.IsDeleted() {
		t.Fatal("z1 is not deleted after z2 overwrote it")
	}
	if want := []string{"z", "a"}; !reflect.DeepEqual(b.MapOrder, want) {
		t.Fatalf("MapOrder = %v, want %v", b.MapOrder, want)
	}
	if b.Map["z"] != z2 {
		t.Fatal("z2 is not the winner for z")
	}
}
