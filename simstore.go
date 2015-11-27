// Package simstore implements a storage layer for simhash locality-sensitive hashes.
/*

This package is an implementation of section 3 of "Detecting Near-Duplicates
for Web Crawling" by Manku, Jain, and Sarma,

    http://www2007.org/papers/paper215.pdf

It is hard-coded for hamming distance 3 or 6.
*/
package simstore

import (
	"runtime"
	"sort"
	"sync"

	"github.com/dgryski/go-bits"
)

// TODO(dgryski): split hashes and docid into different arrays to optimize cache usage
// TODO(dgryski): t.add(hash, docid) to make struct change easier

type entry struct {
	hash  uint64
	docid uint64
}

// TODO(dgryski): table persistent via mmap
type table []entry

func (t table) Len() int           { return len(t) }
func (t table) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
func (t table) Less(i, j int) bool { return t[i].hash < t[j].hash }

const mask3 = 0xfffffff000000000

func (t table) find(sig uint64) []uint64 {

	// TODO(dgryski): interpolation search instead of binary search; 2x speed up vs. sort.Search()
	i := sort.Search(len(t), func(i int) bool { return t[i].hash >= sig })

	var ids []uint64

	for i < len(t) && t[i].hash == sig {
		ids = append(ids, t[i].docid)
		i++
	}

	return ids
}

func NewU64Slice(hashes int) u64store {
	u := make(u64slice, 0, hashes)
	return &u
}

type u64store interface {
	add(hash uint64)
	find(sig uint64, mask uint64, d int) []uint64
	finish()
}

// a store for uint64s
type u64slice []uint64

func (u u64slice) Len() int               { return len(u) }
func (u u64slice) Less(i int, j int) bool { return u[i] < u[j] }
func (u u64slice) Swap(i int, j int)      { u[i], u[j] = u[j], u[i] }

func (u u64slice) find(sig, mask uint64, d int) []uint64 {

	prefix := sig & mask
	// TODO(dgryski): interpolation search instead of binary search; 2x speed up vs. sort.Search()
	i := sort.Search(len(u), func(i int) bool { return u[i] >= prefix })

	var ids []uint64

	for i < len(u) && u[i]&mask == prefix {
		if distance(u[i], sig) <= d {
			ids = append(ids, u[i])
		}
		i++
	}

	return ids
}

func (u *u64slice) add(p uint64) {
	*u = append(*u, p)
}

func (u u64slice) finish() {
	sort.Sort(u)
}

// Store is a storage engine for 64-bit hashes
type Store struct {
	docids  table
	rhashes []u64store
}

// New3 returns a Store for searching hamming distance <= 3
func New3(hashes int, newStore func(int) u64store) *Store {
	s := Store{}
	s.rhashes = make([]u64store, 16)
	if hashes != 0 {
		s.docids = make(table, 0, hashes)
		for i := range s.rhashes {
			s.rhashes[i] = newStore(hashes)
		}
	}
	return &s
}

// Add inserts a signature and document id into the store
func (s *Store) Add(sig uint64, docid uint64) {

	var t int

	s.docids = append(s.docids, entry{hash: sig, docid: docid})

	for i := 0; i < 4; i++ {
		p := sig
		s.rhashes[t].add(p)
		t++

		p = (sig & 0xffff000000ffffff) | (sig & 0x0000fff000000000 >> 12) | (sig & 0x0000000fff000000 << 12)
		s.rhashes[t].add(p)
		t++

		p = (sig & 0xffff000fff000fff) | (sig & 0x0000fff000000000 >> 24) | (sig & 0x0000000000fff000 << 24)
		s.rhashes[t].add(p)
		t++

		p = (sig & 0xffff000ffffff000) | (sig & 0x0000fff000000000 >> 36) | (sig & 0x0000000000000fff << 36)
		s.rhashes[t].add(p)
		t++

		sig = (sig << 16) | (sig >> (64 - 16))
	}
}

func (*Store) unshuffle(sig uint64, t int) uint64 {
	const m2 = 0x0000fff000000000

	t4 := t % 4
	shift := 12 * uint64(t4)
	m3 := uint64(m2 >> shift)
	m1 := ^uint64(0) &^ (m2 | m3)

	sig = (sig & m1) | (sig & m2 >> shift) | (sig & m3 << shift)
	sig = (sig >> (16 * (uint64(t) / 4))) | (sig << (64 - (16 * (uint64(t) / 4))))
	return sig
}

