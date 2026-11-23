package encoding

import (
	"errors"

	"github.com/Deln0r/ygo/internal/block"
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
		if fromV2 && !toV2 && b.Kind == WireBlockItem {
			negativeClocksAsLib0(b.Item)
		}
	}
	return writeRawUpdate(raw.stream, raw.ds, toV2, floor, "the converted update")
}

// negativeClocksAsLib0 gives the clocks of it's origins and parent ID, as V2
// read them, the value lib0's writeVarUint writes for them in V1. V2 writes
// those clocks as signed differences, so a malformed update can make one
// negative (the decoder keeps it as its two's complement); writeVarUint
// writes a negative number as the single byte num & 127, which a V1 reader
// reads as that value.
func negativeClocksAsLib0(it *block.Item) {
	fix := func(id *block.ID) *block.ID {
		if id == nil || id.Clock < 1<<63 {
			return id
		}
		c := *id
		c.Clock &= 0x7f
		return &c
	}
	it.Origin = fix(it.Origin)
	it.RightOrigin = fix(it.RightOrigin)
	if it.Parent.Kind == block.ParentID {
		it.Parent.ID = *fix(&it.Parent.ID)
	}
}

// visitStructs hands visit every struct of an update in wire order, keeping
// none, and stops before the delete set, which yjs's state vector and meta
// of an update never read.
func visitStructs(buf []byte, v2 bool, floor int, visit func(*Block)) error {
	raw := &rawUpdate{structsOnly: true, visit: visit}
	if v2 {
		_, err := decodeUpdateV2Into(buf, len(buf), max(len(buf), floor), raw)
		return err
	}
	_, _, err := decodeUpdateInto(buf, strictStrings, raw)
	return err
}

// StateVectorFromUpdateBytes is yjs encodeStateVectorFromUpdate (v2 false)
// or encodeStateVectorFromUpdateV2, which write the same bytes: for each
// run of one client in wire order, the clock it reaches counting from 0
// until a Skip, written as (client, clock) when the client changes and the
// clock is not 0, clients in the order met (a client in two runs twice),
// the count first. As in yjs, the first struct's clock is taken before it is
// checked for a Skip, and no gap between sections of one client is checked.
func StateVectorFromUpdateBytes(update []byte, v2 bool, floor int) ([]byte, error) {
	var (
		body                  []byte
		pairs                 uint64
		started, stop         bool
		currClient, currClock uint64
	)
	flush := func() {
		if currClock != 0 {
			body = lib0.WriteVarUint(body, currClient)
			body = lib0.WriteVarUint(body, currClock)
			pairs++
		}
	}
	err := visitStructs(update, v2, floor, func(b *Block) {
		c, start := blockClient(b), blockStartClock(*b)
		if !started {
			started = true
			currClient = c
			stop = start != 0
			if !stop {
				currClock = start + blockLen(*b)
			}
		}
		if c != currClient {
			flush()
			currClient = c
			currClock = 0
			stop = start != 0
		}
		if b.Kind == WireBlockSkip {
			stop = true
		}
		if !stop {
			currClock = start + blockLen(*b)
		}
	})
	if err != nil {
		return nil, err
	}
	if started {
		flush()
	}
	return append(lib0.WriteVarUint(nil, pairs), body...), nil
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
	var (
		started               bool
		currClient, currClock uint64
	)
	err := visitStructs(update, v2, floor, func(b *Block) {
		c, start := blockClient(b), blockStartClock(*b)
		if !started {
			started = true
			currClient = c
			meta.From[c] = start
		}
		if c != currClient {
			meta.To[currClient] = currClock
			meta.From[c] = start
			currClient = c
		}
		currClock = start + blockLen(*b)
	})
	if err != nil {
		return UpdateMeta{}, err
	}
	if started {
		meta.To[currClient] = currClock
	}
	return meta, nil
}
