// Package logfile stores one agent's records in a directory of append-only
// segment files: 000001.seg, 000002.seg, ... Together they form one log.
// Only the highest-numbered segment (the active one) is ever appended to;
// the others are sealed.
//
// Three jobs live here and nowhere else:
//   - durability: a record is acknowledged only after fsync has returned;
//   - group commit: one committer goroutine writes every record that is
//     waiting with a single write + fsync, so concurrent callers share the
//     expensive fsync;
//   - recovery: Open replays the segments, cuts off the torn tail a crash can
//     leave in the active segment, and refuses real damage (ErrCorrupt).
//
// See ARCHITECTURE.md sections 3 and 6 for the rules.
package logfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// MaxBatchBytes caps how many bytes one write + fsync covers. It matters for
// recovery: only the batch in flight can be half-saved after a crash, so a
// damaged region at the end of the active segment bigger than this cannot
// come from a crash. It equals one maximum-size record, so a single record
// always fits.
const MaxBatchBytes = record.PrefixSize + record.MaxBodySize

const (
	// DefaultSegmentSize is used when Options.SegmentSize is 0. Redaction
	// rewrites a whole segment, so this also bounds the cost of one erase.
	DefaultSegmentSize = 64 << 20

	// MinSegmentSize guarantees that any batch fits in a fresh segment, so
	// the committer never has to split a batch or overfill a segment.
	MinSegmentSize = MaxBatchBytes

	// MaxSegmentSize keeps every offset inside a segment within a uint32,
	// which halves the offset table's memory (see pos).
	MaxSegmentSize = 1 << 30
)

var (
	ErrCorrupt       = errors.New("logfile: corrupt")
	ErrClosed        = errors.New("logfile: closed")
	ErrBroken        = errors.New("logfile: a write failed; reopen the log")
	ErrNotFound      = errors.New("logfile: no such record")
	ErrBatchTooLarge = errors.New("logfile: batch too large")
	ErrEmptyBatch    = errors.New("logfile: empty batch")
	ErrBadOptions    = errors.New("logfile: bad options")
)

// Options configures a log. The zero value is ready to use.
type Options struct {
	// SegmentSize is how large the active segment may grow before it is
	// sealed and a new one is started. 0 means DefaultSegmentSize.
	SegmentSize int64
}

func (o Options) segmentSize() (int64, error) {
	switch s := o.SegmentSize; {
	case s == 0:
		return DefaultSegmentSize, nil
	case s < MinSegmentSize:
		return 0, fmt.Errorf("%w: SegmentSize %d is smaller than the largest batch (%d)", ErrBadOptions, s, MinSegmentSize)
	case s > MaxSegmentSize:
		return 0, fmt.Errorf("%w: SegmentSize %d is above the maximum %d", ErrBadOptions, s, MaxSegmentSize)
	default:
		return s, nil
	}
}

// Entry is one record to append: what kind it is, its structural section
// (kept forever) and the caller's data (removable by redaction).
type Entry struct {
	Type      record.Type
	Structure []byte
	Data      []byte
}

// Report says what Open found, so a recovery is never silent.
type Report struct {
	Records      int   // complete records replayed
	DroppedBytes int64 // torn tail cut off; 0 if the log ended cleanly

	// ResumedRollover means a crash happened after the last segment was
	// sealed but before the next one was created; Open created it.
	ResumedRollover bool

	// FinishedErase counts records whose content was erased while opening,
	// to finish an erase that a crash interrupted (set by the ledger).
	FinishedErase int
}

// pos is where a record lives: segment number (1-based, as in the file
// name) and byte offset inside that segment. Two uint32s take 8 bytes, the
// same as one int64, which is what made the per-segment size cap worth it.
type pos struct {
	seg uint32
	off uint32
}

