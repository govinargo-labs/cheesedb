package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// small makes segments as small as Options allows (1 MiB + 8 bytes), so
// tests can fill one with a few big records.
var small = Options{SegmentSize: MinSegmentSize}

// big returns ~400 KB of data. Each record is then 400,174 bytes, so a
// small segment holds two and the third starts a new segment.
func big(tag string) string {
	return tag + strings.Repeat("x", 400_000-len(tag))
}

func segPath(dir string, n int) string { return filepath.Join(dir, segmentName(n)) }

func mustOpen(t *testing.T, dir string, opts Options) (*Log, Report, []*record.Record) {
	t.Helper()
	var recs []*record.Record
	l, rep, err := Open(dir, opts, func(r *record.Record) error {
		recs = append(recs, r)
		return nil
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l, rep, recs
}

func mustAppend(t *testing.T, l *Log, data string) uint64 {
	t.Helper()
	id, err := l.Append(record.TypeLot, []byte(data))
	if err != nil {
		t.Fatalf("Append(%.10q): %v", data, err)
	}
	return id
}

// writeLog creates a log in dir holding the given records, closes it and
// returns where each record starts.
func writeLog(t *testing.T, dir string, opts Options, data ...string) []pos {
	t.Helper()
	l, _, _ := mustOpen(t, dir, opts)
	for _, d := range data {
		mustAppend(t, l, d)
	}
	offs := append([]pos(nil), l.offsets...)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return offs
}

// writeOneSegment is writeLog for tests about a single (active) segment: it
// returns segment 1's bytes and the record offsets in it.
func writeOneSegment(t *testing.T, dir string, data ...string) ([]byte, []int64) {
	t.Helper()
	var offs []int64
	for _, p := range writeLog(t, dir, Options{}, data...) {
		offs = append(offs, int64(p.off))
	}
	return readFile(t, segPath(dir, 1)), offs
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// encode builds the bytes of a valid record with any id and prevHash, so
// tests can forge segments the committer would never write.
func encode(t *testing.T, id uint64, prev [32]byte, data string) []byte {
	t.Helper()
	r, err := record.New(record.TypeLot, id, prev, nil, []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	b, err := record.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decodeAll decodes every record in a segment's bytes.
func decodeAll(t *testing.T, b []byte) []*record.Record {
	t.Helper()
	var recs []*record.Record
	for len(b) > 0 {
		r, n, err := record.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
		b = b[n:]
	}
	return recs
}

// sealSegment writes segment n's correct seal, as a rollover would, for
// tests that build multi-segment logs by hand. It returns the seal.
func sealSegment(t *testing.T, dir string, n int) seal {
	t.Helper()
	recs := decodeAll(t, readFile(t, segPath(dir, n)))
	last := recs[len(recs)-1]
	s := seal{first: recs[0].ID, last: last.ID, hash: last.ChainHash()}
	if err := writeSeal(dir, n, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// TestAppendAndReopen: records survive a close and reopen, in order, and new
// ids continue where the old ones stopped. Open also creates the directory.
func TestAppendAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	l, rep, _ := mustOpen(t, dir, Options{})
	if rep != (Report{}) {
		t.Fatalf("new log: report %+v, want empty", rep)
	}

	for i, d := range []string{"A", "B", "C"} {
		if id := mustAppend(t, l, d); id != uint64(i+1) {
			t.Fatalf("Append(%q) = id %d, want %d", d, id, i+1)
		}
	}
	first, err := l.AppendBatch([]Entry{
		{Type: record.TypeMemory, Structure: []byte("legs: [1] or [2]"), Data: []byte("M1")},
		{Type: record.TypeMemory, Data: []byte("M2")},
	})
	if err != nil || first != 4 {
		t.Fatalf("AppendBatch = %d, %v; want 4, nil", first, err)
	}
	if r, err := l.Read(5); err != nil || string(r.Data()) != "M2" {
		t.Fatalf("Read(5) = %v, %v", r, err)
	}
	l.Close()

	l, rep, recs := mustOpen(t, dir, Options{})
	defer l.Close()
	if rep != (Report{Records: 5}) {
		t.Fatalf("reopen: report %+v", rep)
	}
	for i, want := range []string{"A", "B", "C", "M1", "M2"} {
		if recs[i].ID != uint64(i+1) || string(recs[i].Data()) != want {
			t.Errorf("record %d = id %d %q, want id %d %q", i, recs[i].ID, recs[i].Data(), i+1, want)
		}
	}
	if string(recs[3].Structure) != "legs: [1] or [2]" {
		t.Errorf("structure of M1 = %q after reopen", recs[3].Structure)
	}
	if id := mustAppend(t, l, "D"); id != 6 {
		t.Fatalf("after reopen, Append = id %d, want 6", id)
	}
}

// TestOptionsValidated: a segment smaller than the largest possible batch
// is refused, so a batch can never fail to fit a fresh segment. Bad options
// are refused before anything is created on disk.
func TestOptionsValidated(t *testing.T) {
	for _, size := range []int64{-1, 4096, MinSegmentSize - 1, MaxSegmentSize + 1} {
		dir := filepath.Join(t.TempDir(), "agent")
		if _, _, err := Open(dir, Options{SegmentSize: size}, nil); !errors.Is(err, ErrBadOptions) {
			t.Errorf("SegmentSize %d: got %v, want ErrBadOptions", size, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("SegmentSize %d: directory was created despite bad options", size)
		}
	}
	for _, size := range []int64{0, MinSegmentSize, MaxSegmentSize} {
		l, _, err := Open(t.TempDir(), Options{SegmentSize: size}, nil)
		if err != nil {
			t.Errorf("SegmentSize %d: %v", size, err)
			continue
		}
		l.Close()
	}
}

// TestRolloverAndReopen: when the active segment is full, the next record
// goes into a new segment; ids run on across segments, Read finds records
// in any segment, and a reopen replays them all in order.
func TestRolloverAndReopen(t *testing.T) {
	dir := t.TempDir()
	data := []string{big("r1"), big("r2"), big("r3"), big("r4"), big("r5")}
	offs := writeLog(t, dir, small, data...)

	want := []pos{{1, 0}, {1, 400_174}, {2, 0}, {2, 400_174}, {3, 0}}
	for i, p := range offs {
		if p != want[i] {
			t.Errorf("record %d at %+v, want %+v", i+1, p, want[i])
		}
	}
	for n := 1; n <= 3; n++ {
		if size := fileSize(t, segPath(dir, n)); size > small.SegmentSize {
			t.Errorf("segment %d is %d bytes, over the %d limit", n, size, small.SegmentSize)
		}
	}

	// Files that are not segments are left alone.
	writeFile(t, filepath.Join(dir, "notes.txt"), []byte("not a segment"))

	l, rep, recs := mustOpen(t, dir, small)
	defer l.Close()
	if rep != (Report{Records: 5}) {
		t.Fatalf("reopen: report %+v", rep)
	}
	for i, r := range recs {
		if r.ID != uint64(i+1) || string(r.Data()) != data[i] {
			t.Fatalf("replayed record %d has id %d and the wrong data", i+1, r.ID)
		}
	}
	for id := uint64(1); id <= 5; id++ {
		if r, err := l.Read(id); err != nil || string(r.Data()) != data[id-1] {
			t.Fatalf("Read(%d) = %v", id, err)
		}
	}

	// After a reopen, appends continue in the last segment while it has room.
	if id := mustAppend(t, l, big("r6")); id != 6 || l.offsets[5] != (pos{3, 400_174}) {
		t.Fatalf("Append after reopen = id %d at %+v, want id 6 at {3 400174}", id, l.offsets[5])
	}
}

// TestBatchNeverSpansSegments: a batch that does not fit in what is left of
// the active segment goes, whole, into a fresh one.
func TestBatchNeverSpansSegments(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := mustOpen(t, dir, small)
	defer l.Close()
	mustAppend(t, l, big("r1"))

	half := []byte(big("half"))
	first, err := l.AppendBatch([]Entry{{Type: record.TypeLot, Data: half}, {Type: record.TypeLot, Data: half}})
	if err != nil {
		t.Fatal(err)
	}
	if first != 2 || l.offsets[1] != (pos{2, 0}) || l.offsets[2] != (pos{2, 400_174}) {
		t.Fatalf("batch landed at %+v, %+v; want both in segment 2", l.offsets[1], l.offsets[2])
	}
	if size := fileSize(t, segPath(dir, 1)); size != 400_174 {
		t.Fatalf("segment 1 is %d bytes; it should hold only record 1", size)
	}
}

// TestTornTailEveryCut simulates a crash in the middle of writing the last
// record, at every possible byte. Open must keep the complete records, cut
// the partial one, and report exactly how much it cut.
func TestTornTailEveryCut(t *testing.T) {
	base := t.TempDir()
	full, offs := writeOneSegment(t, filepath.Join(base, "full"), "A", "B", "carry an umbrella")
	start := offs[2] // where the last record begins

	for cut := start; cut < int64(len(full)); cut++ {
		dir := filepath.Join(base, fmt.Sprintf("cut%d", cut))
		writeFile(t, segPath(dir, 1), full[:cut])
		l, rep, recs := mustOpen(t, dir, Options{})
		l.Close()
		if rep.Records != 2 || len(recs) != 2 || rep.DroppedBytes != cut-start {
			t.Fatalf("cut at %d: report %+v, want 2 records and %d dropped", cut, rep, cut-start)
		}
		if got := fileSize(t, segPath(dir, 1)); got != start {
			t.Fatalf("cut at %d: segment is %d bytes after Open, want %d", cut, got, start)
		}
	}

	// After recovery the log keeps working, and the lost id is reused
	// (it was never acknowledged, so nobody can be holding it).
	l, _, _ := mustOpen(t, filepath.Join(base, fmt.Sprintf("cut%d", start+1)), Options{})
	defer l.Close()
	if id := mustAppend(t, l, "again"); id != 3 {
		t.Fatalf("Append after recovery = id %d, want 3", id)
	}
}

// TestTornTailInActiveSegment: after rollovers, a crash can only tear the
// active segment. It is cut; the sealed segments are not touched.
func TestTornTailInActiveSegment(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, small, big("r1"), big("r2"), big("r3"), big("r4"), big("r5"))
	seg3 := readFile(t, segPath(dir, 3))
	writeFile(t, segPath(dir, 3), seg3[:len(seg3)/2])

	l, rep, _ := mustOpen(t, dir, small)
	defer l.Close()
	if rep.Records != 4 || rep.DroppedBytes != int64(len(seg3)/2) {
		t.Fatalf("report %+v, want 4 records and %d dropped", rep, len(seg3)/2)
	}
	if fileSize(t, segPath(dir, 1)) != 800_348 || fileSize(t, segPath(dir, 2)) != 800_348 {
		t.Fatal("a sealed segment was changed")
	}
	if id := mustAppend(t, l, "again"); id != 5 || l.offsets[4] != (pos{3, 0}) {
		t.Fatalf("Append after recovery = id %d at %+v, want id 5 at {3 0}", id, l.offsets[4])
	}
}

// TestDamageInSealedSegmentIsCorrupt: a sealed segment was fully fsynced
// before the next one existed, so even a tiny bad region in it cannot be a
// torn write. Unlike damage at the end of the active segment, it is never
// cut off: Open refuses and leaves every file as it was.
func TestDamageInSealedSegmentIsCorrupt(t *testing.T) {
	for name, damage := range map[string]func([]byte) []byte{
		"one flipped byte in its last record": func(b []byte) []byte { b[len(b)-1] ^= 0xFF; return b },
		"its last 10 bytes missing":           func(b []byte) []byte { return b[:len(b)-10] },
	} {
		dir := t.TempDir()
		writeLog(t, dir, small, big("r1"), big("r2"), big("r3"))
		damaged := damage(readFile(t, segPath(dir, 1)))
		writeFile(t, segPath(dir, 1), damaged)

		if _, _, err := Open(dir, small, nil); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: got %v, want ErrCorrupt", name, err)
		}
		if got := fileSize(t, segPath(dir, 1)); got != int64(len(damaged)) {
			t.Errorf("%s: sealed segment changed size; a corrupt log must not be touched", name)
		}
	}
}

// TestMissingSegmentIsCorrupt: a gap in the numbering means a lost file
// full of acknowledged records.
func TestMissingSegmentIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, small, big("r1"), big("r2"), big("r3"), big("r4"), big("r5"))
	if err := os.Remove(segPath(dir, 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(dir, small, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

// TestIDsMustContinueAcrossSegments: the first id of segment N+1 must be the
// last id of segment N plus one. A repeat or a jump at the boundary is
// corruption, even though each segment on its own looks fine.
func TestIDsMustContinueAcrossSegments(t *testing.T) {
	for _, firstID := range []uint64{2, 4} { // segment 1 ends with id 2; 3 is right
		dir := t.TempDir()
		writeLog(t, dir, Options{}, "A", "B")
		s := sealSegment(t, dir, 1) // seal and chain are right, so only the id is wrong
		writeFile(t, segPath(dir, 2), encode(t, firstID, s.hash, "C"))

		_, _, err := Open(dir, Options{}, nil)
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "segment 2 starts with id") {
			t.Errorf("segment 2 starting at id %d: got %v, want ErrCorrupt naming the boundary", firstID, err)
		}
	}

	// The correct id passes, which shows the check is not over-eager.
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A", "B")
	s := sealSegment(t, dir, 1)
	writeFile(t, segPath(dir, 2), encode(t, 3, s.hash, "C"))
	l, rep, _ := mustOpen(t, dir, Options{})
	l.Close()
	if rep.Records != 3 {
		t.Fatalf("report %+v, want 3 records", rep)
	}
}

// TestEmptySealedSegmentIsCorrupt: the committer only starts a new segment
// when the active one holds records, so an empty segment that is not the
// last one cannot come from normal operation or a crash.
func TestEmptySealedSegmentIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A", "B")
	s := sealSegment(t, dir, 1)
	writeFile(t, segPath(dir, 2), nil)
	writeFile(t, segPath(dir, 3), encode(t, 3, s.hash, "C")) // ids and chain still continue
	if _, _, err := Open(dir, Options{}, nil); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("got %v, want ErrCorrupt for the empty segment", err)
	}
}

// TestEmptyActiveSegmentIsFine: a crash right after a rollover leaves a new,
// empty last segment. That is normal, and the next record goes into it.
func TestEmptyActiveSegmentIsFine(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A", "B")
	sealSegment(t, dir, 1)
	writeFile(t, segPath(dir, 2), nil)

	l, rep, _ := mustOpen(t, dir, Options{})
	defer l.Close()
	if rep != (Report{Records: 2}) {
		t.Fatalf("report %+v", rep)
	}
	if id := mustAppend(t, l, "C"); id != 3 || l.offsets[2] != (pos{2, 0}) {
		t.Fatalf("Append = id %d at %+v, want id 3 at {2 0}", id, l.offsets[2])
	}
}

// TestFailedRolloverBreaksLog: if the next segment cannot be created, the
// log refuses all further writes, even once the problem is gone (no
// retries). A reopen recovers, with every acknowledged record intact.
func TestFailedRolloverBreaksLog(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := mustOpen(t, dir, small)
	mustAppend(t, l, big("r1"))
	mustAppend(t, l, big("r2")) // segment 1 is now full

	// A directory sitting where segment 2 belongs makes createSegment fail
	// the same way on every OS. A read-only parent directory does not: it
	// is honoured on Unix but ignored for folders on Windows.
	blocked := segPath(dir, 2)
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(record.TypeLot, []byte(big("r3"))); !errors.Is(err, ErrBroken) {
		t.Fatalf("Append needing a new segment = %v, want ErrBroken", err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(record.TypeLot, []byte("small")); !errors.Is(err, ErrBroken) {
		t.Fatalf("Append after a failed rollover = %v, want ErrBroken until reopen", err)
	}
	l.Close()

	l, rep, _ := mustOpen(t, dir, small)
	defer l.Close()
	if rep.Records != 2 {
		t.Fatalf("reopen: report %+v, want the 2 acknowledged records", rep)
	}
	if id := mustAppend(t, l, big("r3")); id != 3 {
		t.Fatalf("Append after reopen = id %d, want 3", id)
	}
}

// TestBadChecksumOnLastRecord: a damaged final record looks exactly like a
// torn write, so it is cut and reported.
func TestBadChecksumOnLastRecord(t *testing.T) {
	dir := t.TempDir()
	full, offs := writeOneSegment(t, dir, "A", "B", "C")
	full[len(full)-1] ^= 0xFF
	writeFile(t, segPath(dir, 1), full)

	l, rep, _ := mustOpen(t, dir, Options{})
	defer l.Close()
	if rep.Records != 2 || rep.DroppedBytes != int64(len(full))-offs[2] {
		t.Fatalf("report %+v", rep)
	}
}

// TestHoleInLastBatch reproduces what group commit can leave after a crash:
// the disk saved a later record but not an earlier one in the same batch.
// Neither was acknowledged, so both are cut and Open still succeeds.
func TestHoleInLastBatch(t *testing.T) {
	dir := t.TempDir()
	full, offs := writeOneSegment(t, dir, "A", "B", "C", "D")
	clear(full[offs[2]:offs[3]]) // record 3 never reached the disk; record 4 did
	writeFile(t, segPath(dir, 1), full)

	l, rep, _ := mustOpen(t, dir, Options{})
	defer l.Close()
	if rep.Records != 2 || rep.DroppedBytes != int64(len(full))-offs[2] {
		t.Fatalf("report %+v, want 2 records and the last %d bytes dropped", rep, int64(len(full))-offs[2])
	}
}

// TestDamageBeyondOneBatchIsCorrupt: a bad record in the active segment with
// more than one batch of data after it cannot be a crash, so Open refuses
// the log and leaves it untouched instead of dropping acknowledged records.
func TestDamageBeyondOneBatchIsCorrupt(t *testing.T) {
	for name, damage := range map[string]func(b []byte){
		"bad checksum": func(b []byte) { b[record.PrefixSize+record.HeaderSize+20] ^= 0xFF },
		"bad length":   func(b []byte) { copy(b[0:4], []byte{0xFF, 0xFF, 0xFF, 0xFF}) },
	} {
		dir := t.TempDir()
		full, _ := writeOneSegment(t, dir, "first", big("a"), big("b"), big("c")) // ~1.2 MB after record 1
		damage(full)
		writeFile(t, segPath(dir, 1), full)

		if _, _, err := Open(dir, Options{}, nil); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: got %v, want ErrCorrupt", name, err)
		}
		if got := fileSize(t, segPath(dir, 1)); got != int64(len(full)) {
			t.Errorf("%s: segment changed from %d to %d bytes; a corrupt log must not be touched", name, len(full), got)
		}
	}
}

// TestSmallLogDamageIsCutAsTail documents the accepted cost of the
// size-bounded rule: in an active segment smaller than one batch, damage
// anywhere looks like a torn batch, so everything from it onward is dropped
// (and reported). Sealed segments do not have this weakness.
func TestSmallLogDamageIsCutAsTail(t *testing.T) {
	dir := t.TempDir()
	full, offs := writeOneSegment(t, dir, "A", "B", "C")
	full[offs[1]+record.PrefixSize+record.HeaderSize] ^= 0xFF // damage record 2 of 3
	writeFile(t, segPath(dir, 1), full)

	l, rep, _ := mustOpen(t, dir, Options{})
	defer l.Close()
	if rep.Records != 1 || rep.DroppedBytes != int64(len(full))-offs[1] {
		t.Fatalf("report %+v, want 1 record kept and records 2-3 dropped", rep)
	}
}

// TestWrongIDIsCorrupt: a valid record with an unexpected id cannot come
// from a crash, so it is corruption even at the end of the active segment.
func TestWrongIDIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	full, _ := writeOneSegment(t, dir, "A")
	writeFile(t, segPath(dir, 1), append(full, encode(t, 5, [32]byte{}, "skipped ahead")...))

	if _, _, err := Open(dir, Options{}, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

// TestConcurrentAppendsShareFsyncs: many goroutines append at once, enough to
// fill several segments. Every record gets a unique id with no gaps, all of
// them survive a reopen, and group commit means far fewer fsyncs than records.
func TestConcurrentAppendsShareFsyncs(t *testing.T) {
	const workers, each = 20, 25
	const total = workers * each
	dir := t.TempDir()
	l, _, _ := mustOpen(t, dir, small)
	payload := strings.Repeat("y", 10_000) // 500 x ~10 KB = ~5 MB: about 5 segments

	ids := make(chan uint64, total)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				id, err := l.Append(record.TypeLot, fmt.Appendf(nil, "w%d-%d %s", w, i, payload))
				if err != nil {
					t.Error(err)
					return
				}
				ids <- id
			}
		})
	}
	wg.Wait()
	close(ids)

	seen := make([]bool, total+1)
	for id := range ids {
		if id < 1 || id > total || seen[id] {
			t.Fatalf("id %d out of range or handed out twice", id)
		}
		seen[id] = true
	}
	syncs, segs := l.syncs.Load(), len(l.segs)
	t.Logf("%d appends shared %d fsyncs across %d segments", total, syncs, segs)
	if syncs >= total {
		t.Errorf("%d fsyncs for %d appends: group commit did not batch", syncs, total)
	}
	if segs < 3 {
		t.Errorf("only %d segments; the test should cross several rollovers", segs)
	}
	l.Close()

	l, rep, _ := mustOpen(t, dir, small)
	defer l.Close()
	if rep.Records != total {
		t.Fatalf("reopen found %d records, want %d", rep.Records, total)
	}
}

// TestBatchTooLarge: a batch that could not be recovered as one torn tail
// is refused before anything is written.
func TestBatchTooLarge(t *testing.T) {
	l, _, _ := mustOpen(t, t.TempDir(), Options{})
	defer l.Close()
	half := bytes.Repeat([]byte("x"), 600_000)
	_, err := l.AppendBatch([]Entry{{Type: record.TypeLot, Data: half}, {Type: record.TypeLot, Data: half}})
	if !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("got %v, want ErrBatchTooLarge", err)
	}
	if _, err := l.Read(1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing should have been written, but Read(1) = %v", err)
	}
}

func TestReadUnknownID(t *testing.T) {
	l, _, _ := mustOpen(t, t.TempDir(), Options{})
	defer l.Close()
	mustAppend(t, l, "A")
	for _, id := range []uint64{0, 2} {
		if _, err := l.Read(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Read(%d) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestUseAfterClose(t *testing.T) {
	l, _, _ := mustOpen(t, t.TempDir(), Options{})
	mustAppend(t, l, "A")
	l.Close()
	if _, err := l.Append(record.TypeLot, []byte("B")); !errors.Is(err, ErrClosed) {
		t.Errorf("Append after Close = %v, want ErrClosed", err)
	}
	if _, err := l.Read(1); !errors.Is(err, ErrClosed) {
		t.Errorf("Read after Close = %v, want ErrClosed", err)
	}
}

// TestReplayCallbackErrorStopsOpen: if the layer above rejects a record
// while rebuilding its state, Open fails instead of starting half-built.
func TestReplayCallbackErrorStopsOpen(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A")
	boom := errors.New("boom")
	if _, _, err := Open(dir, Options{}, func(*record.Record) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the callback's error", err)
	}
}
