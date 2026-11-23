package encoding

// v8Sort sorts a in place as V8 14.6 (Node 26) sorts an array with a
// comparison function (third_party/v8/builtins/array-sort.tq): fewer than 8
// elements by binary insertion, more by its TimSort (natural runs, extended
// by binary insertion to the minimum run length, merged by MergeCollapse
// with galloping). It is a port comparison for comparison, so it gives V8's
// order also for a comparator that is not consistent: yjs mergeUpdatesV2's
// says each of a GC and an item at one clock goes first, and which one V8
// puts first depends on the comparisons it makes.
func v8Sort[T any](a []T, cmp func(x, y T) int) {
	n := len(a)
	if n < 2 {
		return
	}
	if n < 8 {
		v8BinaryInsertionSort(a, 0, 0, n, cmp)
		return
	}
	s := &v8TimSort[T]{a: a, cmp: cmp, minGallop: v8MinGallopWins}
	s.sort()
}

const v8MinGallopWins = 7

// v8BinaryInsertionSort is BinaryInsertionSort: [low, high) sorted by
// binary insertion, [low, start) already sorted.
func v8BinaryInsertionSort[T any](a []T, low, startArg, high int, cmp func(x, y T) int) {
	start := startArg
	if low == startArg {
		start++
	}
	for ; start < high; start++ {
		left, right := low, start
		pivot := a[start]
		for left < right {
			mid := left + ((right - left) >> 1)
			if cmp(pivot, a[mid]) < 0 {
				right = mid
			} else {
				left = mid + 1
			}
		}
		for p := start; p > left; p-- {
			a[p] = a[p-1]
		}
		a[left] = pivot
	}
}

type v8TimSort[T any] struct {
	a         []T
	cmp       func(x, y T) int
	minGallop int
	runs      [][2]int // base, length
	tmp       []T
}

func (s *v8TimSort[T]) temp(n int) []T {
	if len(s.tmp) < n {
		s.tmp = make([]T, max(n, 64))
	}
	return s.tmp
}

// sort is ArrayTimSortImpl.
func (s *v8TimSort[T]) sort() {
	remaining := len(s.a)
	low := 0
	minRun := v8ComputeMinRunLength(remaining)
	for remaining != 0 {
		run := s.countAndMakeRun(low, low+remaining)
		if run < minRun {
			forced := min(minRun, remaining)
			v8BinaryInsertionSort(s.a, low, low+run, low+forced, s.cmp)
			run = forced
		}
		s.runs = append(s.runs, [2]int{low, run})
		s.mergeCollapse()
		low += run
		remaining -= run
	}
	s.mergeForceCollapse()
}

func v8ComputeMinRunLength(n int) int {
	r := 0
	for n >= 64 {
		r |= n & 1
		n >>= 1
	}
	return n + r
}

// countAndMakeRun is CountAndMakeRun: the length of the run at low,
// reversed when it descends (strictly).
func (s *v8TimSort[T]) countAndMakeRun(lowArg, high int) int {
	a := s.a
	low := lowArg + 1
	if low == high {
		return 1
	}
	run := 2
	elementLow := a[low]
	order := s.cmp(elementLow, a[low-1])
	descending := order < 0
	prev := elementLow
	for idx := low + 1; idx < high; idx++ {
		cur := a[idx]
		order = s.cmp(cur, prev)
		if descending {
			if order >= 0 {
				break
			}
		} else if order < 0 {
			break
		}
		prev = cur
		run++
	}
	if descending {
		for l, h := lowArg, lowArg+run-1; l < h; l, h = l+1, h-1 {
			a[l], a[h] = a[h], a[l]
		}
	}
	return run
}

func (s *v8TimSort[T]) runLen(i int) int { return s.runs[i][1] }

// runInvariantEstablished: run(n-2) > run(n-1) + run(n).
func (s *v8TimSort[T]) runInvariantEstablished(n int) bool {
	if n < 2 {
		return true
	}
	return s.runLen(n-2) > s.runLen(n-1)+s.runLen(n)
}

func (s *v8TimSort[T]) mergeCollapse() {
	for len(s.runs) > 1 {
		n := len(s.runs) - 2
		if !s.runInvariantEstablished(n+1) || !s.runInvariantEstablished(n) {
			if s.runLen(n-1) < s.runLen(n+1) {
				n--
			}
			s.mergeAt(n)
		} else if s.runLen(n) <= s.runLen(n+1) {
			s.mergeAt(n)
		} else {
			break
		}
	}
}

func (s *v8TimSort[T]) mergeForceCollapse() {
	for len(s.runs) > 1 {
		n := len(s.runs) - 2
		if n > 0 && s.runLen(n-1) < s.runLen(n+1) {
			n--
		}
		s.mergeAt(n)
	}
}

