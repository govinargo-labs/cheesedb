package ledger

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

// Snapshots: a copy of the state on disk, so Open replays only the records
// written after it instead of the whole log. See ARCHITECTURE.md section 3.
//
// The log stays the truth. The snapshot is only a shortcut: Open checks it
// against the log (logfile.OpenAt), and if anything does not match, or the
// file is damaged, it is thrown away and the whole log is replayed as
// before. A bad snapshot can make Open slower, never wrong.
//
// It holds no content: ids, kinds, flags and legs only, so erased text
// never ends up in it.

const (
	snapFile    = "state.snap"
	snapMagic   = "cdbsnap\x00"
	snapVersion = 1

	// snapshotEvery is how many new records Close waits for before it
	// writes a new snapshot. Replaying 1,000 records takes about 1 ms,
	// and writing a snapshot of a 1M-record agent about 30 ms, so a
	// command that adds one record and exits writes one every 1,000 runs.
	snapshotEvery = 1000
)

// node flags in a snapshot
const (
	snapDead     = 1 << 4
	snapRedacted = 1 << 5
	snapKindMask = 0x0f
)

// state.snap layout (integers are uvarints unless a size is given):
//
//	magic        8 bytes, "cdbsnap\0"
//	version      1 byte, currently 1
//	position     where the log stood (logfile.Position): records covered,
//	             their last chain hash, each segment's size, record count
//	             and last chain hash, and the offset of every record
//	nodes        how many follow; equal to the records covered
//	per node:
//	  kind+flags 1 byte: kind (1 lot, 2 memory, 3 recall) in the low 4
//	             bits, 16 = dead, 32 = content erased
//	  legs       memories only: their length, then the legs encoded as in
//	             a MEMORY record (section 4)
//	erase        how many erase targets follow, then each id
//	crc          4 bytes, CRC-32C over everything before it
//
// Children and counters are not stored: load rebuilds them from the legs
// and the dead flags, with the same code replay uses.

// encodeSnapshot encodes s as it stands at position p.
func encodeSnapshot(p *logfile.Position, s *state) []byte {
	b := make([]byte, 0, 64+len(s.nodes)*20)
	b = append(b, snapMagic...)
	b = append(b, snapVersion)
	b = p.AppendEncoded(b)
	b = binary.AppendUvarint(b, uint64(len(s.nodes)))
	for i := range s.nodes {
		n := &s.nodes[i]
		c := byte(n.kind)
		if n.dead {
			c |= snapDead
		}
		if n.redacted {
			c |= snapRedacted
		}
		b = append(b, c)
		if n.kind == record.TypeMemory {
			legs := make([]Leg, len(n.legs))
			for j, l := range n.legs {
				legs[j] = l.parents
			}
			enc := encodeMemory(legs)
			b = binary.AppendUvarint(b, uint64(len(enc)))
			b = append(b, enc...)
		}
	}
	b = binary.AppendUvarint(b, uint64(len(s.eraseTargets)))
	for _, t := range s.eraseTargets {
		b = binary.AppendUvarint(b, uint64(t))
	}
	return binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, castagnoli))
}

// decodeSnapshot checks and decodes a snapshot file. It refuses anything
// encodeSnapshot never writes, and any state replay could not have built
// (a parent that is not a lot or memory, a live memory with no live leg, an
// erase target that is alive).
func decodeSnapshot(b []byte) (*logfile.Position, state, error) {
	head := len(snapMagic) + 1
	if len(b) < head+4 || string(b[:len(snapMagic)]) != snapMagic {
		return nil, state{}, errors.New("not a snapshot file")
	}
	if b[len(snapMagic)] != snapVersion {
		return nil, state{}, fmt.Errorf("unknown snapshot version %d", b[len(snapMagic)])
	}
	end := len(b) - 4
	if crc32.Checksum(b[:end], castagnoli) != binary.LittleEndian.Uint32(b[end:]) {
		return nil, state{}, errors.New("snapshot checksum does not match")
	}
	p, used, err := logfile.DecodePosition(b[head:end])
	if err != nil {
		return nil, state{}, err
	}
	s, err := decodeState(b[head+used : end])
	if err != nil {
		return nil, state{}, err
	}
	if uint64(len(s.nodes)) != p.Records {
		return nil, state{}, fmt.Errorf("snapshot has %d nodes for %d records", len(s.nodes), p.Records)
	}
	return p, s, nil
}

