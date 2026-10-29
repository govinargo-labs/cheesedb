package logfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// Tests for the hash chain, seals and Verify.

// reencode decodes a segment, lets edit change its records (as an attacker
// with file access could), and writes them back with valid crcs. Anything
// the crc protects is "fixed", so only the chain or digest can catch it.
func reencode(t *testing.T, dir string, n int, edit func([]*record.Record) []*record.Record) {
	t.Helper()
	var out []byte
	for _, r := range edit(decodeAll(t, readFile(t, segPath(dir, n)))) {
		b, err := record.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	writeFile(t, segPath(dir, n), out)
}

// TestChainLinksRecords: the first record links to zeros, every later one
// to the record before it, across segment boundaries too.
func TestChainLinksRecords(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, small, big("r1"), big("r2"), big("r3"), big("r4"), big("r5"))
	l, _, recs := mustOpen(t, dir, small)
	defer l.Close()
	var want [32]byte
	for _, r := range recs {
		if r.PrevHash != want {
			t.Fatalf("record %d: prevHash does not match record %d's chain hash", r.ID, r.ID-1)
		}
		want = r.ChainHash()
	}
	if l.lastHash != want {
		t.Fatal("after replay, the log's last hash is not the newest record's")
	}
}

// TestEditedRecordIsCorrupt: changing a record's structure (its legs) and
// fixing its crc is caught by the chain at the next record, both by Open
// and by Verify on a log that is already open.
func TestEditedRecordIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := mustOpen(t, dir, Options{})
	for _, s := range []string{"legs: [1]", "legs: [2]", "legs: [3]"} {
		if _, err := l.AppendBatch([]Entry{{Type: record.TypeMemory, Structure: []byte(s), Data: []byte("m")}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Verify(); err != nil {
		t.Fatalf("Verify on an untouched log: %v", err)
	}

	reencode(t, dir, 1, func(rs []*record.Record) []*record.Record {
		rs[1].Structure = []byte("legs: [9]")
		return rs
	})
	if _, err := l.Verify(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify after the edit = %v, want ErrCorrupt", err)
	}
	l.Close()
	if _, _, err := Open(dir, Options{}, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open after the edit = %v, want ErrCorrupt", err)
	}
}

// TestRemovedOrReorderedRecordsAreCorrupt: removing or swapping records,
// even with ids renumbered and crcs fixed so nothing else notices, breaks
// the chain.
func TestRemovedOrReorderedRecordsAreCorrupt(t *testing.T) {
	for name, edit := range map[string]func([]*record.Record) []*record.Record{
		"record 2 removed": func(rs []*record.Record) []*record.Record {
			rs = append(rs[:1], rs[2:]...)
			rs[1].ID = 2
			return rs
		},
		"records 2 and 3 swapped": func(rs []*record.Record) []*record.Record {
			rs[1], rs[2] = rs[2], rs[1]
			rs[1].ID, rs[2].ID = 2, 3
			return rs
		},
		"record 1 copied in again": func(rs []*record.Record) []*record.Record {
			dup := *rs[0]
			rs = append(rs[:1], append([]*record.Record{&dup}, rs[1:]...)...)
			for i, r := range rs {
				r.ID = uint64(i + 1)
			}
			return rs
		},
	} {
		dir := t.TempDir()
		writeLog(t, dir, Options{}, "A", "B", "C")
		reencode(t, dir, 1, edit)
		if _, _, err := Open(dir, Options{}, nil); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Open = %v, want ErrCorrupt", name, err)
		}
	}
}

// TestRedactionKeepsChain is the reason records have three parts: erasing a
// record's content (what redaction does) leaves the chain intact,
// because the chain hash covers the header and structure, not the content.
func TestRedactionKeepsChain(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A", "user lives in Pune", "C")
	reencode(t, dir, 1, func(rs []*record.Record) []*record.Record {
		rs[1].Content = nil // redacted
		return rs
	})

	l, rep, recs := mustOpen(t, dir, Options{})
	defer l.Close()
	if rep.Records != 3 || !recs[1].Redacted() {
		t.Fatalf("report %+v, record 2 redacted: %v", rep, recs[1].Redacted())
	}
	if _, err := l.Verify(); err != nil {
		t.Fatalf("Verify after redaction: %v", err)
	}
}

// TestSealsAreChecked: each sealed segment must have a seal that matches
// it, and no seal may exist without its segment.
func TestSealsAreChecked(t *testing.T) {
	for name, damage := range map[string]func(dir string){
		"seal deleted": func(dir string) { os.Remove(filepath.Join(dir, sealName(1))) },
		"seal byte flipped": func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, sealName(1)))
			b[20] ^= 0xFF
			os.WriteFile(filepath.Join(dir, sealName(1)), b, 0o600)
		},
		"seal with a wrong last id": func(dir string) {
			s, _ := readSeal(dir, 1)
			s.last++
			writeSeal(dir, 1, s)
		},
		"seal with a wrong hash": func(dir string) {
			s, _ := readSeal(dir, 1)
			s.hash[0] ^= 1
			writeSeal(dir, 1, s)
		},
		"seal with no segment": func(dir string) { writeSeal(dir, 9, seal{}) },
	} {
		dir := t.TempDir()
		writeLog(t, dir, small, big("r1"), big("r2"), big("r3"), big("r4"), big("r5"))
		damage(dir)
		if _, _, err := Open(dir, small, nil); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Open = %v, want ErrCorrupt", name, err)
		}
	}
}