// mergeAt is MergeAt: runs i and i+1 merged.
func (s *v8TimSort[T]) mergeAt(i int) {
	stack := len(s.runs)
	baseA, lengthA := s.runs[i][0], s.runs[i][1]
	baseB, lengthB := s.runs[i+1][0], s.runs[i+1][1]
	s.runs[i][1] = lengthA + lengthB
	if i == stack-3 {
		s.runs[i+1] = s.runs[i+2]
	}
	s.runs = s.runs[:stack-1]

	k := s.gallopRight(s.a, s.a[baseB], baseA, lengthA, 0)
	baseA += k
	lengthA -= k
	if lengthA == 0 {
		return
	}
	lengthB = s.gallopLeft(s.a, s.a[baseA+lengthA-1], baseB, lengthB, lengthB-1)
	if lengthB == 0 {
		return
	}
	if lengthA <= lengthB {
		s.mergeLow(baseA, lengthA, baseB, lengthB)
	} else {
		s.mergeHigh(baseA, lengthA, baseB, lengthB)
	}
}

// gallopLeft is GallopLeft: where key goes in array[base, base+length),
// left of equal elements.
func (s *v8TimSort[T]) gallopLeft(array []T, key T, base, length, hint int) int {
	lastOfs, offset := 0, 1
	if s.cmp(array[base+hint], key) < 0 {
		maxOfs := length - hint
		for offset < maxOfs {
			if s.cmp(array[base+hint+offset], key) >= 0 {
				break
			}
			lastOfs = offset
			offset = (offset << 1) + 1
			if offset <= 0 {
				offset = maxOfs
			}
		}
		if offset > maxOfs {
			offset = maxOfs
		}
		lastOfs += hint
		offset += hint
	} else {
		maxOfs := hint + 1
		for offset < maxOfs {
			if s.cmp(array[base+hint-offset], key) < 0 {
				break
			}
			lastOfs = offset
			offset = (offset << 1) + 1
			if offset <= 0 {
				offset = maxOfs
			}
		}
		if offset > maxOfs {
			offset = maxOfs
		}
		tmp := lastOfs
		lastOfs = hint - offset
		offset = hint - tmp
	}
	lastOfs++
	for lastOfs < offset {
		m := lastOfs + ((offset - lastOfs) >> 1)
		if s.cmp(array[base+m], key) < 0 {
			lastOfs = m + 1
		} else {
			offset = m
		}
	}
	return offset
}

// gallopRight is GallopRight: where key goes in array[base, base+length),
// right of equal elements.
func (s *v8TimSort[T]) gallopRight(array []T, key T, base, length, hint int) int {
	lastOfs, offset := 0, 1
	if s.cmp(key, array[base+hint]) < 0 {
		maxOfs := hint + 1
		for offset < maxOfs {
			if s.cmp(key, array[base+hint-offset]) >= 0 {
				break
			}
			lastOfs = offset
			offset = (offset << 1) + 1
			if offset <= 0 {
				offset = maxOfs
			}
		}
		if offset > maxOfs {
			offset = maxOfs
		}
		tmp := lastOfs
		lastOfs = hint - offset
		offset = hint - tmp
	} else {
		maxOfs := length - hint
		for offset < maxOfs {
			if s.cmp(key, array[base+hint+offset]) < 0 {
				break
			}
			lastOfs = offset
			offset = (offset << 1) + 1
			if offset <= 0 {
				offset = maxOfs
			}
		}
		if offset > maxOfs {
			offset = maxOfs
		}
		lastOfs += hint
		offset += hint
	}
	lastOfs++
	for lastOfs < offset {
		m := lastOfs + ((offset - lastOfs) >> 1)
		if s.cmp(key, array[base+m]) < 0 {
			offset = m
		} else {
			lastOfs = m + 1
		}
	}
	return offset
}

