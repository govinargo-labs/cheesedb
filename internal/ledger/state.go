package ledger

import (
	"fmt"
	"slices"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// state is the in-memory picture of a ledger: the family tree and the leg
// counters. It is rebuilt by replay and never saved to disk.
//
// It does no I/O and takes no locks: it only answers "is this allowed?"
// (checkParents) and applies records (addLot, addMemory, recall). Keeping it
// pure means it can be tested without a disk, and, if several writers per
// agent are ever allowed, the same code can move into the committer and run
// there in id order.
type state struct {
	nodes        []node // nodes[id-1]; every record has one, RECALLs included
	eraseTargets []ID   // every id recalled with reason Erase
}

type node struct {
	kind      record.Type
	dead      bool
	redacted  bool       // content erased from disk; fits in dead's padding
	legs      []leg      // memories only
	aliveLegs uint32     // legs with no dead parent; 0 means the memory is dead
	children  []childRef // memories that list this node in one of their legs
}

type leg struct {
	parents     []ID
	deadParents uint32 // the leg is alive while this is 0
}

// childRef says "memory child depends on me through its leg number leg".
// Keeping the leg number means a death updates exactly one counter.
type childRef struct {
	child ID
	leg   uint32
}

func (s *state) node(id ID) *node { return &s.nodes[id-1] }

// isLotOrMemory reports whether id names an existing lot or memory, the only
// things that can be parents, recalled, or read.
func (s *state) isLotOrMemory(id ID) bool {
	if id == 0 || id > ID(len(s.nodes)) {
		return false
	}
	k := s.node(id).kind
	return k == record.TypeLot || k == record.TypeMemory
}

// checkParents enforces Remember's rules on legs canonicalLegs has already
// shaped: every parent is an existing lot or memory, and alive.
func (s *state) checkParents(legs []Leg) error {
	for _, leg := range legs {
		for _, p := range leg {
			if !s.isLotOrMemory(p) {
				return fmt.Errorf("%w: parent %d is not a lot or memory", ErrUnknownID, p)
			}
			if s.node(p).dead {
				return fmt.Errorf("%w: parent %d", ErrDeadParent, p)
			}
		}
	}
	return nil
}

// checkNext guards against the state and the log disagreeing about ids,
// which would be a bug: the log assigns ids, the state mirrors them.
func (s *state) checkNext(id ID) error {
	if want := ID(len(s.nodes)) + 1; id != want {
		return fmt.Errorf("ledger: state expected id %d, log gave %d", want, id)
	}
	return nil
}

func (s *state) addLot(id ID) error {
	if err := s.checkNext(id); err != nil {
		return err
	}
	s.nodes = append(s.nodes, node{kind: record.TypeLot})
	return nil
}

// addMemory adds memory id with legs whose parents checkParents accepted,
// so every leg starts alive. It also gives each parent a link back to the
// new memory, so a recall can walk down from parent to child.
func (s *state) addMemory(id ID, legs []Leg) error {
	if err := s.checkNext(id); err != nil {
		return err
	}
	n := node{kind: record.TypeMemory, legs: make([]leg, len(legs)), aliveLegs: uint32(len(legs))}
	for i, ps := range legs {
		n.legs[i] = leg{parents: ps}
		for _, p := range ps {
			parent := s.node(p)
			parent.children = append(parent.children, childRef{child: id, leg: uint32(i)})
		}
	}
	s.nodes = append(s.nodes, n)
	return nil
}

// recall records RECALL id against target and returns the memories that
// died because of it, in id order.
//
// The walk goes down the child links with a queue, a to-do list of nodes
// still to visit (breadth-first search). Recursion would instead keep one
// waiting function call per level of depth; Go allows deep recursion (up to
// 1 GB of stack), but the queue has no depth limit at all and shows exactly
// what is left to do (iterative, never recursion). Its
// work is proportional to the nodes it reaches, never the whole ledger.
func (s *state) recall(id, target ID, m Mode) ([]ID, error) {
	if err := s.checkNext(id); err != nil {
		return nil, err
	}
	s.nodes = append(s.nodes, node{kind: record.TypeRecall})

	var removed []ID
	t := s.node(target)
	if m == Fair && t.dead {
		return nil, nil // its death already reached its children
	}
	if !t.dead {
		removed = s.die(target, removed)
	}

	// Fair: a child dies only when its last leg dies.
	// Strict: every descendant dies. Strict must also walk THROUGH nodes that
	// are already dead: an earlier Fair recall may have killed a node while
	// one of its children survived on another leg, and that child still has
	// the target in its ancestry.
	queue := []ID{target}
	var visited map[ID]bool
	if m == Strict {
		visited = map[ID]bool{target: true}
	}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, ref := range s.node(x).children {
			c := s.node(ref.child)
			switch {
			case m == Fair:
				if !c.dead && c.aliveLegs == 0 {
					removed = s.die(ref.child, removed)
					queue = append(queue, ref.child)
				}
			case !visited[ref.child]:
				visited[ref.child] = true
				if !c.dead {
					removed = s.die(ref.child, removed)
				}
				queue = append(queue, ref.child)
			}
		}
	}
	slices.Sort(removed)
	return removed, nil
}