func (s *Store) unshuffleList(sigs []uint64, t int) []uint64 {
	for i := range sigs {
		sigs[i] = s.unshuffle(sigs[i], t)
	}

	return sigs
}

type limiter chan struct{}

func (l limiter) enter() { l <- struct{}{} }
func (l limiter) leave() { <-l }

// Finish prepares the store for searching.  This must be called once after all
// the signatures have been added via Add().
func (s *Store) Finish() {

	l := make(limiter, runtime.GOMAXPROCS(0))

	var wg sync.WaitGroup

	sort.Sort(s.docids)

	for i := range s.rhashes {
		l.enter()
		wg.Add(1)
		go func(i int) {
			s.rhashes[i].finish()
			l.leave()
			wg.Done()
		}(i)
	}
	wg.Wait()
}

// Find searches the store for all hashes hamming distance 3 or less from the
// query signature.  It returns the associated list of document ids.
func (s *Store) Find(sig uint64) []uint64 {

	var ids []uint64

	// TODO(dgryski): search in parallel
	var t int
	for i := 0; i < 4; i++ {
		p := sig
		ids = append(ids, s.unshuffleList(s.rhashes[t].find(p, mask3, 3), t)...)
		t++

		p = (sig & 0xffff000000ffffff) | (sig & 0x0000fff000000000 >> 12) | (sig & 0x0000000fff000000 << 12)
		ids = append(ids, s.unshuffleList(s.rhashes[t].find(p, mask3, 3), t)...)
		t++

		p = (sig & 0xffff000fff000fff) | (sig & 0x0000fff000000000 >> 24) | (sig & 0x0000000000fff000 << 24)
		ids = append(ids, s.unshuffleList(s.rhashes[t].find(p, mask3, 3), t)...)
		t++

		p = (sig & 0xffff000ffffff000) | (sig & 0x0000fff000000000 >> 36) | (sig & 0x0000000000000fff << 36)
		ids = append(ids, s.unshuffleList(s.rhashes[t].find(p, mask3, 3), t)...)
		t++

		sig = (sig << 16) | (sig >> (64 - 16))
	}

	ids = unique(ids)

	var docids []uint64
	for _, v := range ids {
		docids = append(docids, s.docids.find(v)...)
	}

	return unique(docids)
}

// SmallStore3 is a simstore for distance k=3 with smaller memory requirements
type SmallStore3 struct {
	tables [4][1 << 16]table
}

func New3Small(hashes int) *SmallStore3 {
	return &SmallStore3{}
}

func (s *SmallStore3) Add(sig uint64, docid uint64) {

	for i := 0; i < 4; i++ {
		prefix := (sig & 0xffff000000000000) >> (64 - 16)
		s.tables[i][prefix] = append(s.tables[i][prefix], entry{hash: sig, docid: docid})
		sig = (sig << 16) | (sig >> (64 - 16))
	}
}

func (s *SmallStore3) Find(sig uint64) []uint64 {
	var ids []uint64
	for i := 0; i < 4; i++ {
		prefix := (sig & 0xffff000000000000) >> (64 - 16)

		t := s.tables[i][prefix]

		for i := range t {
			if distance(t[i].hash, sig) <= 3 {
				ids = append(ids, t[i].docid)
			}
		}
		sig = (sig << 16) | (sig >> (64 - 16))
	}
	return unique(ids)
}

func (s *SmallStore3) Finish() {
	for i := range s.tables {
		for p := range s.tables[i] {
			sort.Sort(s.tables[i][p])
		}
	}
}

func unique(ids []uint64) []uint64 {
	// dedup ids
	uniq := make(map[uint64]struct{})
	for _, id := range ids {
		uniq[id] = struct{}{}
	}

	ids = ids[:0]
	for k := range uniq {
		ids = append(ids, k)
	}

	return ids

}

// TODO(dgryski): replacing this with popcnt() would be ~25% speedup

// distance returns the hamming distance between v1 and v2
func distance(v1 uint64, v2 uint64) int {
	return int(bits.Popcnt(v1 ^ v2))
}