// decodeState rebuilds the state from its snapshot encoding. It gives the
// same nodes, legs, children and counters as replaying the records would
// (tests compare the two field by field), but with a few large
// allocations instead of several per memory, which is most of what makes
// it faster than replay:
//   - pass 1 reads and checks every node into flat lists, and counts each
//     node's children;
//   - pass 2 hands each node slices of shared arrays (legs, parents,
//     children), each with its exact size as capacity. A later append to
//     a node's children then moves that list to a new array, never over
//     its neighbour's.
func decodeState(b []byte) (state, error) {
	d := decoder{b: b}
	count := d.count()
	flags := make([]byte, count)
	legsTo := make([]int, count) // node i's legs end at legEnds[legsTo[i]] (memories only add legs)
	var legEnds []int            // leg j's parents end at parents[legEnds[j]]
	var parents []ID             // every leg's parents, in id and leg order
	nChildren := make([]uint32, count)
	for i := range count {
		id := ID(i + 1)
		c := d.byte()
		if d.err != nil {
			break
		}
		if c&^(snapKindMask|snapDead|snapRedacted) != 0 {
			return state{}, fmt.Errorf("%w: node %d has unknown flags %#x", errMalformed, id, c)
		}
		flags[i] = c
		switch k := record.Type(c & snapKindMask); k {
		case record.TypeLot:
		case record.TypeMemory:
			from := len(parents)
			var err error
			parents, legEnds, err = appendLegs(d.fixed(d.count()), id, parents, legEnds)
			if d.err != nil {
				return state{}, d.err
			}
			if err != nil {
				return state{}, err
			}
			for _, p := range parents[from:] {
				if k := record.Type(flags[p-1] & snapKindMask); k != record.TypeLot && k != record.TypeMemory {
					return state{}, fmt.Errorf("%w: memory %d: parent %d is not a lot or memory", errMalformed, id, p)
				}
				nChildren[p-1]++
			}
		case record.TypeRecall:
			if c != byte(k) {
				return state{}, fmt.Errorf("%w: recall %d has flags", errMalformed, id)
			}
		default:
			return state{}, fmt.Errorf("%w: node %d has kind %d", errMalformed, id, k)
		}
		legsTo[i] = len(legEnds)
	}
	var targets []ID
	for range d.count() {
		t := ID(d.uvarint())
		if d.err == nil && (t == 0 || t > ID(count) || flags[t-1]&snapDead == 0 ||
			(record.Type(flags[t-1]&snapKindMask) != record.TypeLot && record.Type(flags[t-1]&snapKindMask) != record.TypeMemory)) {
			return state{}, fmt.Errorf("%w: erase target %d is not a dead lot or memory", errMalformed, t)
		}
		targets = append(targets, t)
	}
	if err := d.finish(); err != nil {
		return state{}, err
	}

	// Pass 2: build the nodes on shared arrays.
	s := state{nodes: make([]node, count), eraseTargets: targets}
	legArena := make([]leg, len(legEnds))
	var total int
	for _, n := range nChildren {
		total += int(n)
	}
	childArena := make([]childRef, total)
	for i := range s.nodes {
		n := &s.nodes[i]
		n.kind = record.Type(flags[i] & snapKindMask)
		n.dead = flags[i]&snapDead != 0
		n.redacted = flags[i]&snapRedacted != 0
		if c := int(nChildren[i]); c > 0 {
			n.children, childArena = childArena[:0:c], childArena[c:]
		}
	}
	leg0, par0 := 0, 0
	for i := range s.nodes {
		id, n := ID(i+1), &s.nodes[i]
		if n.kind != record.TypeMemory {
			continue
		}
		n.legs = legArena[leg0:legsTo[i]:legsTo[i]]
		for j := range n.legs {
			l := &n.legs[j]
			end := legEnds[leg0+j]
			l.parents = parents[par0:end:end]
			for _, p := range l.parents {
				parent := s.node(p)
				parent.children = append(parent.children, childRef{child: id, leg: uint32(j)})
				if parent.dead {
					l.deadParents++
				}
			}
			if l.deadParents == 0 {
				n.aliveLegs++
			}
			par0 = end
		}
		leg0 = legsTo[i]
		if !n.dead && n.aliveLegs == 0 {
			return state{}, fmt.Errorf("%w: memory %d is alive with no live leg", errMalformed, id)
		}
	}
	return s, nil
}

// appendLegs decodes memory self's legs (encoded as in a MEMORY record),
// appending their parents to parents and each leg's end to ends. It checks
// what decodeMemory checks.
func appendLegs(b []byte, self ID, parents []ID, ends []int) ([]ID, []int, error) {
	d := decoder{b: b}
	nLegs := d.count()
	if d.err == nil && nLegs == 0 {
		return nil, nil, fmt.Errorf("%w: memory with no legs", errMalformed)
	}
	firstLeg := len(ends)
	for range nLegs {
		nParents := d.count()
		if d.err == nil && nParents == 0 {
			return nil, nil, fmt.Errorf("%w: empty leg", errMalformed)
		}
		start := len(parents)
		for range nParents {
			p := ID(d.uvarint())
			if d.err != nil {
				break
			}
			if p == 0 || p >= self {
				return nil, nil, fmt.Errorf("%w: parent %d of memory %d must be between 1 and %d", errMalformed, p, self, self-1)
			}
			if len(parents) > start && p <= parents[len(parents)-1] {
				return nil, nil, fmt.Errorf("%w: parents not sorted or repeated", errMalformed)
			}
			parents = append(parents, p)
		}
		ends = append(ends, len(parents))
	}
	if err := d.finish(); err != nil {
		return nil, nil, err
	}
	if nLegs > 1 && hasIdenticalLegs(parents, ends, firstLeg) {
		return nil, nil, fmt.Errorf("%w: identical legs", errMalformed)
	}
	return parents, ends, nil
}