// die marks id dead and tells each child that one of its legs lost a
// parent. A leg dies when its first parent dies (0 -> 1), and each leg
// lists a parent at most once, so every leg is counted dead exactly once.
// Counters are updated in both modes so that later recalls stay correct.
func (s *state) die(id ID, removed []ID) []ID {
	n := s.node(id)
	n.dead = true
	if n.kind == record.TypeMemory {
		removed = append(removed, id)
	}
	for _, ref := range n.children {
		c := s.node(ref.child)
		l := &c.legs[ref.leg]
		l.deadParents++
		if l.deadParents == 1 {
			c.aliveLegs--
		}
	}
	return removed
}

// eraseSet returns what erasing target removes: the target and
// every dead lot or memory below it, through any leg, whose content is
// still on disk. Memories below it that are still ALIVE keep their content:
// another source still backs them. The walk passes through alive nodes too,
// because a dead memory can sit below an alive one.
func (s *state) eraseSet(target ID) []ID {
	var ids []ID
	if !s.node(target).redacted {
		ids = append(ids, target)
	}
	visited := map[ID]bool{target: true}
	queue := []ID{target}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, ref := range s.node(x).children {
			if visited[ref.child] {
				continue
			}
			visited[ref.child] = true
			if c := s.node(ref.child); c.dead && !c.redacted {
				ids = append(ids, ref.child)
			}
			queue = append(queue, ref.child)
		}
	}
	slices.Sort(ids)
	return ids
}

// pendingErase returns what every Erase recall should have removed but is
// still on disk: the rest of an erase that a crash interrupted. It is one
// pass in id order (parents always come first), so it costs the same
// whatever the number of erase recalls.
func (s *state) pendingErase() []ID {
	if len(s.eraseTargets) == 0 {
		return nil
	}
	below := make([]bool, len(s.nodes)+1) // below[id]: id is an erase target or descends from one
	for _, t := range s.eraseTargets {
		below[t] = true
	}
	var ids []ID
	for i := range s.nodes {
		id, n := ID(i+1), &s.nodes[i]
		if n.kind == record.TypeRecall {
			continue
		}
		for _, l := range n.legs {
			for _, p := range l.parents {
				below[id] = below[id] || below[p]
			}
		}
		if below[id] && n.dead && !n.redacted {
			ids = append(ids, id)
		}
	}
	return ids
}

// deadWithContent returns every dead lot or memory whose content is still
// on disk: what Compact erases.
func (s *state) deadWithContent() []ID {
	var ids []ID
	for i, n := range s.nodes {
		if n.kind != record.TypeRecall && n.dead && !n.redacted {
			ids = append(ids, ID(i+1))
		}
	}
	return ids
}

func (s *state) markRedacted(ids []ID) {
	for _, id := range ids {
		s.node(id).redacted = true
	}
}

// apply replays one record from the log into the state. Anything the live
// API would have refused means the log is damaged or forged.
func (s *state) apply(r *record.Record) error {
	id := ID(r.ID)
	switch r.Type {
	case record.TypeLot:
		if _, err := decodeLot(r.Structure); err != nil {
			return err
		}
		if err := s.addLot(id); err != nil {
			return err
		}
		s.node(id).redacted = r.Redacted()
		return nil

	case record.TypeMemory:
		legs, err := decodeMemory(r.Structure, id)
		if err != nil {
			return err
		}
		// Live writes check parents and append under one lock, so a valid
		// log never holds a memory whose parent was already dead.
		if err := s.checkParents(legs); err != nil {
			return fmt.Errorf("%w: memory %d: %v", errMalformed, id, err)
		}
		if err := s.addMemory(id, legs); err != nil {
			return err
		}
		s.node(id).redacted = r.Redacted()
		return nil

	case record.TypeRecall:
		target, m, reason, err := decodeRecall(r.Structure, id)
		if err != nil {
			return err
		}
		if !s.isLotOrMemory(target) {
			return fmt.Errorf("%w: recall %d targets %d, not a lot or memory", errMalformed, id, target)
		}
		if _, err = s.recall(id, target, m); err != nil {
			return err
		}
		if reason == Erase {
			s.eraseTargets = append(s.eraseTargets, target)
		}
		return nil
	}
	return fmt.Errorf("%w: unknown record type %d", errMalformed, r.Type)
}