// Log is one open log: a directory of segments.
type Log struct {
	dir     string
	segSize int64

	reqs chan *request // callers -> committer
	quit chan struct{} // closed by Close: stop taking requests
	done chan struct{} // closed by the committer when it has exited
	once sync.Once

	// mu guards segs and offsets, which readers use while the committer
	// appends to them. Every segment file stays open, so Read is one pread.
	mu      sync.RWMutex
	segs    []*os.File // segs[n-1] = segment n; the last one is active
	offsets []pos      // offsets[id-1] = where record id lives

	// Owned by the committer goroutine only (and by Open before it starts),
	// so they need no lock.
	size        int64    // end of the active segment: where the next batch goes
	nextID      uint64   // id the next record gets
	lastHash    [32]byte // chain hash of the newest record; the next prevHash
	activeFirst uint64   // id of the active segment's first record (for its seal)
	broken      error    // set after a failed write, fsync or rollover

	syncs atomic.Int64 // fsyncs done, so tests can see batching happen
}

// request is one AppendBatch (or Redact) call travelling to the committer.
type request struct {
	recs   []*record.Record
	size   int         // encoded bytes
	redact []uint64    // set for a Redact request instead of recs (redact.go)
	reply  chan result // buffered, so the committer never blocks on it
}

type result struct {
	first uint64 // id of the first record; the rest follow in order
	err   error
}

// Open opens (or creates) the log in dir and replays it, calling fn for each
// record in id order. fn is how the layer above rebuilds its state without
// this package holding every record in memory. fn may be nil.
func Open(dir string, opts Options, fn func(*record.Record) error) (*Log, Report, error) {
	segSize, err := opts.segmentSize()
	if err != nil {
		return nil, Report{}, err
	}
	dir = filepath.Clean(dir)
	if err := makeDir(dir); err != nil {
		return nil, Report{}, err
	}
	n, seals, err := countSegments(dir)
	if err != nil {
		return nil, Report{}, err
	}
	// Seals are written before the next segment is created, so a seal can
	// belong to the last segment (a crash mid-rollover) but never to a
	// segment that does not exist.
	lastSealed := false
	for _, s := range seals {
		if s > n {
			return nil, Report{}, fmt.Errorf("%w: seal %d has no segment", ErrCorrupt, s)
		}
		lastSealed = lastSealed || s == n
	}

	l := &Log{
		dir:     dir,
		segSize: segSize,
		reqs:    make(chan *request),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if err := l.openSegments(n); err != nil {
		l.closeFiles()
		return nil, Report{}, err
	}
	rep, err := l.replay(fn, lastSealed)
	if err == nil && lastSealed {
		// Finish the interrupted rollover: the sealed segment is complete,
		// so start the next one.
		err = l.startSegment()
		rep.ResumedRollover = true
	}
	if err != nil {
		l.closeFiles()
		return nil, rep, err
	}
	go l.commitLoop()
	return l, rep, nil
}

// makeDir creates the log directory if it does not exist yet.
func makeDir(dir string) error {
	err := os.Mkdir(dir, 0o700)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// The directory's name lives in its parent. Without syncing the parent,
	// a crash could lose the directory and every segment in it.
	return syncDir(filepath.Dir(dir))
}

func segmentName(n int) string { return fmt.Sprintf("%06d.seg", n) }

// fileNumber parses a segment (".seg") or seal (".seal") file name. Other
// files are ignored (seal writing and redaction leave temporary
// files).
func fileNumber(name, ext string) (int, bool) {
	stem, ok := strings.CutSuffix(name, ext)
	if !ok || len(stem) < 6 || strings.Trim(stem, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(stem)
	return n, err == nil
}

// countSegments returns how many segments dir holds, and the numbers of the
// seal files it found. Segments must be numbered
// 1..N with no gaps: a missing number means a lost file, and the records in
// it are gone, so that is corruption, not something to skip over.
//
// Why the log must start at segment 1: records are never removed (redaction
// keeps every header and structural section), so no segment ever becomes
// empty enough to delete. A log that starts at, say, segment 4 has lost its
// first files.
//
// If v3 deletes old segments after a snapshot (log truncation, like
// PostgreSQL removing WAL files after a checkpoint), this check must change:
//   - numbering may start above 1, but must still be consecutive;
//   - a deliberate cleanup must be provable, or it looks exactly like lost
//     files. The snapshot (or a small marker written before deleting) must
//     say where the log now starts: first segment, first id, and the chain
//     hash before it. Replay then checks the first segment against that.
func countSegments(dir string) (int, []int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil, err
	}
	var nums, seals []int
	for _, e := range entries {
		// A leftover temporary file is a seal or segment rewrite that a
		// crash interrupted before its rename, so the original file is
		// still intact and the temporary one can go.
		if strings.HasSuffix(e.Name(), ".tmp") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return 0, nil, err
			}
			continue
		}
		if n, ok := fileNumber(e.Name(), ".seg"); ok {
			nums = append(nums, n)
		}
		if n, ok := fileNumber(e.Name(), ".seal"); ok {
			seals = append(seals, n)
		}
	}
	slices.Sort(nums)
	// Check the numbers are exactly 1, 2, 3, ... with nothing missing.
	// i is the position (0, 1, 2, ...), n the segment number found there,
	// so position i must hold segment i+1:
	//   [1 2 3] passes;
	//   [1 3]   fails at i=1: segment 2 is missing;
	//   [4 5 6] fails at i=0: segments 1-3 are missing.
	//
	// UPDATE THIS IF CLEANUP IS ADDED (v3): today the log must start at
	// segment 1, because segments are never deleted. If old segments are
	// ever cleaned up, [4 5 6] becomes a valid log and this loop would wrongly
	// call it corrupt. It must then compare against the first segment
	// recorded by the snapshot/cleanup marker instead of 1 (see the comment
	// above this function).
	for i, n := range nums {
		if n != i+1 {
			return 0, nil, fmt.Errorf("%w: segments must be numbered 1 to %d with no gaps, found %v", ErrCorrupt, len(nums), nums)
		}
	}
	return len(nums), seals, nil
}

