package ledger

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/benchgen"
	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

// Tests for snapshots (state.snap). The rule they check: a snapshot can
// make Open faster, never give a different answer. So every test compares
// the ledger opened with the snapshot against a full replay of a copy of
// the same files without it.

// small makes segments as small as allowed, so tests reach several
// segments with a few large lots.
var small = logfile.Options{SegmentSize: logfile.MinSegmentSize}

func snapPath(dir string) string { return filepath.Join(dir, snapFile) }

func hasSnapshot(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(snapPath(dir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// replayCopy copies dir's files, without its snapshot, to a new folder:
// what a full replay of the same log gives is the reference.
func replayCopy(t *testing.T, dir string) string {
	t.Helper()
	cp := filepath.Join(t.TempDir(), "copy")
	if err := os.CopyFS(cp, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(snapPath(cp)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return cp
}

// openBoth opens dir (expecting its snapshot to cover wantSnap records, 0
// for none) and a full replay of a copy of it taken first, and checks they
// agree on everything: the whole state, Active, Get of every id, Verify's
// head and the Report.
func openBoth(t *testing.T, dir string, opts logfile.Options, wantSnap int) (*Ledger, logfile.Report) {
	t.Helper()
	ref := replayCopy(t, dir)
	l, rep, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	if rep.Snapshot != wantSnap {
		t.Fatalf("Report.Snapshot = %d, want %d", rep.Snapshot, wantSnap)
	}
	full, fullRep, err := Open(ref, opts)
	if err != nil {
		t.Fatalf("full replay: %v", err)
	}
	defer closeNoSnapshot(full)
	if fullRep.Snapshot != 0 {
		t.Fatalf("the copy has no snapshot, but Report.Snapshot = %d", fullRep.Snapshot)
	}
	sameLedger(t, l, full)
	got := rep
	got.Snapshot = 0
	if !reflect.DeepEqual(got, fullRep) {
		t.Fatalf("Report = %+v, full replay %+v", rep, fullRep)
	}
	return l, rep
}

func sameLedger(t *testing.T, a, b *Ledger) {
	t.Helper()
	if !reflect.DeepEqual(a.st, b.st) {
		t.Fatal("the state differs from a full replay")
	}
	if !slices.Equal(a.Active(), b.Active()) {
		t.Fatalf("Active = %v, full replay %v", a.Active(), b.Active())
	}
	for id := ID(0); id <= ID(len(b.st.nodes))+1; id++ {
		x, xerr := a.Get(id)
		y, yerr := b.Get(id)
		if !reflect.DeepEqual(x, y) || (xerr == nil) != (yerr == nil) {
			t.Fatalf("Get(%d) = %+v, %v; full replay %+v, %v", id, x, xerr, y, yerr)
		}
	}
	ha, err := a.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if hb := must(b.Verify()); ha != hb {
		t.Fatal("Verify's head differs from a full replay")
	}
}

// workload runs n random operations on l: lots, memories with one or two
// legs, Fair and Strict recalls, and, if erase is set, Erase recalls and
// Compacts. Refusals (a dead parent, ...) are fine.
func workload(l *Ledger, rng *rand.Rand, n int, erase bool) {
	for range n {
		count := len(l.st.nodes)
		pick := func() ID { return ID(count - rng.IntN(min(count, 12))) }
		switch r := rng.IntN(20); {
		case r < 6 || count == 0:
			must(l.AddLot("tool", []byte("lot text")))
		case r < 15:
			l.Remember([]byte("memory text"), Leg{pick()}, Leg{pick(), pick()})
		case r < 18:
			l.Recall(pick(), Mode(1+rng.IntN(2)), Untrusted)
		case erase && r == 18:
			l.Recall(pick(), Mode(1+rng.IntN(2)), Erase)
		case erase:
			must(l.Compact())
		}
	}
}

// TestSnapshotMatchesReplay: a mix of everything, a snapshot, more
// records after it, then reopen: the same as a full replay. Then an Erase
// and a Compact (which delete the snapshot), a new snapshot, and reopen
// again: erased content must be reported erased, the rest present.
func TestSnapshotMatchesReplay(t *testing.T) {
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(5, 6))
	l := mustOpen(t, dir)
	workload(l, rng, 150, true)
	must(l.Compact())
	if err := l.saveSnapshot(); err != nil {
		t.Fatal(err)
	}
	covered := len(l.st.nodes)
	workload(l, rng, 100, false) // after the snapshot
	closeNoSnapshot(l)

	l, _ = openBoth(t, dir, logfile.Options{}, covered)

	// Erase and Compact rewrite segments: the snapshot must go.
	target := l.Active()[0]
	if _, err := l.Recall(target, Strict, Erase); err != nil {
		t.Fatal(err)
	}
	if hasSnapshot(t, dir) {
		t.Fatal("the snapshot survived an Erase")
	}
	workload(l, rng, 40, false)
	if n := must(l.Compact()); n == 0 {
		t.Fatal("Compact erased nothing; the test wants it to")
	}
	if err := l.saveSnapshot(); err != nil {
		t.Fatal(err)
	}
	covered = len(l.st.nodes)
	closeNoSnapshot(l)

	l, _ = openBoth(t, dir, logfile.Options{}, covered)
	if it := must(l.Get(target)); !it.Redacted || it.Data != nil {
		t.Fatalf("Get(erased %d) = %+v, want redacted with no data", target, it)
	}
}

// writeRaw writes the benchmark workload of n memories straight through
// the log, in large batches, as the benchmarks do: much faster than one
// Remember (and one fsync) each.
func writeRaw(t *testing.T, dir string, n int) {
	t.Helper()
	raw, _, err := logfile.Open(dir, logfile.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var batch []logfile.Entry
	for _, op := range benchgen.Dataset(n) {
		e := logfile.Entry{Type: record.TypeLot, Structure: encodeLot(benchgen.SourceType), Data: []byte("x")}
		if !op.Lot {
			e = logfile.Entry{Type: record.TypeMemory, Structure: encodeMemory(toLegs(op.Legs)), Data: []byte("y")}
		}
		batch = append(batch, e)
	}
	for len(batch) > 0 {
		k := min(len(batch), 2000)
		if _, err := raw.AppendBatch(batch[:k]); err != nil {
			t.Fatal(err)
		}
		batch = batch[k:]
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCloseWritesSnapshot: Close writes a snapshot once snapshotEvery
// records are new, and not before.
func TestCloseWritesSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, snapshotEvery)
	l, rep, err := Open(dir, logfile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Snapshot != 0 {
		t.Fatalf("Report.Snapshot = %d with no snapshot", rep.Snapshot)
	}
	total := rep.Records
	l.Close()
	if !hasSnapshot(t, dir) {
		t.Fatal("Close wrote no snapshot")
	}

	l, _ = openBoth(t, dir, logfile.Options{}, total)
	must(l.AddLot("tool", []byte("one more")))
	l.Close()
	p, _, err := readSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Records != uint64(total) {
		t.Fatalf("one new record wrote a new snapshot (covers %d)", p.Records)
	}

	l, rep = openBoth(t, dir, logfile.Options{}, total)
	if rep.Records != total+1 {
		t.Fatalf("Records = %d, want %d", rep.Records, total+1)
	}
	// An Erase deletes the snapshot; Close writes a new one, since all
	// the records are now newer than any snapshot on disk.
	must(l.Recall(1, Fair, Erase))
	if hasSnapshot(t, dir) {
		t.Fatal("the snapshot survived an Erase")
	}
	l.Close()
	openBoth(t, dir, logfile.Options{}, total+2)
}

// TestFailedSnapshotWriteKeepsClose: a snapshot that cannot be written
// is skipped. Close still succeeds and nothing is lost.
func TestFailedSnapshotWriteKeepsClose(t *testing.T) {
	needPermissions(t)
	dir := t.TempDir()
	writeRaw(t, dir, snapshotEvery)
	l := mustOpen(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := l.Close()
	os.Chmod(dir, 0o700)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if hasSnapshot(t, dir) {
		t.Fatal("a snapshot was written into a read-only folder")
	}
	openBoth(t, dir, logfile.Options{}, 0)
}

// snapshotted builds a ledger in dir with a snapshot, then more records
// after it, and returns how many records the snapshot covers.
func snapshotted(t *testing.T, dir string, seed uint64) int {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed))
	l := mustOpen(t, dir)
	buildRef(t, l)
	workload(l, rng, 60, true)
	if err := l.saveSnapshot(); err != nil {
		t.Fatal(err)
	}
	covered := len(l.st.nodes)
	workload(l, rng, 20, false)
	closeNoSnapshot(l)
	return covered
}

// rewriteSnapshot replaces dir's snapshot with edit(its bytes), fixing the
// crc afterwards, so the edit is checked by more than the crc.
func rewriteSnapshot(t *testing.T, dir string, edit func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(snapPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	b = edit(b[:len(b)-4])
	b = binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, castagnoli))
	if err := os.WriteFile(snapPath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotFallsBack: each way a snapshot can be wrong makes Open
// throw it away and replay the whole log, with the same result as if it
// had never existed. The snapshot file is then deleted.
func TestSnapshotFallsBack(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"corrupt snapshot": func(t *testing.T, dir string) {
			b, _ := os.ReadFile(snapPath(dir))
			b[len(b)/2] ^= 1
			os.WriteFile(snapPath(dir), b, 0o600)
		},
		"truncated snapshot": func(t *testing.T, dir string) {
			b, _ := os.ReadFile(snapPath(dir))
			os.WriteFile(snapPath(dir), b[:len(b)/2], 0o600)
		},
		"empty snapshot": func(t *testing.T, dir string) {
			os.WriteFile(snapPath(dir), nil, 0o600)
		},
		"unknown version": func(t *testing.T, dir string) {
			rewriteSnapshot(t, dir, func(b []byte) []byte { b[len(snapMagic)] = 2; return b })
		},
		"impossible state with a good crc": func(t *testing.T, dir string) {
			// Mark every node dead but the last memory: it then lives
			// with no live leg, which replay can never build.
			p, s, err := readSnapshot(dir)
			if err != nil {
				t.Fatal(err)
			}
			for i := range s.nodes {
				s.nodes[i].dead = s.nodes[i].kind != record.TypeRecall
			}
			for i := len(s.nodes) - 1; i >= 0; i-- {
				if s.nodes[i].kind == record.TypeMemory {
					s.nodes[i].dead = false
					break
				}
			}
			os.WriteFile(snapPath(dir), encodeSnapshot(p, &s), 0o600)
		},
		"snapshot newer than the log": func(t *testing.T, dir string) {
			// The log is restored from a backup taken before the
			// snapshot's last records were written.
			b, _ := os.ReadFile(filepath.Join(dir, "000001.seg"))
			l := mustOpen(t, dir)
			workload(l, rand.New(rand.NewPCG(9, 9)), 10, false)
			if err := l.saveSnapshot(); err != nil {
				t.Fatal(err)
			}
			closeNoSnapshot(l)
			os.WriteFile(filepath.Join(dir, "000001.seg"), b, 0o600)
		},
		"log truncated to before the snapshot": func(t *testing.T, dir string) {
			// Cut the newest records: the log looks valid, just shorter.
			l := mustOpen(t, dir)
			r, _ := l.log.Read(uint64(len(l.st.nodes) - 25))
			closeNoSnapshot(l)
			seg := filepath.Join(dir, "000001.seg")
			info, _ := os.Stat(seg)
			// Find where that record starts by its bytes.
			b, _ := os.ReadFile(seg)
			enc, _ := record.Encode(r)
			at := bytes.Index(b, enc)
			if at < 0 || int64(at) >= info.Size() {
				t.Fatal("record not found")
			}
			os.Truncate(seg, int64(at))
		},
		"snapshot from another agent": func(t *testing.T, dir string) {
			// Same operations, so the same shape and sizes; only the
			// salts, and so the chain hashes, differ.
			other := t.TempDir()
			snapshotted(t, other, 1)
			b, _ := os.ReadFile(snapPath(other))
			os.WriteFile(snapPath(dir), b, 0o600)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			snapshotted(t, dir, 1)
			damage(t, dir)
			openBoth(t, dir, logfile.Options{}, 0)
			if hasSnapshot(t, dir) {
				t.Fatal("the bad snapshot was not deleted")
			}
		})
	}
}

// TestSnapshotTornTailAfterIt: a crash cut the last write, which came
// after the snapshot. The snapshot is still good: Open uses it and cuts
// the torn tail, as a full replay does.
func TestSnapshotTornTailAfterIt(t *testing.T) {
	dir := t.TempDir()
	covered := snapshotted(t, dir, 2)
	f, err := os.OpenFile(filepath.Join(dir, "000001.seg"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(bytes.Repeat([]byte{0xff}, 100)) // half a record's worth of garbage
	f.Close()
	_, rep := openBoth(t, dir, logfile.Options{}, covered)
	if rep.DroppedBytes != 100 {
		t.Fatalf("DroppedBytes = %d, want 100", rep.DroppedBytes)
	}
}

// bigLots builds a ledger of n lots of 100 KB in small segments, so each
// segment holds about ten, saves a snapshot and closes it.
func bigLots(t *testing.T, dir string, n int) {
	t.Helper()
	l, _, err := Open(dir, small)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		must(l.AddLot("doc", []byte(strings.Repeat(string(rune('a'+i%26)), 100_000))))
	}
	if err := l.saveSnapshot(); err != nil {
		t.Fatal(err)
	}
	closeNoSnapshot(l)
}

// TestSnapshotOverSegments: a snapshot over several segments, with
// records after it in a new segment, and a crash right after a rollover.
func TestSnapshotOverSegments(t *testing.T) {
	dir := t.TempDir()
	bigLots(t, dir, 25)
	l, _, err := Open(dir, small)
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		must(l.AddLot("doc", bytes.Repeat([]byte("z"), 100_000)))
	}
	closeNoSnapshot(l)
	openBoth(t, dir, small, 25)
}

// TestSnapshotDoesNotHideSealedDamage: bytes added to a sealed segment
// the snapshot covers. Its size no longer matches, so the snapshot is
// dropped, and the full replay refuses the log as before.
func TestSnapshotDoesNotHideSealedDamage(t *testing.T) {
	dir := t.TempDir()
	bigLots(t, dir, 25)
	f, err := os.OpenFile(filepath.Join(dir, "000001.seg"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("junk"))
	f.Close()
	if _, _, err := Open(dir, small); !errors.Is(err, logfile.ErrCorrupt) {
		t.Fatalf("Open: %v, want ErrCorrupt", err)
	}
}

// TestSnapshotOpenSkipsOldRecords documents the guarantee that changed.
// A record in the middle of a segment the snapshot covers is damaged
// (same size). Without the snapshot, Open refuses the log. With it, Open
// does not read that record, so it succeeds; reading the record, and
// Verify, still catch it. See ARCHITECTURE.md section 6.
func TestSnapshotOpenSkipsOldRecords(t *testing.T) {
	dir := t.TempDir()
	bigLots(t, dir, 25)
	seg := filepath.Join(dir, "000001.seg")
	b, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(b, []byte("ccccc")) // record 3, in the middle of segment 1
	b[at] = 'C'
	os.WriteFile(seg, b, 0o600)

	if _, _, err := Open(replayCopy(t, dir), small); !errors.Is(err, logfile.ErrCorrupt) {
		t.Fatalf("full replay: %v, want ErrCorrupt", err)
	}
	l, rep, err := Open(dir, small)
	if err != nil {
		t.Fatalf("Open with the snapshot: %v", err)
	}
	defer l.Close()
	if rep.Snapshot != 25 {
		t.Fatalf("Report.Snapshot = %d, want 25", rep.Snapshot)
	}
	if _, err := l.Get(3); !errors.Is(err, logfile.ErrCorrupt) {
		t.Fatalf("Get(3): %v, want ErrCorrupt", err)
	}
	if _, err := l.Verify(); !errors.Is(err, logfile.ErrCorrupt) {
		t.Fatalf("Verify: %v, want ErrCorrupt", err)
	}
}

// TestInterruptedEraseAfterSnapshot: the Erase recall reached the log
// after the snapshot, and the crash came before its content was erased.
// Open, from the snapshot, still finishes it.
func TestInterruptedEraseAfterSnapshot(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	if err := l.saveSnapshot(); err != nil {
		t.Fatal(err)
	}
	closeNoSnapshot(l)
	raw, _, err := logfile.Open(dir, logfile.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.AppendBatch([]logfile.Entry{{Type: record.TypeRecall, Structure: encodeRecall(r.c, Fair, Erase)}}); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	_, rep := openBoth(t, dir, logfile.Options{}, 6)
	if rep.FinishedErase != 3 {
		t.Fatalf("FinishedErase = %d, want 3", rep.FinishedErase)
	}
	expectOnDisk(t, dir, []string{textC, textM2, textM3}, []string{textA, textB, textM1})
	if hasSnapshot(t, dir) {
		t.Fatal("the snapshot survived the erase")
	}
}

// TestInterruptedEraseInSnapshot: the snapshot itself holds an Erase
// recall whose content is still on disk (the crash came after the
// snapshot was saved). Its erase targets must bring the erase to an end.
func TestInterruptedEraseInSnapshot(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	// What Recall does, without the erase at the end.
	id := must(l.log.AppendBatch([]logfile.Entry{{Type: record.TypeRecall, Structure: encodeRecall(r.c, Fair, Erase)}}))
	must(l.st.recall(ID(id), r.c, Fair))
	l.st.eraseTargets = append(l.st.eraseTargets, r.c)
	if err := l.saveSnapshot(); err != nil {
		t.Fatal(err)
	}
	closeNoSnapshot(l)

	_, rep := openBoth(t, dir, logfile.Options{}, 7)
	if rep.FinishedErase != 3 {
		t.Fatalf("FinishedErase = %d, want 3", rep.FinishedErase)
	}
	expectOnDisk(t, dir, []string{textC, textM2, textM3}, []string{textA, textB, textM1})
}

// TestSnapshotHoldsNoText: erased or not, no content is copied into the
// snapshot, so an Erase never has to look there.
func TestSnapshotHoldsNoText(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	buildRef(t, l)
	if err := l.saveSnapshot(); err != nil {
		t.Fatal(err)
	}
	closeNoSnapshot(l)
	b, err := os.ReadFile(snapPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{textA, textB, textC, textM1, textM2, textM3, "user_message"} {
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("the snapshot holds %q", s)
		}
	}
}

func FuzzDecodeSnapshot(f *testing.F) {
	dir := f.TempDir()
	l, _, err := Open(dir, logfile.Options{})
	if err != nil {
		f.Fatal(err)
	}
	a, _ := l.AddLot("t", []byte("a"))
	b, _ := l.AddLot("t", []byte("b"))
	m, _ := l.Remember([]byte("m"), Leg{a}, Leg{b})
	l.Remember([]byte("n"), Leg{a, m})
	l.Recall(b, Strict, Erase)
	l.saveSnapshot()
	closeNoSnapshot(l)
	seed, _ := os.ReadFile(snapPath(dir))
	f.Add(seed[:len(seed)-4])
	f.Fuzz(func(t *testing.T, b []byte) {
		// Fix the crc, so the fuzzer gets past it to the decoding.
		b = binary.LittleEndian.AppendUint32(slices.Clone(b), crc32.Checksum(b, castagnoli))
		decodeSnapshot(b)
	})
}

// TestDecodeSnapshotRejectsImpossibleState: a snapshot whose crc is good
// but whose state no replay could build is refused when it is read, before
// any record is looked at.
func TestDecodeSnapshotRejectsImpossibleState(t *testing.T) {
	cases := map[string]func(s *state){
		"alive memory with no live leg": func(s *state) {
			s.nodes[0].dead, s.nodes[1].dead = true, true // A and B: M1's two legs
		},
		"erase target alive":       func(s *state) { s.eraseTargets = append(s.eraseTargets, 4) },
		"recall marked dead":       func(s *state) { s.nodes[6].dead = true },
		"memory built on a recall": func(s *state) { s.nodes[7].legs = []leg{{parents: []ID{7}}} },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l := mustOpen(t, dir)
			r := buildRef(t, l)
			must(l.Recall(r.c, Fair, Untrusted))    // id 7
			must(l.Remember([]byte("x"), Leg{r.a})) // id 8
			p := must(l.log.Position())
			s := l.st
			closeNoSnapshot(l)
			if _, _, err := decodeSnapshot(encodeSnapshot(p, &s)); err != nil {
				t.Fatalf("the untouched state is refused: %v", err)
			}
			s.nodes = slices.Clone(s.nodes)
			edit(&s)
			if _, _, err := decodeSnapshot(encodeSnapshot(p, &s)); err == nil {
				t.Fatal("decodeSnapshot accepted it")
			}
		})
	}
}
