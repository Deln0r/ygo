package encoding

import "github.com/Deln0r/ygo/internal/lib0"

// stringMode says how a decoder reads wire strings: strictly, as lib0 does,
// for bytes that arrive from outside, or as stored, for bytes ygo itself
// stored. Releases before v1.26 accepted and stored strings with invalid
// UTF-8; reading those as stored keeps such documents loading as they did,
// with nothing in them changed.
type stringMode bool

const (
	strictStrings stringMode = false
	storedStrings stringMode = true
)

func (m stringMode) readString(buf []byte) (string, int, error) {
	if m == storedStrings {
		return lib0.ReadVarStringAsStored(buf)
	}
	return lib0.ReadVarString(buf)
}
