package logfile

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/record"
)

// Tests for Position and OpenAt: opening from a saved position must give
// the same log as a full replay, and refuse (ErrStale) any position that
// does not match the files, without changing them.

// positionOf opens dir, takes its position and closes it again.
func positionOf(t *testing.T, dir string, opts Options) *Position {
	t.Helper()
	l, _, _ := mustOpen(t, dir, opts)
	defer l.Close()
	p, err := l.Position()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// roundTrip encodes and decodes p, as the ledger's snapshot does.
func roundTrip(t *testing.T, p *Position) *Position {
	t.Helper()
	b := p.AppendEncoded([]byte("prefix"))
	q, n, err := DecodePosition(b[len("prefix"):])
	if err != nil {
		t.Fatal(err)
	}
	if n != len(b)-len("prefix") {
		t.Fatalf("DecodePosition used %d bytes of %d", n, len(b)-len("prefix"))
	}
	if !reflect.DeepEqual(p, q) {
		t.Fatalf("position changed in a round trip:\n%+v\n%+v", p, q)
	}
	return q
}

func openAt(dir string, opts Options, p *Position) (*Log, Report, []*record.Record, error) {
	var recs []*record.Record
	l, rep, err := OpenAt(dir, opts, p, func(r *record.Record) error {
		recs = append(recs, r)
		return nil
	})
	return l, rep, recs, err
}

// TestOpenAtMatchesOpen: a position that ends in the middle of segment 2,
// then more records, into segment 3. OpenAt hands over only the newer records and
// ends up with the same offsets, head and next id as a full replay.
func TestOpenAtMatchesOpen(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, small, big("a"), big("b"), big("c"), big("d"), "e")
	p := roundTrip(t, positionOf(t, dir, small))
	if p.Records != 5 || len(p.segs) != 2 {
		t.Fatalf("position covers %d records in %d segments, want 5 in 2", p.Records, len(p.segs))
	}
	l, _, _ := mustOpen(t, dir, small)
	mustAppend(t, l, "f")
	mustAppend(t, l, big("g")) // starts segment 3
	mustAppend(t, l, big("h"))
	l.Close()

	full, fullRep, _ := mustOpen(t, dir, small)
	defer full.Close()
	at, rep, recs, err := openAt(dir, small, p)
	if err != nil {
		t.Fatal(err)
	}
	defer at.Close()
	if len(recs) != 3 || recs[0].ID != 6 {
		t.Fatalf("OpenAt replayed %d records starting at %d, want 3 starting at 6", len(recs), recs[0].ID)
	}
	if rep != fullRep {
		t.Fatalf("Report = %+v, full replay %+v", rep, fullRep)
	}
	if !reflect.DeepEqual(at.offsets, full.offsets) || at.lastHash != full.lastHash || at.nextID != full.nextID || at.size != full.size {
		t.Fatal("OpenAt and Open disagree about the log")
	}
	if _, err := at.Verify(); err != nil {
		t.Fatal(err)
	}
	if got := mustAppend(t, at, "i"); got != 9 {
		t.Fatalf("next id %d, want 9", got)
	}
	for id := uint64(1); id <= 9; id++ {
		if _, err := at.Read(id); err != nil {
			t.Fatalf("Read(%d): %v", id, err)
		}
	}
}

// TestOpenAtTornTailAfterPosition: a crash left half a record after the
// position. OpenAt cuts it, as Open would.
func TestOpenAtTornTailAfterPosition(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "a", "b")
	p := positionOf(t, dir, Options{})
	f, err := os.OpenFile(segPath(dir, 1), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(encode(t, 3, p.Hash, "c")[:50])
	f.Close()

	l, rep, recs, err := openAt(dir, Options{}, p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if len(recs) != 0 || rep.DroppedBytes != 50 || rep.Records != 2 {
		t.Fatalf("got %d records, %+v; want none replayed, 50 bytes dropped, 2 records", len(recs), rep)
	}
}

// TestOpenAtRefusesStalePositions: each change to the files that a
// position cannot know about makes OpenAt refuse it with ErrStale and
// leave the files as they were.
func TestOpenAtRefusesStalePositions(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"log shorter than the position": func(t *testing.T, dir string) {
			if err := os.Truncate(segPath(dir, 2), fileSize(t, segPath(dir, 2))-10); err != nil {
				t.Fatal(err)
			}
		},
		"covered segment missing": func(t *testing.T, dir string) {
			os.Remove(segPath(dir, 2))
		},
		"sealed segment grew": func(t *testing.T, dir string) {
			f, _ := os.OpenFile(segPath(dir, 1), os.O_WRONLY|os.O_APPEND, 0)
			f.Write([]byte("junk"))
			f.Close()
		},
		"another log with the same shape": func(t *testing.T, dir string) {
			// Same data, so the same sizes and offsets, but new salts and
			// so other chain hashes.
			for _, n := range []int{1, 2} {
				os.Remove(segPath(dir, n))
				os.Remove(sealPath(dir, n))
			}
			writeLog(t, dir, small, big("a"), big("b"), big("c"))
		},
		"segment redacted": func(t *testing.T, dir string) {
			l, _, _ := mustOpen(t, dir, small)
			if err := l.Redact([]uint64{1}); err != nil {
				t.Fatal(err)
			}
			l.Close()
		},
		"newest record redacted": func(t *testing.T, dir string) {
			// It is the last record covered and it stays where it is, but
			// it is shorter now.
			l, _, _ := mustOpen(t, dir, small)
			if err := l.Redact([]uint64{3}); err != nil {
				t.Fatal(err)
			}
			l.Close()
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeLog(t, dir, small, big("a"), big("b"), big("c"))
			p := positionOf(t, dir, small)
			change(t, dir)
			before := dirContents(t, dir)
			if _, _, _, err := openAt(dir, small, p); !errors.Is(err, ErrStale) {
				t.Fatalf("OpenAt: %v, want ErrStale", err)
			}
			if !reflect.DeepEqual(dirContents(t, dir), before) {
				t.Fatal("a refused OpenAt changed the files")
			}
		})
	}
}

