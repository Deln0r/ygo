package block

import "sync"

// listenersMu guards every branch's listener lists. It is held only to
// read or change a list, never while a listener runs, so a listener may
// register or remove listeners, and registration from another goroutine
// does not race a delivery.
var listenersMu sync.Mutex

// listener is one registration; its pointer is its identity, since Go
// funcs cannot be compared.
type listener[F any] struct{ fn F }

// listenerArray is one generation of a list, as yjs EventHandler keeps
// one JavaScript array: adding appends to the current array, removing
// replaces it with a filtered copy. A delivery walks the array current
// when it started, with its length read afresh at every step, so a
// listener added during the delivery is called in it until the first
// removal, and a listener removed during it is still called, as yjs
// callAll over EventHandler.l does.
type listenerArray[F any] struct{ fns []*listener[F] }

// listeners is a branch's list of listeners of one kind.
type listeners[F any] struct{ cur *listenerArray[F] }

func (l *listeners[F]) add(fn F) *listener[F] {
	e := &listener[F]{fn: fn}
	if l.cur == nil {
		l.cur = &listenerArray[F]{}
	}
	l.cur.fns = append(l.cur.fns, e)
	return e
}

// remove replaces the array even when e is not in it, as yjs
// removeEventHandlerListener filters into a new array either way: a
// delivery in progress keeps walking the old one.
func (l *listeners[F]) remove(e *listener[F]) {
	next := &listenerArray[F]{}
	if l.cur != nil {
		next.fns = make([]*listener[F], 0, len(l.cur.fns))
		for _, x := range l.cur.fns {
			if x != e {
				next.fns = append(next.fns, x)
			}
		}
	}
	l.cur = next
}

func (l *listeners[F]) funcs() []F {
	if l.cur == nil {
		return nil
	}
	out := make([]F, len(l.cur.fns))
	for i, e := range l.cur.fns {
		out[i] = e.fn
	}
	return out
}

// call runs every listener of the array current now, by yjs's rules
// (listenerArray), with the lock released around each call.
func (l *listeners[F]) call(run func(F)) {
	listenersMu.Lock()
	a := l.cur
	listenersMu.Unlock()
	if a == nil {
		return
	}
	for i := 0; ; i++ {
		listenersMu.Lock()
		if i >= len(a.fns) {
			listenersMu.Unlock()
			return
		}
		fn := a.fns[i].fn
		listenersMu.Unlock()
		run(fn)
	}
}

// AddObserver registers fn as an observer of b's own changes (yjs
// type.observe) and returns its removal. Observers and removals made
// while a delivery runs follow yjs EventHandler (listenerArray). The
// exported Observers field mirrors the list for reading.
func (b *Branch) AddObserver(fn func(any)) (remove func()) {
	listenersMu.Lock()
	e := b.observers.add(fn)
	b.Observers = b.observers.funcs()
	listenersMu.Unlock()
	return func() {
		listenersMu.Lock()
		b.observers.remove(e)
		b.Observers = b.observers.funcs()
		listenersMu.Unlock()
	}
}

// AddDeepObserver is AddObserver for deep observers (yjs
// type.observeDeep); DeepObservers mirrors the list.
func (b *Branch) AddDeepObserver(fn func([]any)) (remove func()) {
	listenersMu.Lock()
	e := b.deepObservers.add(fn)
	b.DeepObservers = b.deepObservers.funcs()
	listenersMu.Unlock()
	return func() {
		listenersMu.Lock()
		b.deepObservers.remove(e)
		b.DeepObservers = b.deepObservers.funcs()
		listenersMu.Unlock()
	}
}

// CallObservers delivers ev to b's observers (yjs
// callEventHandlerListeners on type._eH).
func (b *Branch) CallObservers(ev any) {
	b.observers.call(func(fn func(any)) { fn(ev) })
}

// CallDeepObservers delivers events to b's deep observers (yjs
// callEventHandlerListeners on type._dEH).
func (b *Branch) CallDeepObservers(events []any) {
	b.deepObservers.call(func(fn func([]any)) { fn(events) })
}

// ObserverCount returns how many observers b has now.
func (b *Branch) ObserverCount() int {
	listenersMu.Lock()
	defer listenersMu.Unlock()
	if b.observers.cur == nil {
		return 0
	}
	return len(b.observers.cur.fns)
}

// DeepObserverCount returns how many deep observers b has now.
func (b *Branch) DeepObserverCount() int {
	listenersMu.Lock()
	defer listenersMu.Unlock()
	if b.deepObservers.cur == nil {
		return 0
	}
	return len(b.deepObservers.cur.fns)
}

// HasDeepObservers reports whether b has a deep observer now.
func (b *Branch) HasDeepObservers() bool {
	listenersMu.Lock()
	defer listenersMu.Unlock()
	return b.deepObservers.cur != nil && len(b.deepObservers.cur.fns) > 0
}
