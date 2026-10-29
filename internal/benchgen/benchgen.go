// Package benchgen builds the workload every benchmark uses, so cheesedb,
// SQLite and PostgreSQL are measured on exactly the same data. It is only
// used by benchmarks (the main module's and the separate /bench module's).
//
// The data is generated from a fixed seed, so every run and every engine
// gets the same graph.
package benchgen

import (
	"bytes"
	"math/rand/v2"
	"slices"
)

// Content is stored with every lot and memory: 100 bytes, about a sentence.
var Content = bytes.Repeat([]byte("x"), 100)

// SourceType labels every generated lot.
const SourceType = "tool"

// Op is one write. Ids are implicit: op i (from 0) gets id i+1, as in
// cheesedb, where ids are assigned 1, 2, 3, ... in write order.
type Op struct {
	Lot  bool
	Legs [][]uint64 // memories only; each leg's parents sorted
}

// Window is how far back a memory's parents can be. Parents come from the
// recent past, which is how an agent learns (from what it just saw) and
// keeps recall cascades local rather than spanning the whole ledger.
const Window = 1000

// Dataset returns the ops that build a ledger holding the given number of
// memories. Every 10th id is a lot; each memory has 1-2 legs of 1-2
// parents, chosen from the previous Window ids.
func Dataset(memories int) []Op {
	rng := rand.New(rand.NewPCG(42, 42))
	var ops []Op
	for made := 0; made < memories; {
		id := uint64(len(ops) + 1)
		if id%10 == 1 {
			ops = append(ops, Op{Lot: true})
			continue
		}
		window := min(id-1, Window)
		var legs [][]uint64
		for range 1 + rng.IntN(2) {
			leg := []uint64{id - 1 - rng.Uint64N(window)}
			if p := id - 1 - rng.Uint64N(window); rng.IntN(2) == 0 && p != leg[0] {
				leg = append(leg, p)
			}
			slices.Sort(leg)
			if len(legs) == 0 || !slices.Equal(legs[0], leg) { // no identical legs
				legs = append(legs, leg)
			}
		}
		ops = append(ops, Op{Legs: legs})
		made++
	}
	return ops
}

// Probe returns ops, starting at id first, for a lot P followed by width
// memories learned only from P, then width memories each learned only from
// one of those. A Fair recall of P (id first) removes exactly 2*width
// memories, whatever else the ledger holds, so recall cost can be compared
// across ledger sizes.
func Probe(first uint64, width int) []Op {
	ops := []Op{{Lot: true}}
	for range width {
		ops = append(ops, Op{Legs: [][]uint64{{first}}})
	}
	for i := range width {
		ops = append(ops, Op{Legs: [][]uint64{{first + 1 + uint64(i)}}})
	}
	return ops
}

// IsMemory reports whether id is a memory in a Dataset (lots are every 10th
// id, starting at 1).
func IsMemory(id uint64) bool { return id%10 != 1 }