// openSegments opens segments 1..n, or creates segment 1 for a new log.
// Sealed segments are opened read-only, so no bug in the committer can ever
// append to them. Only the active one is writable.
func (l *Log) openSegments(n int) error {
	if n == 0 {
		f, err := createSegment(l.dir, 1)
		if err != nil {
			return err
		}
		l.segs = []*os.File{f}
		return nil
	}
	for i := 1; i <= n; i++ {
		flag := os.O_RDONLY
		if i == n {
			// O_APPEND makes the OS put every write at the current end of
			// the file, so we never compute write positions ourselves.
			flag = os.O_RDWR | os.O_APPEND
		}
		f, err := os.OpenFile(filepath.Join(l.dir, segmentName(i)), flag, 0)
		if err != nil {
			return err
		}
		l.segs = append(l.segs, f)
	}
	return nil
}

// createSegment creates segment n, failing if it already exists.
func createSegment(dir string, n int) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, segmentName(n)), os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	// A new file's name lives in its directory. Without syncing the
	// directory, a crash could lose the file itself, records and all.
	if err := syncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// replay reads every segment in order, checks ids run 1, 2, 3, ... and the
// hash chain links every record to the one before, across all segments,
// builds the offset table and hands each record to fn. lastSealed says the
// last segment already has a seal (a crash mid-rollover).
func (l *Log) replay(fn func(*record.Record) error, lastSealed bool) (Report, error) {
	var rep Report
	for i := range l.segs {
		n := i + 1
		sealed := n < len(l.segs) || lastSealed
		if err := l.replaySegment(n, sealed, fn, &rep); err != nil {
			return rep, err
		}
	}
	l.nextID = uint64(len(l.offsets)) + 1
	return rep, nil
}

