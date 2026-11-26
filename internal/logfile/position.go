package logfile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// A Position is where the log stood at one moment: how many records it
// held, the chain hash of the last one, where each segment ended, and the
// offset table. The ledger saves one in its snapshot (state.snap), and
// OpenAt starts from it, so the records it covers are not read again.
// See ARCHITECTURE.md section 3.
type Position struct {
	Records uint64   // records 1..Records are covered
	Hash    [32]byte // chain hash of record Records

	segs    []segEnd // segments 1..k, k being the segment of record Records
	offsets []pos    // offsets[id-1], as in Log
}

// segEnd is one covered segment: how many bytes and records it held, and
// the chain hash of its last record.
type segEnd struct {
	size  int64
	count uint64
	hash  [32]byte
}

// ErrStale means a Position does not match the files: they changed since,
// or the Position belongs to another log. Nothing was changed on disk; open
// the log with Open instead.
var ErrStale = errors.New("logfile: the saved position does not match the log")

// Position returns where the log stands now. It runs in the committer, so
// it never overlaps an append or a redaction. A log that holds no records,
// or whose last write failed, has no position worth saving: it returns nil
// and the error that broke the log, if any.
func (l *Log) Position() (*Position, error) {
	req := &request{position: true, reply: make(chan result, 1)}
	select {
	case l.reqs <- req:
	case <-l.quit:
		return nil, ErrClosed
	}
	res := <-req.reply
	return res.pos, res.err
}

// positionNow runs in the committer goroutine, which owns size and lastHash.
func (l *Log) positionNow() (*Position, error) {
	if l.broken != nil {
		return nil, l.broken
	}
	if len(l.offsets) == 0 {
		return nil, nil
	}
	p := &Position{Records: uint64(len(l.offsets)), Hash: l.lastHash}
	p.offsets = append([]pos(nil), l.offsets...)
	k := int(l.offsets[len(l.offsets)-1].seg)
	for n := 1; n <= k; n++ {
		s := segEnd{size: l.size, hash: l.lastHash}
		if n < len(l.segs) {
			// A sealed segment: it ended at its file size, and its seal
			// holds the chain hash of its last record.
			info, err := l.segs[n-1].Stat()
			if err != nil {
				return nil, err
			}
			seal, err := readSeal(l.dir, n)
			if err != nil {
				return nil, err
			}
			s.size, s.hash = info.Size(), seal.hash
		}
		p.segs = append(p.segs, s)
	}
	for _, o := range p.offsets {
		p.segs[o.seg-1].count++
	}
	return p, nil
}

// Position layout (every integer a uvarint unless a size is given):
//
//	records
//	hash               32 bytes, chain hash of the last record
//	k                  covered segments
//	per segment:
//	  size             bytes in it
//	  count            records in it (at least 1)
//	  hash             32 bytes, chain hash of its last record
//	  count-1 gaps     offset of each record after the first, minus the
//	                   offset before it; the first record is at 0
//
// The record after the last one covered starts in segment k at byte
// "size of segment k". Gaps are record sizes, so they take 2 or 3 bytes
// each instead of the 8 of the offset table.

// AppendEncoded appends p's encoding to b.
func (p *Position) AppendEncoded(b []byte) []byte {
	b = binary.AppendUvarint(b, p.Records)
	b = append(b, p.Hash[:]...)
	b = binary.AppendUvarint(b, uint64(len(p.segs)))
	i := 0
	for _, s := range p.segs {
		b = binary.AppendUvarint(b, uint64(s.size))
		b = binary.AppendUvarint(b, s.count)
		b = append(b, s.hash[:]...)
		for j := uint64(1); j < s.count; j++ {
			b = binary.AppendUvarint(b, uint64(p.offsets[i+int(j)].off-p.offsets[i+int(j)-1].off))
		}
		i += int(s.count)
	}
	return b
}

