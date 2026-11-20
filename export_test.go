package ygo

// MergeUpdatesV2Floor is MergeUpdatesV2 with a struct floor of floor in
// place of 1,048,576, so that the bounds past the floor are tested on
// updates of thousands of structs, not millions.
func MergeUpdatesV2Floor(updates [][]byte, floor int) ([]byte, error) {
	return mergeUpdatesV2(updates, floor)
}
