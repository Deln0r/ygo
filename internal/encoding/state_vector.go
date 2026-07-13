package encoding

import (
	"sort"

	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
)

// EncodeStateVector appends the V1 wire encoding of sv to buf and
// returns the extended slice. Wire layout:
//
//	varuint clientCount
//	clientCount × (varuint clientID, varuint clock)
//
// Clients are sorted ascending by clientID for deterministic output.
// JS Yjs / yrs accept any order on decode; the canonical sort keeps
// our round-trip byte-equality tests stable.
//
// Mirrors yrs StateVector::encode (state_vector.rs ~line 80).
func EncodeStateVector(sv store.StateVector, buf []byte) []byte {
	clients := make([]uint64, 0, len(sv))
	for c := range sv {
		clients = append(clients, c)
	}
	// DESCENDING client order, matching yjs writeStateVector
	// ("sort((a, b) => b[0] - a[0])"). Ascending was byte-incompatible
	// with yjs for multi-client state vectors; single-client SVs (most
	// existing fixtures) never exposed it. Surfaced by the multi-client
	// snapshot fixture, 2026-06-08.
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	buf = lib0.WriteVarUint(buf, uint64(len(clients)))
	for _, c := range clients {
		buf = lib0.WriteVarUint(buf, c)
		buf = lib0.WriteVarUint(buf, sv[c])
	}
	return buf
}

// EncodeStateVectorFromUpdate computes the state vector a V1 update
// advances to, structurally from the update bytes, without reconstructing
// a document.
//
// Per client it counts the CONTIGUOUS run of blocks starting at clock 0
// and stops at the first Skip block or gap; a client whose blocks do not
// start at clock 0 is omitted entirely. This mirrors yjs
// encodeStateVectorFromUpdate exactly: the result is a safe remote state
// vector for diffing, so a client whose earliest clocks are absent (a
// diff, or a merge that emitted Skips for gaps) is not over-reported,
// which would otherwise make a diff withhold blocks a peer still needs.
//
// Use it to index or diff stored updates server-side without loading the
// full document.
func EncodeStateVectorFromUpdate(update []byte) ([]byte, error) {
	u, _, err := DecodeUpdate(update)
	if err != nil {
		return nil, err
	}
	sv := store.StateVector{}
	for client, blocks := range u.Blocks {
		// Blocks are clock-ascending within a client. Count forward from
		// clock 0 only while the run stays contiguous and Skip-free; the
		// first Skip or gap (or a non-zero start) freezes the clock.
		var curr uint64
		for _, b := range blocks {
			if b.Kind == WireBlockSkip || b.startClock() != curr {
				break
			}
			curr = b.startClock() + b.length()
		}
		if curr != 0 {
			sv[client] = curr
		}
	}
	return EncodeStateVector(sv, nil), nil
}

// DecodeStateVector parses a V1 wire-encoded StateVector from buf and
// returns the StateVector plus the unconsumed tail.
//
// Mirrors yrs StateVector::decode (state_vector.rs ~line 100).
func DecodeStateVector(buf []byte) (store.StateVector, []byte, error) {
	count, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return nil, buf, err
	}
	buf = buf[n:]
	if err := checkDecodeCount(count, len(buf)); err != nil {
		return nil, buf, err
	}

	sv := make(store.StateVector, count)
	for i := uint64(0); i < count; i++ {
		client, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		clock, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		sv[client] = clock
	}
	return sv, buf, nil
}