// DecodePosition reads a Position from the start of b and returns it with
// the number of bytes it used. It checks the numbers fit together (counts
// add up, offsets rise and stay inside their segment), not that they match
// any files: OpenAt does that.
func DecodePosition(b []byte) (*Position, int, error) {
	d := decoder{b: b}
	p := &Position{Records: d.uvarint()}
	copy(p.Hash[:], d.bytes(32))
	k := d.uvarint()
	// Every segment takes at least 34 bytes, so a k this large cannot be
	// real; checking first keeps a damaged file from asking for a huge slice.
	if k == 0 || k > uint64(len(b))/34 {
		return nil, 0, fmt.Errorf("%w: position covers %d segments", ErrStale, k)
	}
	if p.Records == 0 || p.Records > uint64(len(b)) {
		return nil, 0, fmt.Errorf("%w: position covers %d records", ErrStale, p.Records)
	}
	p.segs = make([]segEnd, k)
	p.offsets = make([]pos, 0, p.Records)
	for n := range p.segs {
		s := &p.segs[n]
		s.size = int64(d.uvarint())
		s.count = d.uvarint()
		copy(s.hash[:], d.bytes(32))
		if d.err != nil {
			break
		}
		if s.size <= 0 || s.size > MaxSegmentSize || s.count == 0 || s.count > p.Records-uint64(len(p.offsets)) {
			return nil, 0, fmt.Errorf("%w: segment %d: %d records in %d bytes", ErrStale, n+1, s.count, s.size)
		}
		var off uint64
		p.offsets = append(p.offsets, pos{seg: uint32(n + 1)})
		for j := uint64(1); j < s.count; j++ {
			gap := d.uvarint()
			off += gap
			if gap < record.PrefixSize+record.HeaderSize || off >= uint64(s.size) {
				return nil, 0, fmt.Errorf("%w: segment %d: record %d of %d at offset %d", ErrStale, n+1, j+1, s.count, off)
			}
			p.offsets = append(p.offsets, pos{seg: uint32(n + 1), off: uint32(off)})
		}
		if d.err != nil {
			break
		}
	}
	if d.err != nil {
		return nil, 0, fmt.Errorf("%w: position: %v", ErrStale, d.err)
	}
	if uint64(len(p.offsets)) != p.Records {
		return nil, 0, fmt.Errorf("%w: segments hold %d records, position says %d", ErrStale, len(p.offsets), p.Records)
	}
	if p.segs[k-1].hash != p.Hash {
		return nil, 0, fmt.Errorf("%w: the last segment's hash is not the position's hash", ErrStale)
	}
	return p, d.n, nil
}

// decoder reads uvarints and fixed-size fields, remembering the first
// error so callers check once.
type decoder struct {
	b   []byte
	n   int
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, k := binary.Uvarint(d.b[d.n:])
	if k <= 0 {
		d.err = errors.New("bad or missing number")
		return 0
	}
	d.n += k
	return v
}

func (d *decoder) bytes(k int) []byte {
	if d.err != nil || len(d.b)-d.n < k {
		d.err = errors.New("ended early")
		return make([]byte, k)
	}
	d.n += k
	return d.b[d.n-k : d.n]
}

// resume checks p against the open segment files and, if they match,
// takes p's offset table and chain hash as if the covered records had
// been replayed. For each covered segment it reads and checks two records
// (crc, digest, id, hash), the first and the last, which costs two reads
// per segment however many records it holds:
//   - every covered segment but the last has exactly the size p says, and
//     the last one at least that size (it may have grown since);
//   - its first record links to the previous segment's last hash;
//   - its last record has p's chain hash and ends exactly where p says;
//   - each one but the last matches its seal.
//
// A redaction always shrinks the segment it rewrites, so a snapshot taken
// before one fails the size check. Records in the middle of a segment are
// not read: Verify does that.
func (l *Log) resume(p *Position) error {
	k := len(p.segs)
	if k > len(l.segs) {
		return fmt.Errorf("%w: it covers %d segments, the log has %d", ErrStale, k, len(l.segs))
	}
	var id uint64
	var prev [32]byte
	for n := 1; n <= k; n++ {
		s := p.segs[n-1]
		f := l.segs[n-1]
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if size := info.Size(); size < s.size || (n < k && size != s.size) {
			return fmt.Errorf("%w: segment %d is %d bytes, the position says %d", ErrStale, n, size, s.size)
		}
		first, last := id+1, id+s.count
		r, _, err := readAt(f, int64(p.offsets[first-1].off))
		if err != nil {
			return fmt.Errorf("%w: segment %d, record %d: %v", ErrStale, n, first, err)
		}
		if r.ID != first || r.PrevHash != prev {
			return fmt.Errorf("%w: segment %d does not start with record %d as saved", ErrStale, n, first)
		}
		off := int64(p.offsets[last-1].off)
		r, size, err := readAt(f, off)
		if err != nil {
			return fmt.Errorf("%w: segment %d, record %d: %v", ErrStale, n, last, err)
		}
		if r.ID != last || r.ChainHash() != s.hash || off+int64(size) != s.size {
			return fmt.Errorf("%w: segment %d does not end with record %d as saved", ErrStale, n, last)
		}
		if n < k {
			if err := checkSeal(l.dir, n, seal{first: first, last: last, hash: s.hash}); err != nil {
				return fmt.Errorf("%w: %v", ErrStale, err)
			}
		}
		id, prev = last, s.hash
	}
	l.offsets = p.offsets
	l.lastHash = p.Hash
	return nil
}

// readAt reads and checks (crc, digest) the record at off in f, and
// returns it with its size in bytes.
func readAt(f *os.File, off int64) (*record.Record, int, error) {
	prefix := make([]byte, record.PrefixSize)
	if _, err := f.ReadAt(prefix, off); err != nil {
		return nil, 0, err
	}
	n, err := record.BodyLen(prefix)
	if err != nil {
		return nil, 0, err
	}
	buf := make([]byte, record.PrefixSize+n)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, 0, err
	}
	r, _, err := record.Decode(buf)
	return r, len(buf), err
}
