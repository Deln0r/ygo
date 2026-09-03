package encoding

import (
	"sort"

	"github.com/Deln0r/ygo/internal/lib0"
)

// Range is a half-open clock interval [Start, Start+Length). The wire
// form encodes (Start, Length), not (Start, End). yrs's in-memory
// Range<u32> is half-open; the wire form is unambiguous.
//
// Per docs/yrs-port-notes/update-v1.md gotcha 2.
type Range struct {
	Start  uint64
	Length uint64
}

// End returns the (exclusive) upper bound of the range.
func (r Range) End() uint64 { return r.Start + r.Length }

// IdSet is a per-client set of half-open clock ranges. Used as the V1
// wire DeleteSet. Insert auto-merges overlapping or adjacent ranges so
// the per-client list stays sorted and non-overlapping at all times.
//
// Mirrors yrs IdSet (id_set.rs:121-135), simplified — yrs has a
// generic IdMapInner<V> with a () value type for the wire DeleteSet
// case; we just store []Range directly.
type IdSet struct {
	clients map[uint64][]Range
}

// NewIdSet returns an empty IdSet.
func NewIdSet() *IdSet {
	return &IdSet{clients: map[uint64][]Range{}}
}

// Insert adds the half-open range [start, start+length) for client.
// Overlapping or adjacent existing ranges are merged.
//
// Mirrors yrs IdRanges::insert (id_set.rs:34-90 area).
func (s *IdSet) Insert(client, start, length uint64) {
	if length == 0 {
		return
	}
	end := start + length
	existing := s.clients[client]

	// Find the first existing range that ends at or after start
	// (i.e. its End() >= start). That's the leftmost candidate for
	// a merge. Use sort.Search on the predicate so we can locate
	// the merge window in one binary search.
	i := sort.Search(len(existing), func(i int) bool {
		return existing[i].End() >= start
	})

	// Sweep right collecting every range that starts at or before
	// our (possibly extended) end. Each collected range extends our
	// merged span to max(end, existing.End()) and drops our merged
	// start to min(start, existing.Start).
	j := i
	for j < len(existing) && existing[j].Start <= end {
		if existing[j].End() > end {
			end = existing[j].End()
		}
		if existing[j].Start < start {
			start = existing[j].Start
		}
		j++
	}

	merged := Range{Start: start, Length: end - start}
	switch {
	case i == j:
		// No overlap; insert at i.
		existing = append(existing, Range{})
		copy(existing[i+1:], existing[i:])
		existing[i] = merged
	case j == i+1:
		// Single existing range absorbed; replace in place.
		existing[i] = merged
	default:
		// Multiple existing ranges absorbed; collapse [i, j) into one.
		existing[i] = merged
		copy(existing[i+1:], existing[j:])
		existing = existing[:len(existing)-(j-i-1)]
	}
	s.clients[client] = existing
}

// Contains reports whether the (client, clock) pair is covered by
// some range in the set.
func (s *IdSet) Contains(client, clock uint64) bool {
	ranges, ok := s.clients[client]
	if !ok {
		return false
	}
	i := sort.Search(len(ranges), func(i int) bool {
		return ranges[i].End() > clock
	})
	return i < len(ranges) && ranges[i].Start <= clock
}

// Iterate calls fn once per client, in ascending client ID order.
// The ranges slice handed to fn must NOT be retained or mutated; it
// aliases internal state.
func (s *IdSet) Iterate(fn func(client uint64, ranges []Range)) {
	clients := make([]uint64, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	for _, c := range clients {
		fn(c, s.clients[c])
	}
}

// ClientCount returns the number of distinct clients with at least
// one range in the set.
func (s *IdSet) ClientCount() int { return len(s.clients) }

// Encode appends the V1 wire encoding of s to buf and returns the
// extended slice. Wire layout:
//
//	varuint clientCount
//	clientCount × (
//	    varuint clientID
//	    varuint rangeCount
//	    rangeCount × (varuint start, varuint length)
//	)
//
// Clients are emitted in DESCENDING order, matching the V1 delete-set
// wire form yjs produces (writeDeleteSet sorts "(a, b) => b[0] -
// a[0]"). Ranges within a client are already sorted by Insert. Note
// this differs from Iterate, which is ascending for general-purpose
// deterministic traversal; the wire form specifically tracks yjs, our
// byte-compatibility reference, not yrs (whose BTreeMap emits
// ascending). Single-client sets encode identically either way, which
// is why the difference stayed latent until multi-client snapshots.
func (s *IdSet) Encode(buf []byte) []byte {
	clients := make([]uint64, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	buf = lib0.WriteVarUint(buf, uint64(len(clients)))
	for _, c := range clients {
		ranges := s.clients[c]
		buf = lib0.WriteVarUint(buf, c)
		buf = lib0.WriteVarUint(buf, uint64(len(ranges)))
		for _, r := range ranges {
			buf = lib0.WriteVarUint(buf, r.Start)
			buf = lib0.WriteVarUint(buf, r.Length)
		}
	}
	return buf
}

// DecodeIdSet parses a V1 wire-encoded IdSet from buf and returns
// the IdSet plus the unconsumed tail.
func DecodeIdSet(buf []byte) (*IdSet, []byte, error) {
	clientCount, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return nil, buf, err
	}
	buf = buf[n:]
	if err := checkDecodeCount(clientCount, len(buf)); err != nil {
		return nil, buf, err
	}

	s := NewIdSet()
	for i := uint64(0); i < clientCount; i++ {
		clientID, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]

		rangeCount, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]

		if err := checkDecodeCount(rangeCount, len(buf)); err != nil {
			return nil, buf, err
		}
		ranges := make([]Range, rangeCount)
		for j := uint64(0); j < rangeCount; j++ {
			start, n, err := lib0.ReadVarUint(buf)
			if err != nil {
				return nil, buf, err
			}
			buf = buf[n:]
			length, n, err := lib0.ReadVarUint(buf)
			if err != nil {
				return nil, buf, err
			}
			buf = buf[n:]
			if err := checkClockRange(start, length); err != nil {
				return nil, buf, err
			}
			ranges[j] = Range{Start: start, Length: length}
		}
		s.clients[clientID] = ranges
	}
	return s, buf, nil
}