// TestOpenAtEmptyFolder: a position never creates a log where none is.
func TestOpenAtEmptyFolder(t *testing.T) {
	src := t.TempDir()
	writeLog(t, src, Options{}, "a")
	p := positionOf(t, src, Options{})
	dir := t.TempDir()
	if _, _, _, err := openAt(dir, Options{}, p); !errors.Is(err, ErrStale) {
		t.Fatalf("OpenAt: %v, want ErrStale", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("OpenAt created %d files", len(ents))
	}
}

// TestPositionOfEmptyLog: nothing to save.
func TestPositionOfEmptyLog(t *testing.T) {
	if p := positionOf(t, t.TempDir(), Options{}); p != nil {
		t.Fatalf("Position of an empty log = %+v, want nil", p)
	}
}

// TestDecodePositionRejectsDamage: every cut of an encoded position fails
// to decode, and so do counts that do not add up.
func TestDecodePositionRejectsDamage(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, small, big("a"), big("b"), big("c"), "d")
	b := positionOf(t, dir, small).AppendEncoded(nil)
	for cut := range len(b) {
		if _, _, err := DecodePosition(b[:cut]); !errors.Is(err, ErrStale) {
			t.Fatalf("cut at %d of %d: %v, want ErrStale", cut, len(b), err)
		}
	}
	bad := append([]byte(nil), b...)
	bad[0]++ // one record more than the segments hold
	if _, _, err := DecodePosition(bad); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong record count: %v, want ErrStale", err)
	}
}

func FuzzDecodePosition(f *testing.F) {
	dir := f.TempDir()
	l, _, err := Open(dir, small, nil)
	if err != nil {
		f.Fatal(err)
	}
	for _, d := range []string{big("a"), big("b"), big("c"), "d"} {
		l.Append(record.TypeLot, []byte(d))
	}
	p, _ := l.Position()
	l.Close()
	f.Add(p.AppendEncoded(nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, n, err := DecodePosition(b)
		if err != nil {
			return
		}
		if n > len(b) || uint64(len(p.offsets)) != p.Records {
			t.Fatalf("decoded %d bytes of %d, %d offsets for %d records", n, len(b), len(p.offsets), p.Records)
		}
	})
}

func sealPath(dir string, n int) string {
	return segPath(dir, n)[:len(segPath(dir, n))-len(".seg")] + ".seal"
}

// dirContents maps each file in dir to its bytes.
func dirContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range ents {
		m[e.Name()] = string(readFile(t, dir+"/"+e.Name()))
	}
	return m
}

// TestOpenAtRefusesAnotherLog: one segment, no records after the
// position, same sizes and offsets, so no seal and no later record can
// tell the two logs apart. Only the chain hashes of the first and last
// record do.
func TestOpenAtRefusesAnotherLog(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeLog(t, a, Options{}, "one", "two", "three")
	writeLog(t, b, Options{}, "one", "two", "three")
	if _, _, _, err := openAt(b, Options{}, positionOf(t, a, Options{})); !errors.Is(err, ErrStale) {
		t.Fatalf("OpenAt: %v, want ErrStale", err)
	}
}

// TestOpenAtRefusesBrokenLinkBetweenSegments: segment 2's first record no
// longer links to segment 1, but every size, seal and segment's last
// record is as saved. The first-record check is what catches it.
func TestOpenAtRefusesBrokenLinkBetweenSegments(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, small, big("a"), big("b"), big("c"), "d", "e")
	p := positionOf(t, dir, small)
	reencode(t, dir, 2, func(recs []*record.Record) []*record.Record {
		recs[0].PrevHash[0] ^= 1
		return recs
	})
	if _, _, _, err := openAt(dir, small, p); !errors.Is(err, ErrStale) {
		t.Fatalf("OpenAt: %v, want ErrStale", err)
	}
}
