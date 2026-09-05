// Package ygo is a pure-Go port of the Yjs CRDT framework.
//
// ygo is binary-protocol compatible with the npm yjs package (V1 update encoding),
// allowing JavaScript clients to synchronize seamlessly with Go servers and vice versa.
//
// Pure-Go means no CGO, so gomobile bind works for iOS/Android targets.
//
// The public API is stable for the v1.x line: new functionality lands as a
// minor release and breaking changes are deferred to a future major, per
// semantic versioning. The wire formats are validated bidirectionally against
// yjs on every push.
//
// See https://github.com/Deln0r/ygo for documentation and examples.
package ygo

// Version is the ygo release this source tree corresponds to. It is set as
// part of the release ceremony, so a checkout of main between releases carries
// the last released value.
const Version = "1.19.0"