// replaySegment reads segment n from start to end, one record at a time.
// For each good record it checks the id is the next one expected and that
// its prevHash matches the chain so far, passes the record to fn (so the
// layer above can rebuild its state) and notes where the record lives in
// the offset table. A bad record ends the read: in the active segment it
// may be a crash's torn tail, so it is cut off; in a sealed segment it is
// real damage, so Open fails with ErrCorrupt. A sealed segment must also
// match its seal.
func (l *Log) replaySegment(n int, sealed bool, fn func(*record.Record) error, rep *Report) error {
	f := l.segs[n-1]
	first := uint64(len(l.offsets)) + 1
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end := info.Size()
	if end > math.MaxUint32 {
		return fmt.Errorf("%w: segment %d is %d bytes, more than a segment can hold", ErrCorrupt, n, end)
	}
	// A segment is only sealed when it holds records. (An empty ACTIVE
	// segment is fine: a crash right after a rollover.)
	if end == 0 && sealed {
		return fmt.Errorf("%w: sealed segment %d is empty", ErrCorrupt, n)
	}

	// Read through a 64 KiB buffer: one system call per buffer instead of
	// two per record.
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, end), 64<<10)
	var buf []byte
	var off int64
	for off < end {
		var rec *record.Record
		rec, buf, err = readRecord(r, buf)
		if err != nil {
			if !isDamage(err) {
				return err // the OS failed to read: never cut data because of that
			}
			if sealed {
				// Every batch in a sealed segment was fsynced before it was
				// sealed, so no crash can have left it half-written. Damage
				// here is real.
				return fmt.Errorf("%w: sealed segment %d: bad record at offset %d: %v", ErrCorrupt, n, off, err)
			}
			rep.DroppedBytes, err = cutTail(f, n, off, end, err)
			if err != nil {
				return err
			}
			break
		}

		// A crash cannot produce a valid record with the wrong id, because
		// only the committer assigns ids, in order. So this is damage or a
		// bug, wherever it is. The count runs on across segments, so the
		// same check also catches ids that do not continue from one segment
		// into the next.
		if want := uint64(len(l.offsets)) + 1; rec.ID != want {
			if off == 0 && n > 1 {
				return fmt.Errorf("%w: segment %d starts with id %d, but segment %d ended with id %d",
					ErrCorrupt, n, rec.ID, n-1, want-1)
			}
			return fmt.Errorf("%w: segment %d offset %d has id %d, want %d", ErrCorrupt, n, off, rec.ID, want)
		}
		// The hash chain: each record carries the chain hash of the
		// one before it. Editing, removing, inserting or reordering records
		// breaks this link at the first record after the change, even if the
		// crc was recomputed. A crash cannot produce a valid record with a
		// wrong link, so this is ErrCorrupt, never a torn tail.
		if rec.PrevHash != l.lastHash {
			return fmt.Errorf("%w: record %d (segment %d offset %d) does not link to record %d: the log was edited",
				ErrCorrupt, rec.ID, n, off, rec.ID-1)
		}
		l.lastHash = rec.ChainHash()
		if fn != nil {
			if err := fn(rec); err != nil {
				return err
			}
		}
		l.offsets = append(l.offsets, pos{seg: uint32(n), off: uint32(off)})
		off += int64(len(buf))
		rep.Records++
	}
	if sealed {
		return checkSeal(l.dir, n, seal{first: first, last: uint64(len(l.offsets)), hash: l.lastHash})
	}
	l.size, l.activeFirst = off, first
	return nil
}

// readRecord reads the next record into buf (reused between calls to avoid
// one allocation per record) and returns the bytes it used.
func readRecord(r io.Reader, buf []byte) (*record.Record, []byte, error) {
	buf = slices.Grow(buf[:0], record.PrefixSize)[:record.PrefixSize]
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, buf, endedEarly(err)
	}
	n, err := record.BodyLen(buf)
	if err != nil {
		return nil, buf, err
	}
	buf = slices.Grow(buf, n)[:record.PrefixSize+n]
	if _, err := io.ReadFull(r, buf[record.PrefixSize:]); err != nil {
		return nil, buf, endedEarly(err)
	}
	rec, _, err := record.Decode(buf)
	return rec, buf, err
}

// endedEarly turns "the file ran out of bytes" into record.ErrTruncated, so
// a short file counts as damage while real I/O errors do not.
func endedEarly(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return record.ErrTruncated
	}
	return err
}

