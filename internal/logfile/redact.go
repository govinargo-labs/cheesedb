package logfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// Redaction: erase the content (salt + data) of some records,
// keeping their header and structural section. The record keeps its id and
// its place in the log, and the hash chain is unchanged, because chain
// hashes never covered content. See ARCHITECTURE.md section 7.
//
// A record cannot be shortened in place, so each segment holding a record
// to erase is rewritten as a whole:
//
//	1. copy the segment to NNNNNN.seg.tmp, without those records' content
//	2. fsync the copy
//	3. rename it over the segment (atomic: a crash leaves old or new, never half)
//	4. fsync the folder, so the rename itself survives a crash
//	5. swap the open file and update the offsets of records that moved
//
// A crash before step 3 leaves a .tmp file, which Open removes; the
// original segment is untouched. Seals stay valid: the first id, last id
// and last chain hash do not change.

// Redact erases the content of the given records and returns once the
// rewritten segments are on disk. Records that are already redacted, and
// duplicate ids, are fine. It runs in the committer, so it never overlaps
// with appends.
func (l *Log) Redact(ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	req := &request{redact: slices.Clone(ids), reply: make(chan result, 1)}
	select {
	case l.reqs <- req:
	case <-l.quit:
		return ErrClosed
	}
	return (<-req.reply).err
}

// redactNow runs in the committer goroutine.
func (l *Log) redactNow(ids []uint64) error {
	if l.broken != nil {
		return l.broken
	}
	// Group the ids by segment. Only the committer changes offsets, so it
	// can read them without the lock.
	bySeg := map[uint32]map[uint64]bool{}
	for _, id := range ids {
		if id == 0 || id > uint64(len(l.offsets)) {
			return fmt.Errorf("%w: id %d", ErrNotFound, id)
		}
		seg := l.offsets[id-1].seg
		if bySeg[seg] == nil {
			bySeg[seg] = map[uint64]bool{}
		}
		bySeg[seg][id] = true
	}
	segs := make([]uint32, 0, len(bySeg))
	for seg := range bySeg {
		segs = append(segs, seg)
	}
	slices.Sort(segs) // deterministic order
	for _, seg := range segs {
		if err := l.rewriteSegment(int(seg), bySeg[seg]); err != nil {
			return err
		}
	}
	return nil
}

// rewriteSegment rewrites segment n without the content of the records in
// erase. If nothing in the segment needs erasing (all already redacted),
// the file is left alone.
func (l *Log) rewriteSegment(n int, erase map[uint64]bool) error {
	old := l.segs[n-1]
	active := n == len(l.segs)
	end := l.size // the active segment ends where the committer writes next
	if !active {
		info, err := old.Stat()
		if err != nil {
			return err
		}
		end = info.Size()
	}

	path := filepath.Join(l.dir, segmentName(n))
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// Until the rename succeeds, any failure leaves the segment untouched:
	// remove the copy and report the error. The log stays usable.
	abandon := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}

	r := bufio.NewReaderSize(io.NewSectionReader(old, 0, end), 64<<10)
	w := bufio.NewWriterSize(f, 256<<10)
	var newOffs []pos // new offsets of this segment's records, in order
	var off, written int64
	var buf []byte
	changed := false
	for off < end {
		var rec *record.Record
		rec, buf, err = readRecord(r, buf)
		if err != nil {
			if !isDamage(err) {
				return abandon(err)
			}
			return abandon(fmt.Errorf("%w: segment %d offset %d while redacting: %v", ErrCorrupt, n, off, err))
		}
		out := buf // unchanged records are copied byte for byte
		if erase[rec.ID] && !rec.Redacted() {
			rec.Content = nil // keep header and structure; drop salt + data
			if out, err = record.Encode(rec); err != nil {
				return abandon(err)
			}
			changed = true
		}
		if _, err := w.Write(out); err != nil {
			return abandon(err)
		}
		newOffs = append(newOffs, pos{seg: uint32(n), off: uint32(written)})
		off += int64(len(buf))
		written += int64(len(out))
	}
	if !changed {
		return abandon(nil)
	}
	if err := w.Flush(); err != nil {
		return abandon(err)
	}
	if err := f.Sync(); err != nil {
		return abandon(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}

	// The new segment is in place. From here a failure leaves the files
	// correct but this Log's view of them stale, so the log stops taking
	// writes until reopened (as after any failed write).
	fail := func(err error) error {
		l.broken = fmt.Errorf("%w: after redacting segment %d: %v", ErrBroken, n, err)
		return l.broken
	}
	if err := syncDir(l.dir); err != nil {
		return fail(err)
	}
	flag := os.O_RDONLY
	if active {
		flag = os.O_RDWR | os.O_APPEND
	}
	nf, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return fail(err)
	}

	// Swap under the write lock: readers hold the read lock through their
	// reads, so none is using the old file or the old offsets now.
	first := l.firstID(n)
	l.mu.Lock()
	l.segs[n-1] = nf
	copy(l.offsets[first-1:], newOffs)
	l.mu.Unlock()
	if active {
		l.size = written
	}
	if err := old.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return fail(err)
	}
	return nil
}

// firstID returns the id of segment n's first record. Offsets are in id
// order and grouped by segment, so a binary search finds it.
func (l *Log) firstID(n int) uint64 {
	i, _ := slices.BinarySearchFunc(l.offsets, uint32(n), func(p pos, seg uint32) int {
		return int(p.seg) - int(seg)
	})
	return uint64(i) + 1
}