// mergeLow is MergeLow: run A (the shorter) copied out, merged from the left.
func (s *v8TimSort[T]) mergeLow(baseA, lengthA, baseB, lengthB int) {
	a := s.a
	tmp := s.temp(lengthA)
	copy(tmp, a[baseA:baseA+lengthA])
	dest, cursorTemp, cursorB := baseA, 0, baseB
	a[dest] = a[cursorB]
	dest++
	cursorB++
	lengthB--
	if lengthB == 0 {
		goto succeed
	}
	if lengthA == 1 {
		goto copyB
	}
	{
		minGallop := s.minGallop
		for {
			winsA, winsB := 0, 0
			for {
				if s.cmp(a[cursorB], tmp[cursorTemp]) < 0 {
					a[dest] = a[cursorB]
					dest++
					cursorB++
					winsB++
					lengthB--
					winsA = 0
					if lengthB == 0 {
						goto succeed
					}
					if winsB >= minGallop {
						break
					}
				} else {
					a[dest] = tmp[cursorTemp]
					dest++
					cursorTemp++
					winsA++
					lengthA--
					winsB = 0
					if lengthA == 1 {
						goto copyB
					}
					if winsA >= minGallop {
						break
					}
				}
			}
			minGallop++
			first := true
			for winsA >= v8MinGallopWins || winsB >= v8MinGallopWins || first {
				first = false
				minGallop = max(1, minGallop-1)
				s.minGallop = minGallop
				winsA = s.gallopRight(tmp, a[cursorB], cursorTemp, lengthA, 0)
				if winsA > 0 {
					copy(a[dest:dest+winsA], tmp[cursorTemp:cursorTemp+winsA])
					dest += winsA
					cursorTemp += winsA
					lengthA -= winsA
					if lengthA == 1 {
						goto copyB
					}
					if lengthA == 0 {
						goto succeed
					}
				}
				a[dest] = a[cursorB]
				dest++
				cursorB++
				lengthB--
				if lengthB == 0 {
					goto succeed
				}
				winsB = s.gallopLeft(a, tmp[cursorTemp], cursorB, lengthB, 0)
				if winsB > 0 {
					copy(a[dest:dest+winsB], a[cursorB:cursorB+winsB])
					dest += winsB
					cursorB += winsB
					lengthB -= winsB
					if lengthB == 0 {
						goto succeed
					}
				}
				a[dest] = tmp[cursorTemp]
				dest++
				cursorTemp++
				lengthA--
				if lengthA == 1 {
					goto copyB
				}
			}
			minGallop++
			s.minGallop = minGallop
		}
	}
succeed:
	if lengthA > 0 {
		copy(a[dest:dest+lengthA], tmp[cursorTemp:cursorTemp+lengthA])
	}
	return
copyB:
	copy(a[dest:dest+lengthB], a[cursorB:cursorB+lengthB])
	a[dest+lengthB] = tmp[cursorTemp]
}

// mergeHigh is MergeHigh: run B (the shorter) copied out, merged from the
// right.
func (s *v8TimSort[T]) mergeHigh(baseA, lengthA, baseB, lengthB int) {
	a := s.a
	tmp := s.temp(lengthB)
	copy(tmp, a[baseB:baseB+lengthB])
	dest, cursorTemp, cursorA := baseB+lengthB-1, lengthB-1, baseA+lengthA-1
	a[dest] = a[cursorA]
	dest--
	cursorA--
	lengthA--
	if lengthA == 0 {
		goto succeed
	}
	if lengthB == 1 {
		goto copyA
	}
	{
		minGallop := s.minGallop
		for {
			winsA, winsB := 0, 0
			for {
				if s.cmp(tmp[cursorTemp], a[cursorA]) < 0 {
					a[dest] = a[cursorA]
					dest--
					cursorA--
					winsA++
					lengthA--
					winsB = 0
					if lengthA == 0 {
						goto succeed
					}
					if winsA >= minGallop {
						break
					}
				} else {
					a[dest] = tmp[cursorTemp]
					dest--
					cursorTemp--
					winsB++
					lengthB--
					winsA = 0
					if lengthB == 1 {
						goto copyA
					}
					if winsB >= minGallop {
						break
					}
				}
			}
			minGallop++
			first := true
			for winsA >= v8MinGallopWins || winsB >= v8MinGallopWins || first {
				first = false
				minGallop = max(1, minGallop-1)
				s.minGallop = minGallop
				k := s.gallopRight(a, tmp[cursorTemp], baseA, lengthA, lengthA-1)
				winsA = lengthA - k
				if winsA > 0 {
					dest -= winsA
					cursorA -= winsA
					copy(a[dest+1:dest+1+winsA], a[cursorA+1:cursorA+1+winsA])
					lengthA -= winsA
					if lengthA == 0 {
						goto succeed
					}
				}
				a[dest] = tmp[cursorTemp]
				dest--
				cursorTemp--
				lengthB--
				if lengthB == 1 {
					goto copyA
				}
				k = s.gallopLeft(tmp, a[cursorA], 0, lengthB, lengthB-1)
				winsB = lengthB - k
				if winsB > 0 {
					dest -= winsB
					cursorTemp -= winsB
					copy(a[dest+1:dest+1+winsB], tmp[cursorTemp+1:cursorTemp+1+winsB])
					lengthB -= winsB
					if lengthB == 1 {
						goto copyA
					}
					if lengthB == 0 {
						goto succeed
					}
				}
				a[dest] = a[cursorA]
				dest--
				cursorA--
				lengthA--
				if lengthA == 0 {
					goto succeed
				}
			}
			minGallop++
			s.minGallop = minGallop
		}
	}
succeed:
	if lengthB > 0 {
		copy(a[dest-(lengthB-1):dest+1], tmp[0:lengthB])
	}
	return
copyA:
	dest -= lengthA
	cursorA -= lengthA
	copy(a[dest+1:dest+1+lengthA], a[cursorA+1:cursorA+1+lengthA])
	a[dest] = tmp[cursorTemp]
}