// isDamage reports whether err means "these bytes are not a valid record",
// as opposed to "the disk or OS failed".
func isDamage(err error) bool {
	for _, e := range []error{record.ErrTruncated, record.ErrBadLength, record.ErrChecksum, record.ErrDigest, record.ErrInvalid} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// cutTail handles a bad record at off in the active segment f. If
// everything from there to the end fits in one batch, it can be the
// unfinished last batch of a crash: none of it was acknowledged, so it is cut
// off. Anything bigger cannot come from a crash, so the log is reported as
// corrupt and left untouched rather than losing acknowledged records.
func cutTail(f *os.File, n int, off, end int64, cause error) (int64, error) {
	tail := end - off
	if tail > MaxBatchBytes {
		return 0, fmt.Errorf("%w: segment %d: bad record at offset %d (%v) with %d bytes after it",
			ErrCorrupt, n, off, cause, tail)
	}
	if err := f.Truncate(off); err != nil {
		return 0, err
	}
	// Make the cut durable, or a crash could bring the bad bytes back.
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return tail, nil
}

// Append adds one record and returns its id once it is safely on disk.
func (l *Log) Append(t record.Type, data []byte) (uint64, error) {
	return l.AppendBatch([]Entry{{Type: t, Data: data}})
}

// AppendBatch adds several records that are written together, and returns
// the id of the first one (the others follow in order). It returns only
// after the fsync covering them has returned. Safe to call from many
// goroutines at once.
func (l *Log) AppendBatch(entries []Entry) (uint64, error) {
	if len(entries) == 0 {
		return 0, ErrEmptyBatch
	}

	// Salt, digest and validation happen here, in the caller's goroutine,
	// so that work runs in parallel. Only the id, which must be assigned in
	// order, is left to the committer.
	req := &request{reply: make(chan result, 1)}
	for _, e := range entries {
		r, err := record.New(e.Type, 0, [32]byte{}, e.Structure, e.Data)
		if err != nil {
			return 0, err
		}
		req.recs = append(req.recs, r)
		req.size += r.Size()
	}
	if req.size > MaxBatchBytes {
		return 0, fmt.Errorf("%w: %d bytes, max %d", ErrBatchTooLarge, req.size, MaxBatchBytes)
	}

	select {
	case l.reqs <- req:
	case <-l.quit:
		return 0, ErrClosed
	}
	res := <-req.reply
	return res.first, res.err
}

// commitLoop is the committer goroutine: the only code that writes segments.
func (l *Log) commitLoop() {
	defer close(l.done)
	var carry *request // a request that did not fit in the previous batch
	for {
		req := carry
		carry = nil
		if req == nil {
			select {
			case req = <-l.reqs:
			case <-l.quit:
				return
			}
		}
		// A redaction rewrites segment files; it runs on its own, never
		// mixed into a batch of appends.
		if req.redact != nil {
			req.reply <- result{err: l.redactNow(req.redact)}
			continue
		}

		// Gather whatever else is already waiting. There is no timer: under
		// light load a batch is one request and adds no delay; under heavy
		// load, requests pile up during the previous fsync and share the next.
		batch := []*request{req}
		size := req.size
	gather:
		for {
			select {
			case r := <-l.reqs:
				if r.redact != nil || size+r.size > MaxBatchBytes {
					carry = r
					break gather
				}
				batch = append(batch, r)
				size += r.size
			default:
				break gather
			}
		}
		l.commit(batch, size)
	}
}

// commit assigns ids, writes the batch with one write and one fsync, and
// answers every caller in it.
func (l *Log) commit(batch []*request, size int) {
	if l.broken != nil {
		replyAll(batch, l.broken)
		return
	}

	// A batch never spans two segments: if it does not fit in what is left
	// of the active one, start a new segment first. A fresh segment always
	// has room, because SegmentSize >= MaxBatchBytes >= size.
	if l.size > 0 && l.size+int64(size) > l.segSize {
		if err := l.roll(); err != nil {
			// Nothing of this batch was written, but a half-finished
			// rollover may have left the new file behind, and a retry would
			// trip over it. Same rule as any failed write: stop, and let a
			// reopen find out what is really on disk.
			l.broken = fmt.Errorf("%w: starting a new segment: %v", ErrBroken, err)
			replyAll(batch, l.broken)
			return
		}
	}
	// Only the committer changes segs, so it can read it without the lock.
	seg := uint32(len(l.segs))
	active := l.segs[seg-1]

	buf := make([]byte, 0, size)
	var offs []pos
	firsts := make([]uint64, len(batch))
	id := l.nextID
	hash := l.lastHash
	for i, req := range batch {
		firsts[i] = id
		for _, r := range req.recs {
			// Link the record into the chain: it carries the previous
			// record's hash, and its own hash (which covers its id and that
			// link) becomes the next record's prevHash.
			r.ID = id
			r.PrevHash = hash
			hash = r.ChainHash()
			id++
			enc, err := record.Encode(r)
			if err != nil {
				// Should not happen: New already validated the record.
				// Fail the whole batch before writing, so no id is skipped.
				replyAll(batch, err)
				return
			}
			offs = append(offs, pos{seg: seg, off: uint32(l.size + int64(len(buf)))})
			buf = append(buf, enc...)
		}
	}

	_, err := active.Write(buf)
	if err == nil {
		err = active.Sync()
		l.syncs.Add(1)
	}
	if err != nil {
		// After a failed write or fsync we cannot know what reached the
		// disk, and calling fsync again is no fix: the OS may already have
		// dropped the unsaved data and report success the second time
		// (PostgreSQL's "fsyncgate", 2018). So the log stops taking writes;
		// reopening replays the segments and finds out what really survived.
		l.broken = fmt.Errorf("%w: %v", ErrBroken, err)
		replyAll(batch, l.broken)
		return
	}

	l.size += int64(len(buf))
	l.nextID = id
	l.lastHash = hash
	l.mu.Lock()
	l.offsets = append(l.offsets, offs...)
	l.mu.Unlock()
	for i, req := range batch {
		req.reply <- result{first: firsts[i]}
	}
}

// roll seals the active segment and starts the next one.
//
// Order matters for crashes. The seal is written (atomically) BEFORE the
// next segment is created:
//   - crash before the seal: the old segment is still the active one;
//   - crash after the seal, before the next segment: Open sees a sealed
//     last segment and finishes the rollover (Report.ResumedRollover);
//   - crash after both: a normal sealed segment and an empty active one.
//
// The segment's records need no extra fsync: every batch was already
// fsynced before its callers were answered.
func (l *Log) roll() error {
	n := len(l.segs)
	s := seal{first: l.activeFirst, last: l.nextID - 1, hash: l.lastHash}
	if err := writeSeal(l.dir, n, s); err != nil {
		return err
	}
	return l.startSegment()
}

// startSegment creates the next segment and makes it the active one.
func (l *Log) startSegment() error {
	f, err := createSegment(l.dir, len(l.segs)+1)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.segs = append(l.segs, f)
	l.mu.Unlock()
	l.size, l.activeFirst = 0, l.nextID
	return nil
}

func replyAll(batch []*request, err error) {
	for _, req := range batch {
		req.reply <- result{err: err}
	}
}

// Read returns record id, read from disk. Its crc is checked again, so
// damage that happened after Open is still caught.
func (l *Log) Read(id uint64) (*record.Record, error) {
	select {
	case <-l.quit:
		return nil, ErrClosed
	default:
	}

	// The read lock is held through the reads: a redaction swaps a segment
	// file and moves offsets under the write lock, so a read never uses a
	// file that is being replaced or an offset that just moved. Reads are
	// one pread each, so the committer waits at most that long.
	l.mu.RLock()
	defer l.mu.RUnlock()
	if id == 0 || id > uint64(len(l.offsets)) {
		return nil, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	p := l.offsets[id-1]
	f := l.segs[p.seg-1]
	off := int64(p.off)

	// ReadAt reads at a given offset without moving the file position, so
	// reads never disturb the committer's appends.
	prefix := make([]byte, record.PrefixSize)
	if _, err := f.ReadAt(prefix, off); err != nil {
		return nil, err
	}
	n, err := record.BodyLen(prefix)
	if err != nil {
		return nil, fmt.Errorf("%w: record %d (segment %d): %v", ErrCorrupt, id, p.seg, err)
	}
	buf := make([]byte, record.PrefixSize+n)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	r, _, err := record.Decode(buf)
	if err != nil {
		return nil, fmt.Errorf("%w: record %d (segment %d): %v", ErrCorrupt, id, p.seg, err)
	}
	if r.ID != id {
		return nil, fmt.Errorf("%w: offset for id %d holds id %d", ErrCorrupt, id, r.ID)
	}
	return r, nil
}

// Verify re-reads every record from disk and runs every check again: crc,
// content digest, ids, the hash chain, and each sealed segment against its
// seal. Open already did all of this; Verify catches files changed since.
// It checks the records that existed when it started, so appends running
// at the same time are fine.
//
// What it cannot catch, from the files alone: the whole chain rebuilt after
// an edit, or the newest records removed or edited (nothing after them
// points back to them). That is why it returns the head: the chain hash of
// the newest record it checked (zeros for an empty log). Every one of those
// changes alters the head, so a copy of the head kept elsewhere catches them.
// See ARCHITECTURE.md section 4.
func (l *Log) Verify() ([32]byte, error) {
	var head [32]byte
	if err := l.verify(&head); err != nil {
		return [32]byte{}, err
	}
	return head, nil
}

func (l *Log) verify(head *[32]byte) error {
	select {
	case <-l.quit:
		return ErrClosed
	default:
	}
	l.mu.RLock()
	nsegs := len(l.segs)
	total := uint64(len(l.offsets))
	l.mu.RUnlock()

	var id uint64
	var hash [32]byte
	var buf []byte
	for i := range nsegs {
		n := i + 1
		sealed := n < nsegs
		first := id + 1
		// Open each segment afresh by name: Verify checks what is on disk
		// now, and a redaction may swap a segment file while Verify runs.
		// An open file keeps reading the same version even if it is
		// replaced meanwhile, so each segment is checked consistently.
		f, err := os.Open(filepath.Join(l.dir, segmentName(n)))
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		end := info.Size()
		r := bufio.NewReaderSize(io.NewSectionReader(f, 0, end), 64<<10)
		var off int64
		for off < end && id < total {
			var rec *record.Record
			rec, buf, err = readRecord(r, buf)
			if err != nil {
				if !isDamage(err) {
					return err
				}
				return fmt.Errorf("%w: segment %d offset %d: %v", ErrCorrupt, n, off, err)
			}
			id++
			if rec.ID != id {
				return fmt.Errorf("%w: segment %d offset %d has id %d, want %d", ErrCorrupt, n, off, rec.ID, id)
			}
			if rec.PrevHash != hash {
				return fmt.Errorf("%w: record %d does not link to record %d: the log was edited", ErrCorrupt, id, id-1)
			}
			hash = rec.ChainHash()
			off += int64(len(buf))
		}
		if sealed {
			if off != end {
				return fmt.Errorf("%w: sealed segment %d has %d unexpected bytes at its end", ErrCorrupt, n, end-off)
			}
			if err := checkSeal(l.dir, n, seal{first: first, last: id, hash: hash}); err != nil {
				return err
			}
		}
	}
	if id != total {
		return fmt.Errorf("%w: found %d records, the log held %d", ErrCorrupt, id, total)
	}
	*head = hash
	return nil
}

// Close stops the committer and closes every segment. Every acknowledged
// record is already fsynced, so there is nothing left to flush.
func (l *Log) Close() error {
	var err error
	l.once.Do(func() {
		close(l.quit)
		<-l.done // let a batch in progress finish
		err = l.closeFiles()
	})
	return err
}

func (l *Log) closeFiles() error {
	var errs []error
	for _, f := range l.segs {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}
