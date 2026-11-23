package encoding

import "github.com/Deln0r/ygo/internal/lib0"

// stringMode says how a decoder reads wire strings: strictly, as lib0 does,
// for bytes that arrive from outside, or as stored, for bytes ygo itself
// stored. Releases before v1.26 accepted and stored strings with invalid
// UTF-8; reading those as stored keeps such documents loading as they did,
// with nothing in them changed. ownStrings reads strings strictly from bytes
// ygo encoded from its own document.
//
// It also says how shared type refs are read: from outside, a ref yjs does
// not read is refused (checkTypeRef); from bytes ygo stored or encoded, any
// ref is read as releases before 1.53.0 read it, so a document holding one
// still loads, compacts and restores.
type stringMode uint8

const (
	strictStrings stringMode = iota
	storedStrings
	ownStrings
)

// oldTypeRefs reports whether shared type refs are read as before 1.53.0.
func (m stringMode) oldTypeRefs() bool { return m != strictStrings }

func (m stringMode) readString(buf []byte) (string, int, error) {
	if m == storedStrings {
		return lib0.ReadVarStringAsStored(buf)
	}
	return lib0.ReadVarString(buf)
}
