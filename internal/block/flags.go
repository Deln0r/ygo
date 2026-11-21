package block

// Wire info byte. Emitted as the first byte of every Item record in the
// V1 update format. Bits 0-3 carry the Content kind (ref number 0..11),
// bits 5-7 carry presence flags. Bit 4 is currently unused on the wire.
//
// Verbatim from yrs/src/block.rs:64-72 (commit 639db20).
const (
	InfoHasRightOrigin uint8 = 0b0100_0000 // bit 6
	InfoHasOrigin      uint8 = 0b1000_0000 // bit 7
	InfoHasParentSub   uint8 = 0b0010_0000 // bit 5
	InfoContentMask    uint8 = 0b0000_1111
)

// Internal Item flags. NOT serialized; live in Item.Flags as a uint16.
//
// Verbatim from yrs/src/block.rs:1044-1057.
//
// FlagMarked is yjs's Item.marker (bit 3 of Item.info): the item a
// TextMarkers marker points at (text_markers.go). yrs does not use it.
const (
	FlagKeep      uint16 = 0b0000_0000_0000_0001 // bit 0
	FlagCountable uint16 = 0b0000_0000_0000_0010 // bit 1
	FlagDeleted   uint16 = 0b0000_0000_0000_0100 // bit 2
	FlagMarked    uint16 = 0b0000_0000_0000_1000 // bit 3 — yjs Item.marker
	FlagLinked    uint16 = 0b0000_0001_0000_0000 // bit 8 — weak-link target
)
