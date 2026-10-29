// Package ledger is the memory logic on top of the log: lots (sources),
// memories with legs (how each was learned), and recall (removing a source
// and everything that depended on it).
//
// Every change is first written to the log as a record and only then
// applied to the in-memory state, so replaying the log on Open rebuilds the
// exact same state. See ARCHITECTURE.md sections 1 and 3.
package ledger

import (
	"errors"
	"fmt"
	"sync"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

// ID names a record: lot, memory or recall. IDs are 1, 2, 3, ... per ledger.
type ID uint64

// Leg is one independent way a memory was learned. It holds while ALL its
// parents (lots or memories) are alive.
type Leg []ID

// Mode chooses how far a recall reaches.
type Mode uint8

const (
	Fair   Mode = 1 // a memory survives while ANY of its legs is alive
	Strict Mode = 2 // a memory dies if the recalled node is ANYWHERE in its ancestry
)

func (m Mode) valid() bool { return m == Fair || m == Strict }

// Reason says why something is recalled. It is stored in the RECALL record.
type Reason uint8

const (
	Untrusted Reason = 1 // the source was wrong or poisoned
	Erase     Reason = 2 // the content must be erased (redaction)
)

func (r Reason) valid() bool { return r == Untrusted || r == Erase }

var (
	ErrUnknownID   = errors.New("ledger: no lot or memory with this id")
	ErrDeadParent  = errors.New("ledger: parent is recalled or dead")
	ErrBadLegs     = errors.New("ledger: invalid legs")
	ErrBadArgument = errors.New("ledger: invalid argument")
)

// Item is what Get returns.
type Item struct {
	ID       ID
	Kind     record.Type // record.TypeLot or record.TypeMemory
	Data     []byte      // nil once redacted
	Alive    bool
	Redacted bool // the content was erased from disk (Erase or Compact)
}

// Ledger is one agent's open ledger.
type Ledger struct {
	log *logfile.Log

	// write lets one change through at a time. Remember checks its parents are alive and then appends;
	// without this lock a Recall could kill a parent in between, and the
	// log would hold a memory built on a dead parent. Cost: writes to one
	// agent each wait for their own fsync. Different agents are unaffected.
	write sync.Mutex

	// mu guards st for readers. Writers hold it only for the quick
	// in-memory update after the fsync, so reads never wait for the disk.
	mu sync.RWMutex
	st state
}

// Open opens (or creates) the ledger in dir and rebuilds its state by
// replaying every record.
func Open(dir string, opts logfile.Options) (*Ledger, logfile.Report, error) {
	l := &Ledger{}
	log, rep, err := logfile.Open(dir, opts, func(r *record.Record) error {
		if err := l.st.apply(r); err != nil {
			return fmt.Errorf("%w: record %d: %v", logfile.ErrCorrupt, r.ID, err)
		}
		return nil
	})
	if err != nil {
		return nil, rep, err
	}
	l.log = log

	// An Erase recall is on disk before its content is erased. If a crash
	// came in between, finish the erase now. Erasing is idempotent, so
	// this is safe to repeat.
	if ids := l.st.pendingErase(); len(ids) > 0 {
		if err := l.redact(ids); err != nil {
			log.Close()
			return nil, rep, fmt.Errorf("ledger: finishing an interrupted erase: %w", err)
		}
		rep.FinishedErase = len(ids)
	}
	return l, rep, nil
}

// redact erases ids' content on disk, then records that in the state.
// Callers hold l.write (or are still in Open).
func (l *Ledger) redact(ids []ID) error {
	raw := make([]uint64, len(ids))
	for i, id := range ids {
		raw[i] = uint64(id)
	}
	if err := l.log.Redact(raw); err != nil {
		return err
	}
	l.mu.Lock()
	l.st.markRedacted(ids)
	l.mu.Unlock()
	return nil
}

// Verify re-reads the whole log from disk and checks every record, the hash
// chain linking them, and every seal. Open did the same; Verify catches
// files changed since. It returns logfile.ErrCorrupt on any mismatch, and
// otherwise the head: the newest record's chain hash, worth keeping
// elsewhere (see logfile.Log.Verify).
func (l *Ledger) Verify() ([32]byte, error) { return l.log.Verify() }

// Close closes the log. Everything acknowledged is already on disk.
func (l *Ledger) Close() error { return l.log.Close() }

// AddLot registers a source. sourceType is the caller's label for it, such
// as "user_message" or "web_page"; the engine does not judge it.
func (l *Ledger) AddLot(sourceType string, data []byte) (ID, error) {
	if err := checkSourceType(sourceType); err != nil {
		return 0, err
	}
	l.write.Lock()
	defer l.write.Unlock()
	id, err := l.log.AppendBatch([]logfile.Entry{{Type: record.TypeLot, Structure: encodeLot(sourceType), Data: data}})
	if err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return ID(id), l.st.addLot(ID(id))
}

// Remember stores a memory learned through one or more legs. Every parent
// must be an existing lot or memory that is still alive.
func (l *Ledger) Remember(data []byte, legs ...Leg) (ID, error) {
	legs, err := canonicalLegs(legs)
	if err != nil {
		return 0, err
	}
	l.write.Lock()
	defer l.write.Unlock()
	// Reading st without mu is safe here: only holders of l.write change it.
	if err := l.st.checkParents(legs); err != nil {
		return 0, err
	}
	id, err := l.log.AppendBatch([]logfile.Entry{{Type: record.TypeMemory, Structure: encodeMemory(legs), Data: data}})
	if err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return ID(id), l.st.addMemory(ID(id), legs)
}

// Recall recalls a lot or memory and returns the memories that died because
// of it (in id order), so the caller can remove them elsewhere too, such as
// a vector store. Recall is permanent. Recalling something again is allowed:
// a Strict recall after a Fair one can remove more.
//
// With reason Erase it also erases, before returning, the content of the
// target and of every dead lot or memory below it. Memories still
// alive through another source keep their content. If the erase fails, the
// recall itself is already on disk, and the next Open finishes the erase.
func (l *Ledger) Recall(target ID, m Mode, r Reason) ([]ID, error) {
	if !m.valid() || !r.valid() {
		return nil, fmt.Errorf("%w: mode %d, reason %d", ErrBadArgument, m, r)
	}
	l.write.Lock()
	defer l.write.Unlock()
	if !l.st.isLotOrMemory(target) {
		return nil, fmt.Errorf("%w: %d", ErrUnknownID, target)
	}
	id, err := l.log.AppendBatch([]logfile.Entry{{Type: record.TypeRecall, Structure: encodeRecall(target, m, r)}})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	removed, err := l.st.recall(ID(id), target, m)
	var erase []ID
	if err == nil && r == Erase {
		l.st.eraseTargets = append(l.st.eraseTargets, target)
		erase = l.st.eraseSet(target)
	}
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := l.redact(erase); err != nil {
		return removed, fmt.Errorf("ledger: recall %d is saved, but erasing content failed (the next Open retries): %w", id, err)
	}
	return removed, nil
}

// Compact erases the content of every dead lot or memory that still has it,
// for example those recalled as Untrusted, and returns how many it erased.
// It runs only when called (automatic compaction is planned for later).
// The family tree, counters and hash chain are unchanged.
func (l *Ledger) Compact() (int, error) {
	l.write.Lock()
	defer l.write.Unlock()
	l.mu.RLock()
	ids := l.st.deadWithContent()
	l.mu.RUnlock()
	if err := l.redact(ids); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// Active returns the ids of all live memories, in id order.
func (l *Ledger) Active() []ID {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var ids []ID
	for i, n := range l.st.nodes {
		if n.kind == record.TypeMemory && !n.dead {
			ids = append(ids, ID(i+1))
		}
	}
	return ids
}

// Get returns a lot or memory: its kind, whether it is alive, and its data,
// read from disk.
func (l *Ledger) Get(id ID) (Item, error) {
	l.mu.RLock()
	ok := l.st.isLotOrMemory(id)
	var it Item
	if ok {
		n := l.st.node(id)
		it = Item{ID: id, Kind: n.kind, Alive: !n.dead, Redacted: n.redacted}
	}
	l.mu.RUnlock()
	if !ok {
		return Item{}, fmt.Errorf("%w: %d", ErrUnknownID, id)
	}

	r, err := l.log.Read(uint64(id))
	if err != nil {
		return Item{}, err
	}
	it.Data = r.Data()
	return it, nil
}