// hasIdenticalLegs reports whether two of the legs from ends[first] on are
// equal. A few legs are compared pair by pair, which needs no map; many
// legs go through legSet, in one pass, as decodeMemory does.
func hasIdenticalLegs(parents []ID, ends []int, first int) bool {
	leg := func(j int) Leg {
		start := 0
		if j > 0 {
			start = ends[j-1]
		}
		return parents[start:ends[j]]
	}
	if len(ends)-first <= 4 {
		for i := first; i < len(ends); i++ {
			for j := i + 1; j < len(ends); j++ {
				if slices.Equal(leg(i), leg(j)) {
					return true
				}
			}
		}
		return false
	}
	seen := legSet{}
	for j := first; j < len(ends); j++ {
		if _, dup := seen.add(leg(j), j); dup {
			return true
		}
	}
	return false
}

// readSnapshot reads dir's snapshot. No file is not an error: it returns
// nil.
func readSnapshot(dir string) (*logfile.Position, state, error) {
	b, err := os.ReadFile(filepath.Join(dir, snapFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, state{}, nil
	}
	if err != nil {
		return nil, state{}, err
	}
	return decodeSnapshot(b)
}

// writeSnapshot writes b as dir's snapshot atomically, the same way seals
// are written: a temporary file, fsync, rename over the final name, fsync
// the folder. A crash leaves the old snapshot or the new one, never part
// of one. A leftover temporary file ends in ".tmp", so the log's Open
// removes it.
func writeSnapshot(dir string, b []byte) error {
	final := filepath.Join(dir, snapFile)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// removeSnapshot deletes dir's snapshot, if there is one, and makes the
// deletion durable.
func removeSnapshot(dir string) error {
	err := os.Remove(filepath.Join(dir, snapFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(dir)
}

// saveSnapshot writes a snapshot of the state as it is now. Callers hold
// l.write, so no record is added while it runs.
func (l *Ledger) saveSnapshot() error {
	p, err := l.log.Position()
	if err != nil || p == nil {
		return err
	}
	// Reading st without mu is safe: only holders of l.write change it.
	if p.Records != uint64(len(l.st.nodes)) {
		return fmt.Errorf("ledger: the state has %d records, the log %d", len(l.st.nodes), p.Records)
	}
	if err := writeSnapshot(l.dir, encodeSnapshot(p, &l.st)); err != nil {
		return err
	}
	l.snapAt = p.Records
	return nil
}

// snapshotDue reports whether Close should write a new snapshot.
func (l *Ledger) snapshotDue() bool {
	return !l.snapBad && uint64(len(l.st.nodes)) >= l.snapAt+snapshotEvery
}

// openLog opens the main log, from the snapshot if there is a good one.
// If the snapshot is damaged or does not match the log, it replays the
// whole log instead and deletes the snapshot.
func (l *Ledger) openLog() (*logfile.Log, logfile.Report, error) {
	apply := func(r *record.Record) error {
		if err := l.st.apply(r); err != nil {
			return fmt.Errorf("%w: record %d: %v", logfile.ErrCorrupt, r.ID, err)
		}
		return nil
	}
	p, st, snapErr := readSnapshot(l.dir)
	var dropped int64
	if p != nil {
		l.st = st
		log, rep, err := logfile.OpenAt(l.dir, l.opts, p, apply)
		if err == nil {
			l.snapAt = p.Records
			rep.Snapshot = int(p.Records)
			return log, rep, nil
		}
		// Whatever went wrong, the full replay below finds out what is
		// really on disk. A torn tail OpenAt already cut is not there any
		// more, so its count is carried over.
		dropped = rep.DroppedBytes
		l.st = state{}
	}
	log, rep, err := logfile.Open(l.dir, l.opts, apply)
	if err != nil {
		return nil, rep, err
	}
	rep.DroppedBytes += dropped
	if p != nil || snapErr != nil {
		// Best effort: a snapshot that does not fit only costs time, and
		// the next Close writes a good one.
		removeSnapshot(l.dir)
	}
	return log, rep, nil
}
