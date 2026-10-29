package ledger

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// sim drives a state directly, with no log and no disk, the way replay
// does. It lets the graph logic be tested at sizes and speeds the disk
// would not allow.
type sim struct {
	t *testing.T
	s state
}

func (m *sim) next() ID { return ID(len(m.s.nodes)) + 1 }

func (m *sim) lot() ID {
	m.t.Helper()
	id := m.next()
	if err := m.s.addLot(id); err != nil {
		m.t.Fatal(err)
	}
	return id
}

func (m *sim) mem(legs ...Leg) ID {
	m.t.Helper()
	legs, err := canonicalLegs(legs)
	if err == nil {
		err = m.s.checkParents(legs)
	}
	if err != nil {
		m.t.Fatal(err)
	}
	id := m.next()
	if err := m.s.addMemory(id, legs); err != nil {
		m.t.Fatal(err)
	}
	return id
}

func (m *sim) recall(target ID, mode Mode) []ID {
	m.t.Helper()
	removed, err := m.s.recall(m.next(), target, mode)
	if err != nil {
		m.t.Fatal(err)
	}
	return removed
}

type recallOp struct {
	target ID
	mode   Mode
}

// TestDiamondCountsLegOnce: M3's single leg needs M1 AND M2, and both die
// from the same recall. The leg must be counted dead once, not twice; a
// double count would make aliveLegs wrap around below zero.
func TestDiamondCountsLegOnce(t *testing.T) {
	m := &sim{t: t}
	a := m.lot()
	m1 := m.mem(Leg{a})
	m2 := m.mem(Leg{a})
	m3 := m.mem(Leg{m1, m2})

	if got := m.recall(a, Fair); !slices.Equal(got, []ID{m1, m2, m3}) {
		t.Fatalf("removed %v, want [%d %d %d]", got, m1, m2, m3)
	}
	if n := m.s.node(m3); n.aliveLegs != 0 || n.legs[0].deadParents != 2 {
		t.Fatalf("M3: aliveLegs %d, deadParents %d; want 0 and 2", n.aliveLegs, n.legs[0].deadParents)
	}
}

// TestStrictWalksThroughDeadNodes: a Fair recall kills M1 while M2 survives
// on another leg. A later Strict recall of the same lot must still reach
// M2 through the already-dead M1, because the lot is in M2's ancestry.
func TestStrictWalksThroughDeadNodes(t *testing.T) {
	m := &sim{t: t}
	a, x := m.lot(), m.lot()
	m1 := m.mem(Leg{a})
	m2 := m.mem(Leg{m1}, Leg{x})

	if got := m.recall(a, Fair); !slices.Equal(got, []ID{m1}) {
		t.Fatalf("Fair removed %v, want [%d]", got, m1)
	}
	if got := m.recall(a, Fair); got != nil {
		t.Fatalf("a second Fair recall removed %v, want nothing", got)
	}
	if got := m.recall(a, Strict); !slices.Equal(got, []ID{m2}) {
		t.Fatalf("Strict removed %v, want [%d]", got, m2)
	}
}

// TestDeepChainNoRecursion: a chain of 100,000 memories, each learned from
// the one before, is removed by one recall. The queue handles any depth;
// this pins that behaviour so a future change cannot quietly break it.
func TestDeepChainNoRecursion(t *testing.T) {
	for _, mode := range []Mode{Fair, Strict} {
		m := &sim{t: t}
		root := m.lot()
		prev := root
		for range 100_000 {
			prev = m.mem(Leg{prev})
		}
		if got := m.recall(root, mode); len(got) != 100_000 {
			t.Fatalf("mode %d: removed %d memories, want 100000", mode, len(got))
		}
	}
}

// TestMatchesDefinition is the main correctness check. It runs thousands of
// random operations on the incremental state (counters, child links, the
// queue walk), then recomputes every node's fate from scratch using only the
// definitions of Fair and Strict recall, and requires the two to agree, counters
// included.
func TestMatchesDefinition(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) // fixed seed: failures reproduce
	m := &sim{t: t}
	var recalls []recallOp

	for op := range 5000 {
		n := len(m.s.nodes)
		switch r := rng.IntN(10); {
		case r < 3 || n == 0:
			m.lot()
		case r < 8:
			// 1-3 legs of 1-3 parents, mostly recent ids so the graph
			// gets deep, not just wide.
			var legs []Leg
			for range 1 + rng.IntN(3) {
				var leg Leg
				for range 1 + rng.IntN(3) {
					leg = append(leg, ID(n-rng.IntN(min(n, 20))))
				}
				legs = append(legs, leg)
			}
			legs, err := canonicalLegs(legs)
			if err != nil || m.s.checkParents(legs) != nil {
				continue // a random pick may repeat ids or hit a dead or RECALL node
			}
			m.mem(legs...)
		default:
			target := ID(1 + rng.IntN(n))
			if !m.s.isLotOrMemory(target) {
				continue
			}
			mode := Fair
			if rng.IntN(3) == 0 {
				mode = Strict
			}
			m.recall(target, mode)
			recalls = append(recalls, recallOp{target, mode})
		}
		if op%500 == 499 {
			checkAgainstDefinition(t, &m.s, recalls)
		}
	}
	checkAgainstDefinition(t, &m.s, recalls)
}

// checkAgainstDefinition recomputes liveness from the rules alone, in id
// order (parents always have smaller ids, so they are decided first):
//   - a recalled node is dead;
//   - a node below a Strict-recalled node (through any leg) is dead;
//   - a memory whose every leg has a dead parent is dead (Fair).
//
// The rules only ever add deaths, so the order recalls happened in cannot
// change the answer.
func checkAgainstDefinition(t *testing.T, s *state, recalls []recallOp) {
	t.Helper()
	size := len(s.nodes) + 1
	recalled, strictBelow, dead := make([]bool, size), make([]bool, size), make([]bool, size)
	for _, r := range recalls {
		recalled[r.target] = true
		if r.mode == Strict {
			strictBelow[r.target] = true
		}
	}
	for i := range s.nodes {
		id := ID(i + 1)
		n := &s.nodes[i]
		if n.kind == record.TypeRecall {
			continue
		}
		dead[id] = recalled[id] || strictBelow[id]
		allLegsDead := n.kind == record.TypeMemory
		aliveLegs := uint32(0)
		for li, l := range n.legs {
			var deadParents uint32
			for _, p := range l.parents {
				strictBelow[id] = strictBelow[id] || strictBelow[p]
				if dead[p] {
					deadParents++
				}
			}
			if deadParents == 0 {
				allLegsDead = false
				aliveLegs++
			}
			if l.deadParents != deadParents {
				t.Fatalf("node %d leg %d: deadParents %d, definition says %d", id, li, l.deadParents, deadParents)
			}
		}
		dead[id] = dead[id] || strictBelow[id] || allLegsDead
		if n.dead != dead[id] {
			t.Fatalf("node %d: dead=%v, definition says %v", id, n.dead, dead[id])
		}
		if n.kind == record.TypeMemory && n.aliveLegs != aliveLegs {
			t.Fatalf("node %d: aliveLegs %d, definition says %d", id, n.aliveLegs, aliveLegs)
		}
	}
}
