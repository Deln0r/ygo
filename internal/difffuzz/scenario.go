// Package difffuzz is a differential fuzzer of ygo against yjs: a seeded
// scenario of several peers editing, undoing and exchanging updates is
// replayed through ygo's public API (Run) and through yjs by a Node worker
// (testdata/gen/difffuzz-oracle.mjs), and the two traces are compared step by
// step (Compare): every update event byte for byte, the bytes a sync, merge
// or diff sends, and each touched peer's state, state vector, pending queue,
// view and undo stacks.
//
// The scenario is data, so both sides replay the same one: positions and
// lengths are hints reduced against the target's current length at run
// time, nested targets are paths resolved at run time, and an op whose path
// no longer resolves does nothing on either side.
package difffuzz

import "encoding/json"

// Schema is the trace format version both sides write.
const Schema = 1

// Scenario is one fuzz run.
type Scenario struct {
	Seed    uint64 `json:"seed"`
	Peers   []Peer `json:"peers"`
	Observe bool   `json:"observe,omitempty"`
	Steps   []Step `json:"steps"`
}

// Peer is one document: its client ID, whether it collects garbage, and
// whether an UndoManager watches its roots.
type Peer struct {
	Client uint64 `json:"client"`
	GC     bool   `json:"gc"`
	Undo   bool   `json:"undo"`
}

// Step kinds.
const (
	StepTxn     = "txn"     // Ops on peer P in one transaction
	StepUndo    = "undo"    // peer P's UndoManager
	StepRedo    = "redo"    //
	StepStop    = "stop"    // stopCapturing
	StepUClear  = "uclear"  // UndoManager clear
	StepDeliver = "deliver" // From's update events not yet delivered to To, by Order
	StepSync    = "sync"    // To applies From's state diffed against To's state vector
	StepMerge   = "merge"   // To applies the merge of From's undelivered events
	StepDiff    = "diff"    // To applies that merge diffed against To's state vector
	StepFinal   = "final"   // every peer syncs with every other until nothing changes
	StepUnwind  = "unwind"  // after final: each undo peer undoes until nothing changes, then redoes
)

// Delivery orders.
const (
	OrderIn   = "in"
	OrderRev  = "rev"
	OrderShuf = "shuf" // mulberry32(R) Fisher-Yates
	OrderDup  = "dup"  // each event twice, in order
	OrderPart = "part" // shuffled as shuf, then the first 1 + N % len
	OrderLate = "late" // all but the earliest, in order (it comes later)
)

// Step is one action; only the fields of its kind are set.
type Step struct {
	K     string `json:"k"`
	P     int    `json:"p,omitempty"`
	From  int    `json:"from,omitempty"`
	To    int    `json:"to,omitempty"`
	V2    bool   `json:"v2,omitempty"`
	Order string `json:"order,omitempty"`
	N     int    `json:"n,omitempty"`
	R     uint32 `json:"r,omitempty"`
	// Conv delivers each event converted from the other format
	// (ConvertUpdateFormatV1ToV2 for a V2 delivery, V2ToV1 for V1).
	Conv bool `json:"conv,omitempty"`
	Ops  []Op `json:"ops,omitempty"`
}

// Target kinds an op wants at the end of its path.
const (
	WantText    = "text"
	WantArray   = "array"
	WantMap     = "map"
	WantXml     = "xml"   // an XML fragment or element (children)
	WantXmlEl   = "xmlel" // an XML element (attributes)
	WantXmlText = "xmltext"
)

// Op codes.
const (
	OpIns     = "ins"     // text: insert S (with Attrs if given); array: insert V's elements
	OpDel     = "del"     // text, array, xml: delete from Pos, Len
	OpFmt     = "fmt"     // text: format Pos, Len with Attrs
	OpEmb     = "emb"     // text: insert the embed V (with Attrs if given)
	OpDelta   = "delta"   // text: apply Delta
	OpPush    = "push"    // array: push V's elements
	OpInsMap  = "insmap"  // array: insert a nested map at Pos
	OpInsArr  = "insarr"  //
	OpInsText = "instext" // array: nested text; xml: an XmlText child
	OpSet     = "set"     // map: set Key to V
	OpMapDel  = "mdel"    // map: delete Key
	OpSetMap  = "setmap"  // map: nested map at Key
	OpSetArr  = "setarr"  //
	OpSetText = "settext" //
	OpClear   = "clear"   // map: clear
	OpInsEl   = "insel"   // xml: insert element S at Pos
	OpSetAttr = "setattr" // xml element: attribute Key = S
	OpRmAttr  = "rmattr"  // xml element: remove attribute Key
)

// Op is one change inside a transaction. Path segments are keys ("k0") for a
// map and index hints ("#3") for an array or an XML node; Attrs nil means
// "not given" (an insert then inherits the formatting around it), an empty
// list means "given, empty".
type Op struct {
	Root  string          `json:"root"`
	Path  []string        `json:"path,omitempty"`
	Want  string          `json:"want"`
	O     string          `json:"o"`
	Pos   int             `json:"pos,omitempty"`
	Len   int             `json:"len,omitempty"`
	S     string          `json:"s,omitempty"`
	Key   string          `json:"key,omitempty"`
	V     json.RawMessage `json:"v,omitempty"`
	Attrs *[]KV           `json:"attrs,omitempty"`
	Delta []DeltaStep     `json:"delta,omitempty"`
}

// KV is an attribute: a key and a tagged value.
type KV struct {
	K string          `json:"k"`
	V json.RawMessage `json:"v"`
}

// DeltaStep is a delta op by hints: R retains 1 + R % (what is left), D
// deletes 1 + D % (what is left), I inserts a string, E an embed; A are its
// attributes (nil: none given). A retain or delete with nothing left is
// dropped on both sides.
type DeltaStep struct {
	R *int            `json:"r,omitempty"`
	D *int            `json:"d,omitempty"`
	I *string         `json:"i,omitempty"`
	E json.RawMessage `json:"e,omitempty"`
	A *[]KV           `json:"a,omitempty"`
}
