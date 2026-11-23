package encoding

import (
	"errors"

	"github.com/Deln0r/ygo/internal/lib0"
)

// ConvertUpdateBytes is yjs convertUpdateFormat with the identity
// transformer: every struct of update in wire order, Skips and empty structs
// included, written in the other format (a section wherever the client
// changes), then the delete set as read (clients descending). Bytes after the
// delete set are ignored. A V2 output holding more structs than its length
// pays for is checked to decode within floor.
func ConvertUpdateBytes(update []byte, fromV2, toV2 bool, floor int) ([]byte, error) {
	raw, err := decodeRaw(update, fromV2, floor)
	if err != nil {
		return nil, err
	}
	for _, b := range raw.stream {
		if b.Kind == WireBlockItem && b.Item == nil {
			return nil, errors.New("encoding: conversion of an update holding content ygo cannot write")
		}
	}
	return writeRawUpdate(raw.stream, raw.ds, toV2, floor, "the converted update")
}

// decodeRawStructs reads an update's structs in wire order and stops before
// its delete set, which yjs's state vector and meta of an update never read.
func decodeRawStructs(buf []byte, v2 bool, floor int) ([]Block, error) {
	raw := &rawUpdate{structsOnly: true}
	if v2 {
		if _, err := decodeUpdateV2Into(buf, len(buf), max(len(buf), floor), raw); err != nil {
			return nil, err
		}
		return raw.stream, nil
	}
	if _, _, err := decodeUpdateInto(buf, strictStrings, raw); err != nil {
		return nil, err
	}
	return raw.stream, nil
}

// StateVectorFromUpdateBytes is yjs encodeStateVectorFromUpdate (v2 false)
// or encodeStateVectorFromUpdateV2, which write the same bytes: for each
// run of one client in wire order, the clock it reaches counting from 0
// until a Skip, written as (client, clock) when the client changes and the
// clock is not 0, clients in the order met (a client in two runs twice),
// the count first. As in yjs, the first struct's clock is taken before it is
// checked for a Skip, and no gap between sections of one client is checked.
func StateVectorFromUpdateBytes(update []byte, v2 bool, floor int) ([]byte, error) {
	st, err := decodeRawStructs(update, v2, floor)
	if err != nil {
		return nil, err
	}
	if len(st) == 0 {
		return []byte{0}, nil
	}
	var pairs []uint64
	first := st[0]
	currClient := blockClient(&first)
	stop := blockStartClock(first) != 0
	var currClock uint64
	if !stop {
		currClock = blockStartClock(first) + blockLen(first)
	}
	for i := range st {
		b := &st[i]
		if c := blockClient(b); c != currClient {
			if currClock != 0 {
				pairs = append(pairs, currClient, currClock)
			}
			currClient = c
			currClock = 0
			stop = blockStartClock(*b) != 0
		}
		if b.Kind == WireBlockSkip {
			stop = true
		}
		if !stop {
			currClock = blockStartClock(*b) + blockLen(*b)
		}
	}
	if currClock != 0 {
		pairs = append(pairs, currClient, currClock)
	}
	out := lib0.WriteVarUint(nil, uint64(len(pairs)/2))
	for _, v := range pairs {
		out = lib0.WriteVarUint(out, v)
	}
	return out, nil
}

// UpdateMeta is yjs parseUpdateMeta's result: per client, the clock its
// structs start at and the clock they end at.
type UpdateMeta struct {
	From, To map[uint64]uint64
}

// ParseUpdateMetaBytes is yjs parseUpdateMeta (v2 false) or
// parseUpdateMetaV2: for each run of one client in wire order, From is the
// clock it starts at (a later run of the client overwrites) and To the clock
// its last struct ends at, Skips included.
func ParseUpdateMetaBytes(update []byte, v2 bool, floor int) (UpdateMeta, error) {
	meta := UpdateMeta{From: map[uint64]uint64{}, To: map[uint64]uint64{}}
	st, err := decodeRawStructs(update, v2, floor)
	if err != nil {
		return UpdateMeta{}, err
	}
	if len(st) == 0 {
		return meta, nil
	}
	currClient := blockClient(&st[0])
	currClock := blockStartClock(st[0])
	meta.From[currClient] = currClock
	for i := range st {
		b := &st[i]
		if c := blockClient(b); c != currClient {
			meta.To[currClient] = currClock
			meta.From[c] = blockStartClock(*b)
			currClient = c
		}
		currClock = blockStartClock(*b) + blockLen(*b)
	}
	meta.To[currClient] = currClock
	return meta, nil
}