// TestCrashAfterSealBeforeNextSegment: a crash between writing a segment's
// seal and creating the next segment. Open finishes the rollover, says so
// in the report, and the log carries on.
func TestCrashAfterSealBeforeNextSegment(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A", "B")
	sealSegment(t, dir, 1) // the seal is written; segment 2 never was

	l, rep, _ := mustOpen(t, dir, Options{})
	if rep != (Report{Records: 2, ResumedRollover: true}) {
		t.Fatalf("report %+v, want 2 records and a resumed rollover", rep)
	}
	if id := mustAppend(t, l, "C"); id != 3 || l.offsets[2] != (pos{2, 0}) {
		t.Fatalf("Append = id %d at %+v, want id 3 at {2 0}", id, l.offsets[2])
	}
	l.Close()

	l, rep, _ = mustOpen(t, dir, Options{})
	defer l.Close()
	if rep != (Report{Records: 3}) {
		t.Fatalf("second reopen: report %+v", rep)
	}
}

// TestVerifyCatchesTamperingInSealedSegment: a record in a sealed segment
// changed after Open (content rewritten, digest and crc left to catch it)
// and a changed seal are both found by Verify.
func TestVerifyCatchesTamperingInSealedSegment(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, small, big("r1"), big("r2"), big("r3"))
	l, _, _ := mustOpen(t, dir, small)
	defer l.Close()
	if _, err := l.Verify(); err != nil {
		t.Fatalf("Verify on an untouched log: %v", err)
	}

	seg1 := readFile(t, segPath(dir, 1))
	tampered := bytes.Clone(seg1)
	tampered[len(tampered)-1] ^= 0xFF // last byte of record 2's content
	writeFile(t, segPath(dir, 1), tampered)
	if _, err := l.Verify(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify after changing a sealed record = %v, want ErrCorrupt", err)
	}

	writeFile(t, segPath(dir, 1), seg1)
	s, _ := readSeal(dir, 1)
	s.first = 2
	writeSeal(dir, 1, s)
	if _, err := l.Verify(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify after changing a seal = %v, want ErrCorrupt", err)
	}
}

// TestVerifyDuringAppends: Verify checks the records that existed when it
// started, so it can run while other goroutines append (and roll over).
func TestVerifyDuringAppends(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := mustOpen(t, dir, small)
	defer l.Close()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 8 {
			if _, err := l.Append(record.TypeLot, []byte(big(string(rune('a'+i))))); err != nil {
				t.Error(err)
			}
		}
	})
	for range 20 {
		if _, err := l.Verify(); err != nil {
			t.Fatalf("Verify during appends: %v", err)
		}
	}
	wg.Wait()
	if _, err := l.Verify(); err != nil {
		t.Fatalf("Verify after appends: %v", err)
	}
}

// TestVerifyReturnsHead: the head is zeros for an empty log, then always
// the newest record's chain hash, so it changes with every append.
func TestVerifyReturnsHead(t *testing.T) {
	l, _, _ := mustOpen(t, t.TempDir(), Options{})
	defer l.Close()
	if head, err := l.Verify(); err != nil || head != ([32]byte{}) {
		t.Fatalf("empty log: head %x, err %v; want zeros", head, err)
	}
	mustAppend(t, l, "A")
	first, err := l.Verify()
	if err != nil || first != l.lastHash {
		t.Fatalf("head %x, err %v; want the newest record's chain hash", first, err)
	}
	mustAppend(t, l, "B")
	if second, _ := l.Verify(); second == first {
		t.Fatal("the head did not change after an append")
	}
}

// TestLimitsOfTheChain documents what the chain cannot catch from the files
// alone. Each case opens without error. What does change every time is the
// head: the chain hash of the newest record. Keeping the head somewhere
// else (v2: signed checkpoints sent to a witness) is what catches these.
func TestLimitsOfTheChain(t *testing.T) {
	for name, edit := range map[string]func(dir string){
		"a record edited and every later link rebuilt": func(dir string) {
			reencode(t, dir, 1, func(rs []*record.Record) []*record.Record {
				rs[1].Structure = []byte("forged")
				rs[2].PrevHash = rs[1].ChainHash()
				return rs
			})
		},
		"only the newest record edited": func(dir string) {
			reencode(t, dir, 1, func(rs []*record.Record) []*record.Record {
				rs[2].Structure = []byte("forged")
				return rs
			})
		},
		"the newest record removed": func(dir string) {
			reencode(t, dir, 1, func(rs []*record.Record) []*record.Record { return rs[:2] })
		},
	} {
		dir := t.TempDir()
		writeLog(t, dir, Options{}, "A", "B", "C")
		l, _, _ := mustOpen(t, dir, Options{})
		head := l.lastHash
		l.Close()

		edit(dir)
		l, _, err := Open(dir, Options{}, nil)
		if err != nil {
			t.Errorf("%s: Open = %v; this test expects the limit to still exist", name, err)
			continue
		}
		if l.lastHash == head {
			t.Errorf("%s: the head did not change, so even a kept head could not catch it", name)
		}
		l.Close()
	}
}
