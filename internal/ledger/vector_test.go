package ledger

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
	"github.com/govinargo-labs/cheesedb/internal/vecindex"
)

const testDims = 16

// vecFor returns a fixed, recognisable vector for id: its bytes on disk
// (after Normalize) can be searched for in the files.
func vecFor(id ID) []float32 {
	rng := rand.New(rand.NewPCG(uint64(id), 99))
	v := make([]float32, testDims)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	return v
}

// storedBytes is what Embed writes for v.
func storedBytes(v []float32) []byte {
	n, err := vecindex.Normalize(v, len(v))
	if err != nil {
		panic(err)
	}
	return vectorBytes(n)
}

// inTree reports whether b appears in any file under dir, subfolders included.
func inTree(t *testing.T, dir string, b []byte) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if bytes.Contains(data, b) {
			found = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// embedAll embeds every lot and memory of the reference example.
func embedAll(t *testing.T, l *Ledger, r refExample) {
	t.Helper()
	var items []Embedding
	for _, id := range []ID{r.a, r.b, r.c, r.m1, r.m2, r.m3} {
		items = append(items, Embedding{id, vecFor(id)})
	}
	if err := l.EmbedBatch("default", items); err != nil {
		t.Fatal(err)
	}
}

// searchAll returns every id the index finds, sorted.
func searchAll(t *testing.T, l *Ledger) []ID {
	t.Helper()
	hits, err := l.Search("default", vecFor(1), 100)
	if err != nil {
		t.Fatal(err)
	}
	var ids []ID
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	slices.Sort(ids)
	return ids
}

// rowsOf returns the targets the in-memory index holds, sorted. It looks
// inside, because Search also filters by liveness and would hide a leak.
func rowsOf(t *testing.T, l *Ledger) []ID {
	t.Helper()
	l.write.Lock()
	defer l.write.Unlock()
	ix := l.indexes["default"]
	if err := l.load(ix); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.idx.Search(storedVec(), 1<<20, ix.readVector)
	if err != nil {
		t.Fatal(err)
	}
	var ids []ID
	for _, h := range hits {
		ids = append(ids, ID(h.Target))
	}
	slices.Sort(ids)
	return ids
}

func storedVec() []float32 {
	v, _ := vecindex.Normalize(vecFor(1), testDims)
	return v
}

func newIndexed(t *testing.T) (*Ledger, string, refExample) {
	t.Helper()
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	if err := l.CreateIndex("default", testDims, "test-model"); err != nil {
		t.Fatal(err)
	}
	embedAll(t, l, r)
	return l, dir, r
}

func TestEmbedAndSearch(t *testing.T) {
	l, dir, r := newIndexed(t)
	hits := must(l.Search("default", vecFor(r.m2), 3))
	if len(hits) != 3 || hits[0].ID != r.m2 || hits[0].Score < 0.9999 {
		t.Fatalf("Search for M2's own vector = %v, want M2 first with score 1", hits)
	}

	// The same answers after a reopen: replay rebuilds the index.
	l.Close()
	l = mustOpen(t, dir)
	defer l.Close()
	if got := must(l.Search("default", vecFor(r.m2), 3)); !slices.Equal(got, hits) {
		t.Fatalf("after reopen: %v, want %v", got, hits)
	}
	info := l.Indexes()
	if len(info) != 1 || info[0].Name != "default" || info[0].Dims != testDims ||
		info[0].Label != "test-model" || info[0].Vectors != 6 || info[0].Err != nil {
		t.Fatalf("Indexes = %+v", info)
	}
	if _, err := l.VerifyIndex("default"); err != nil {
		t.Fatal(err)
	}
}

// TestEmbedRefuses: bad input is refused and nothing is written.
func TestEmbedRefuses(t *testing.T) {
	l, _, r := newIndexed(t)
	defer l.Close()
	must(l.Recall(r.c, Fair, Untrusted)) // kills C, M2, M3; the RECALL is id 7
	cases := map[string]struct {
		index string
		id    ID
		vec   []float32
		want  error
	}{
		"no such index":  {"nope", r.a, vecFor(1), ErrNoIndex},
		"wrong dims":     {"default", r.a, make([]float32, testDims+1), ErrBadVector},
		"all zero":       {"default", r.a, make([]float32, testDims), ErrBadVector},
		"unknown id":     {"default", 999, vecFor(1), ErrUnknownID},
		"id of a recall": {"default", 7, vecFor(1), ErrUnknownID},
		"dead lot":       {"default", r.c, vecFor(1), ErrRecalled},
		"dead memory":    {"default", r.m3, vecFor(1), ErrRecalled},
	}
	for name, c := range cases {
		if err := l.Embed(c.index, c.id, c.vec); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	// A batch with one bad item stores none of them.
	err := l.EmbedBatch("default", []Embedding{{r.a, vecFor(1)}, {r.m3, vecFor(2)}})
	if !errors.Is(err, ErrRecalled) {
		t.Fatalf("batch: got %v", err)
	}
	if got := searchAll(t, l); !slices.Equal(got, []ID{r.a, r.b, r.m1}) {
		t.Fatalf("searchable = %v", got)
	}
	if _, err := l.Search("default", vecFor(1), 0); !errors.Is(err, ErrBadArgument) {
		t.Fatalf("k=0: got %v", err)
	}
}

// TestRecallRemovesVectors: Fair keeps the vector of a memory that
// survives on another leg; Strict removes it. Checked inside the index, in
// memory and after a reopen.
func TestRecallRemovesVectors(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		want func(r refExample) []ID
	}{
		{Fair, func(r refExample) []ID { return []ID{r.b, r.c, r.m1, r.m2, r.m3} }},
		{Strict, func(r refExample) []ID { return []ID{r.b, r.c, r.m2} }},
	} {
		l, dir, r := newIndexed(t)
		must(l.Recall(r.a, tc.mode, Untrusted))
		if got := rowsOf(t, l); !slices.Equal(got, tc.want(r)) {
			t.Errorf("mode %d: rows %v, want %v", tc.mode, got, tc.want(r))
		}
		if got := searchAll(t, l); !slices.Equal(got, tc.want(r)) {
			t.Errorf("mode %d: search %v, want %v", tc.mode, got, tc.want(r))
		}
		l.Close()
		l = mustOpen(t, dir)
		if got := rowsOf(t, l); !slices.Equal(got, tc.want(r)) {
			t.Errorf("mode %d after reopen: rows %v, want %v", tc.mode, got, tc.want(r))
		}
		l.Close()
	}
}

// TestUntrustedKeepsBytesUntilCompact: an Untrusted recall hides vectors,
// Compact removes them from disk, like text. A replaced vector also goes.
func TestUntrustedKeepsBytesUntilCompact(t *testing.T) {
	l, dir, r := newIndexed(t)
	defer l.Close()
	must(l.Recall(r.c, Fair, Untrusted)) // C, M2, M3 die
	oldA := storedBytes(vecFor(r.a))
	if err := l.Embed("default", r.a, vecFor(1000)); err != nil { // replace A's vector
		t.Fatal(err)
	}
	for _, id := range []ID{r.c, r.m2, r.m3} {
		if !inTree(t, dir, storedBytes(vecFor(id))) {
			t.Fatalf("vector of %d left disk before Compact", id)
		}
	}
	if !inTree(t, dir, oldA) {
		t.Fatal("replaced vector left disk before Compact")
	}
	must(l.Compact())
	for _, id := range []ID{r.c, r.m2, r.m3} {
		if inTree(t, dir, storedBytes(vecFor(id))) {
			t.Errorf("vector of %d still on disk after Compact", id)
		}
	}
	if inTree(t, dir, oldA) {
		t.Error("replaced vector still on disk after Compact")
	}
	for _, id := range []ID{r.b, r.m1} {
		if !inTree(t, dir, storedBytes(vecFor(id))) {
			t.Errorf("live vector of %d was erased", id)
		}
	}
	if !inTree(t, dir, storedBytes(vecFor(1000))) {
		t.Error("the new vector of A was erased")
	}
	if _, err := l.VerifyIndex("default"); err != nil {
		t.Fatal(err)
	}
}

// TestEraseRemovesVectors is the end-to-end promise: after an Erase, neither
// the text nor the vector of anything erased is anywhere in the agent's
// folder, and both chains still verify. It runs with the index loaded and
// with the index not yet loaded (a fresh open).
func TestEraseRemovesVectors(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		l, dir, r := newIndexed(t)
		if reopen {
			l.Close()
			l = mustOpen(t, dir)
		}
		must(l.Recall(r.c, Fair, Erase)) // C, M2, M3 die and are erased
		for _, id := range []ID{r.c, r.m2, r.m3} {
			if inTree(t, dir, storedBytes(vecFor(id))) {
				t.Errorf("reopen=%v: vector of %d still on disk", reopen, id)
			}
		}
		for _, text := range []string{"Pune: rain", "Pune is rainy", "carry an umbrella"} {
			if inTree(t, dir, []byte(text)) {
				t.Errorf("reopen=%v: %q still on disk", reopen, text)
			}
		}
		for _, id := range []ID{r.a, r.b, r.m1} {
			if !inTree(t, dir, storedBytes(vecFor(id))) {
				t.Errorf("reopen=%v: live vector of %d was erased", reopen, id)
			}
		}
		if _, err := l.Verify(); err != nil {
			t.Fatal(err)
		}
		if _, err := l.VerifyIndex("default"); err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
}

// hideIndex runs fn on a ledger that cannot see the index, by moving the
// index folder away meanwhile. That writes to the main log exactly what a
// crash, or an older binary, would leave: the main log moved on, the index
// did not.
func hideIndex(t *testing.T, dir string, fn func(l *Ledger)) {
	t.Helper()
	ixDir := filepath.Join(dir, indexFolder("default"))
	away := filepath.Join(t.TempDir(), "away")
	if err := os.Rename(ixDir, away); err != nil {
		t.Fatal(err)
	}
	l := mustOpen(t, dir)
	fn(l)
	l.Close()
	if err := os.Rename(away, ixDir); err != nil {
		t.Fatal(err)
	}
}

// TestEraseCrashes stops an Erase between each pair of its steps and
// checks that opening again finishes it: no vector or text of an erased
// item is left on disk, without anyone searching.
//
//  1. RECALL written  2. vectors erased  3. text erased
func TestEraseCrashes(t *testing.T) {
	gone := func(t *testing.T, dir string, r refExample) {
		t.Helper()
		for _, id := range []ID{r.c, r.m2, r.m3} {
			if inTree(t, dir, storedBytes(vecFor(id))) {
				t.Errorf("vector of %d still on disk", id)
			}
		}
		if inTree(t, dir, []byte("Pune: rain")) {
			t.Error("text still on disk")
		}
	}

	t.Run("after RECALL, before anything erased", func(t *testing.T) {
		l, dir, r := newIndexed(t)
		l.Close()
		appendRaw(t, dir, logfile.Entry{Type: record.TypeRecall, Structure: encodeRecall(r.c, Fair, Erase)})
		l, rep, err := Open(dir, logfile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if rep.FinishedErase == 0 {
			t.Fatal("FinishedErase = 0")
		}
		gone(t, dir, r)
	})

	t.Run("after vectors erased, before text", func(t *testing.T) {
		l, dir, r := newIndexed(t)
		l.Close()
		// Erase the vectors by hand, then write the RECALL.
		ixDir := filepath.Join(dir, indexFolder("default"))
		lg, _, err := logfile.Open(ixDir, logfile.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Records 3, 5, 6 are the vectors of C, M2, M3 (embedAll order).
		if err := lg.Redact([]uint64{3, 5, 6}); err != nil {
			t.Fatal(err)
		}
		lg.Close()
		appendRaw(t, dir, logfile.Entry{Type: record.TypeRecall, Structure: encodeRecall(r.c, Fair, Erase)})
		l, rep, err := Open(dir, logfile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if rep.FinishedErase == 0 {
			t.Fatal("FinishedErase = 0")
		}
		gone(t, dir, r)
	})

	// Text erased but vectors not: what an older binary leaves, or a
	// broken index at the time of the erase. Nothing on the main log says
	// the erase is unfinished, so the first load of the index must do it.
	t.Run("text erased, vectors not", func(t *testing.T) {
		l, dir, r := newIndexed(t)
		l.Close()
		hideIndex(t, dir, func(l *Ledger) { must(l.Recall(r.c, Fair, Erase)) })
		l = mustOpen(t, dir)
		defer l.Close()
		if len(l.Indexes()) != 1 { // loads it
			t.Fatal("index lost")
		}
		gone(t, dir, r)
		if _, err := l.VerifyIndex("default"); err != nil {
			t.Fatal(err)
		}
	})
}

// TestEraseVectorsBeforeText: when erasing the vectors fails, the text
// must still be on disk. Then the main log alone shows the erase is
// unfinished, and the next Open finishes both, without anyone searching.
// (Text first would leave vectors behind that only a later load finds.)
func TestEraseVectorsBeforeText(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the process cannot write to")
	}
	l, dir, r := newIndexed(t)
	ixDir := filepath.Join(dir, indexFolder("default"))
	if err := os.Chmod(ixDir, 0o500); err != nil { // the rewrite cannot create its copy
		t.Fatal(err)
	}
	_, err := l.Recall(r.c, Fair, Erase)
	os.Chmod(ixDir, 0o700)
	if err == nil {
		t.Fatal("Recall did not report the failed vector erase")
	}
	if !inTree(t, dir, []byte("Pune: rain")) {
		t.Fatal("text erased although its vectors were not")
	}
	l.Close()

	l, rep, err := Open(dir, logfile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if rep.FinishedErase == 0 {
		t.Fatal("FinishedErase = 0")
	}
	if inTree(t, dir, []byte("Pune: rain")) || inTree(t, dir, storedBytes(vecFor(r.c))) {
		t.Fatal("erase not finished by Open")
	}
}

// appendRaw appends one record to the main log, bypassing the ledger.
func appendRaw(t *testing.T, dir string, e logfile.Entry) {
	t.Helper()
	lg, _, err := logfile.Open(dir, logfile.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	if _, err := lg.AppendBatch([]logfile.Entry{e}); err != nil {
		t.Fatal(err)
	}
}

// tagFor returns the tag a VECTOR record for id must carry, read from the
// main log in dir. Damage cases use it so that only the damage they mean
// is wrong, and each check is tested on its own.
func tagFor(t *testing.T, dir string, id ID) [tagSize]byte {
	t.Helper()
	lg, _, err := logfile.Open(dir, logfile.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	h := must(lg.Read(uint64(id))).ChainHash()
	var tag [tagSize]byte
	copy(tag[:], h[:])
	return tag
}

// appendIndex appends one record to the "default" index log, bypassing
// the ledger.
func appendIndex(t *testing.T, dir string, e logfile.Entry) {
	t.Helper()
	appendRaw(t, filepath.Join(dir, indexFolder("default")), e)
}

// TestVectorRecordInMainLog: a VECTOR record has no place in the main log.
func TestVectorRecordInMainLog(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	a := must(l.AddLot("user_message", []byte("hi")))
	l.Close()
	appendRaw(t, dir, logfile.Entry{Type: record.TypeVector, Structure: encodeVector(a, [tagSize]byte{}), Data: vectorBytes(vecFor(1))})
	if _, _, err := Open(dir, logfile.Options{}); !errors.Is(err, logfile.ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

// TestBrokenIndexIsolated damages an index in several ways. Every time the
// agent still opens and works; only that index fails; Erase and Compact
// are refused with nothing written; dropping the index clears it all.
func TestBrokenIndexIsolated(t *testing.T) {
	ixDirOf := func(dir string) string { return filepath.Join(dir, indexFolder("default")) }
	damage := map[string]func(t *testing.T, dir string){
		"meta missing": func(t *testing.T, dir string) {
			os.Remove(filepath.Join(ixDirOf(dir), metaFile))
		},
		"meta damaged": func(t *testing.T, dir string) {
			p := filepath.Join(ixDirOf(dir), metaFile)
			b, _ := os.ReadFile(p)
			b[1] ^= 0xff
			os.WriteFile(p, b, 0o600)
		},
		"vector for an unknown id": func(t *testing.T, dir string) {
			lg, _, err := logfile.Open(ixDirOf(dir), logfile.Options{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			lg.AppendBatch([]logfile.Entry{{Type: record.TypeVector, Structure: encodeVector(999, [tagSize]byte{}), Data: vectorBytes(vecFor(1))}})
			lg.Close()
		},
		"wrong vector length": func(t *testing.T, dir string) {
			lg, _, err := logfile.Open(ixDirOf(dir), logfile.Options{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			lg.AppendBatch([]logfile.Entry{{Type: record.TypeVector, Structure: encodeVector(1, tagFor(t, dir, 1)), Data: []byte{1, 2, 3}}})
			lg.Close()
		},
		"meta label damaged": func(t *testing.T, dir string) {
			p := filepath.Join(ixDirOf(dir), metaFile)
			b, _ := os.ReadFile(p)
			b[8] ^= 0xff // only the crc covers the label
			os.WriteFile(p, b, 0o600)
		},
		"vector for a RECALL": func(t *testing.T, dir string) {
			appendRaw(t, dir, logfile.Entry{Type: record.TypeRecall, Structure: encodeRecall(2, Fair, Untrusted)}) // id 7
			appendIndex(t, dir, logfile.Entry{Type: record.TypeVector, Structure: encodeVector(7, tagFor(t, dir, 7)), Data: storedBytes(vecFor(1))})
		},
		"NaN in a vector": func(t *testing.T, dir string) {
			nan := make([]float32, testDims)
			for i := range nan {
				nan[i] = float32(math.NaN())
			}
			appendIndex(t, dir, logfile.Entry{Type: record.TypeVector, Structure: encodeVector(1, tagFor(t, dir, 1)), Data: vectorBytes(nan)})
		},
		"vector not of length 1": func(t *testing.T, dir string) {
			appendIndex(t, dir, logfile.Entry{Type: record.TypeVector, Structure: encodeVector(1, tagFor(t, dir, 1)), Data: vectorBytes(vecFor(1))})
		},
		"vector for another history": func(t *testing.T, dir string) {
			appendIndex(t, dir, logfile.Entry{Type: record.TypeVector, Structure: encodeVector(1, [tagSize]byte{}), Data: storedBytes(vecFor(1))})
		},
		"a LOT record in the index": func(t *testing.T, dir string) {
			lg, _, err := logfile.Open(ixDirOf(dir), logfile.Options{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			lg.AppendBatch([]logfile.Entry{{Type: record.TypeLot, Structure: encodeLot("x"), Data: []byte("x")}})
			lg.Close()
		},
	}
	for name, fn := range damage {
		t.Run(name, func(t *testing.T) {
			l, dir, r := newIndexed(t)
			l.Close()
			fn(t, dir)

			l, _, err := Open(dir, logfile.Options{})
			if err != nil {
				t.Fatalf("agent did not open: %v", err)
			}
			defer l.Close()
			if _, err := l.Search("default", vecFor(1), 3); err == nil {
				t.Fatal("Search on a broken index worked")
			}
			if info := l.Indexes(); len(info) != 1 || info[0].Err == nil {
				t.Fatalf("Indexes = %+v, want the error listed", info)
			}
			// Everything that existed before indexes still works.
			x := must(l.AddLot("user_message", []byte("new")))
			must(l.Remember([]byte("from new"), Leg{x}))
			must(l.Recall(r.b, Fair, Untrusted))
			must(l.Get(r.a))
			head := must(l.Verify())

			// Erase and Compact are refused before writing anything.
			if _, err := l.Recall(r.c, Fair, Erase); err == nil {
				t.Fatal("Erase with a broken index worked")
			}
			if _, err := l.Compact(); err == nil {
				t.Fatal("Compact with a broken index worked")
			}
			if got := must(l.Verify()); got != head {
				t.Fatal("a refused Erase changed the main log")
			}
			if !must(l.Get(r.c)).Alive {
				t.Fatal("a refused Erase recalled")
			}

			if err := l.DropIndex("default"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(ixDirOf(dir)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("index folder still there: %v", err)
			}
			must(l.Recall(r.c, Fair, Erase))
			must(l.Compact())
		})
	}
}

// TestIndexDoesNotTouchMainLog: creating, using and dropping an index
// leaves every main-log file byte for byte the same, and the head too.
func TestIndexDoesNotTouchMainLog(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	head := must(l.Verify())
	before := mainFiles(t, dir)

	if err := l.CreateIndex("default", testDims, ""); err != nil {
		t.Fatal(err)
	}
	embedAll(t, l, r)
	must(l.Search("default", vecFor(1), 3))
	must(l.VerifyIndex("default"))
	l.Close()
	l = mustOpen(t, dir)
	l.Indexes()
	if err := l.DropIndex("default"); err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if got := must(l.Verify()); got != head {
		t.Fatal("head changed")
	}
	if after := mainFiles(t, dir); !maps.Equal(before, after) {
		t.Fatalf("main-log files changed:\nbefore %v\nafter  %v", before, after)
	}
}

// mainFiles hashes every file directly in dir (the main log; index
// folders are subfolders).
func mainFiles(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][32]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = sha256.Sum256(b)
	}
	return out
}

func TestCreateAndDropIndex(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	for name, c := range map[string]struct {
		name  string
		dims  int
		label string
	}{
		"empty name": {"", 4, ""},
		"long name":  {strings.Repeat("x", MaxIndexName+1), 4, ""},
		"zero dims":  {"a", 0, ""},
		"huge dims":  {"a", MaxDims + 1, ""},
		"long label": {"a", 4, strings.Repeat("x", MaxLabel+1)},
	} {
		if err := l.CreateIndex(c.name, c.dims, c.label); !errors.Is(err, ErrBadArgument) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// Tricky names stay inside the agent folder: they are hex-encoded.
	for _, name := range []string{"../x", "a/b", "A", "a", strings.Repeat("z", MaxIndexName)} {
		if err := l.CreateIndex(name, 4, "m"); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
	if err := l.CreateIndex("a", 4, ""); !errors.Is(err, ErrIndexExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if got := len(l.Indexes()); got != 5 {
		t.Fatalf("%d indexes, want 5", got)
	}

	// A leftover from a crash mid-create or mid-drop goes on the next Open.
	l.Close()
	leftover := filepath.Join(dir, indexTmpPrefix+"0123456789abcdef")
	os.Mkdir(leftover, 0o700)
	os.WriteFile(filepath.Join(leftover, "000001.seg"), []byte("x"), 0o600)
	l = mustOpen(t, dir)
	defer l.Close()
	if _, err := os.Stat(leftover); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("leftover not removed: %v", err)
	}
	if got := len(l.Indexes()); got != 5 {
		t.Fatalf("%d indexes after reopen, want 5", got)
	}
	if err := l.DropIndex("../x"); err != nil {
		t.Fatal(err)
	}
	if err := l.DropIndex("../x"); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("second drop: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), indexPrefix) {
			t.Errorf("unexpected folder %s", e.Name())
		}
	}
}

// TestIndexTornTail: a crash during an Embed leaves a torn tail in the
// index log. Loading cuts it, says so, and keeps every whole record.
func TestIndexTornTail(t *testing.T) {
	l, dir, r := newIndexed(t)
	l.Close()
	p := filepath.Join(dir, indexFolder("default"), "000001.seg")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, b[:100]...), 0o600) // half a record
	l = mustOpen(t, dir)
	defer l.Close()
	info := l.Indexes()[0]
	if info.Err != nil || info.Report.DroppedBytes != 100 || info.Vectors != 6 {
		t.Fatalf("Indexes = %+v", info)
	}
	if hits := must(l.Search("default", vecFor(r.m1), 1)); hits[0].ID != r.m1 {
		t.Fatalf("Search = %v", hits)
	}
}

// TestEmbedBatchSplits embeds more than one write can hold: the batch is
// split, and every vector arrives.
func TestEmbedBatchSplits(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	defer l.Close()
	const dims = 4096 // 16 KB per vector: a 1 MiB write holds about 63
	if err := l.CreateIndex("big", dims, ""); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 8))
	var items []Embedding
	for range 150 {
		v := make([]float32, dims)
		for i := range v {
			v[i] = float32(rng.NormFloat64())
		}
		items = append(items, Embedding{must(l.AddLot("doc", []byte("x"))), v})
	}
	if err := l.EmbedBatch("big", items); err != nil {
		t.Fatal(err)
	}
	for _, it := range items[:5] {
		if hits := must(l.Search("big", it.Vector, 1)); hits[0].ID != it.ID {
			t.Fatalf("Search for %d's vector found %v", it.ID, hits)
		}
	}
	if n := l.Indexes()[0].Vectors; n != 150 {
		t.Fatalf("%d vectors, want 150", n)
	}
}

// TestEmbedRecallRace runs Embed, Recall and Search on the same memories
// at once (run with -race). Afterwards no dead memory has a vector, in
// memory or after a replay.
func TestEmbedRecallRace(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	if err := l.CreateIndex("default", testDims, ""); err != nil {
		t.Fatal(err)
	}
	var lots, mems []ID
	for i := range 40 {
		lot := must(l.AddLot("doc", []byte{byte(i)}))
		lots = append(lots, lot)
		mems = append(mems, must(l.Remember([]byte{byte(i)}, Leg{lot})))
	}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < len(mems); i += 4 {
				// Errors are expected: the memory may already be dead.
				l.Embed("default", mems[i], vecFor(mems[i]))
				l.Embed("default", lots[i], vecFor(lots[i]))
				l.Search("default", vecFor(1), 5)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < len(lots); i += 2 {
			l.Recall(lots[i], Fair, Untrusted)
		}
	}()
	wg.Wait()

	check := func(when string) {
		for _, id := range rowsOf(t, l) {
			if must(l.Get(id)).Alive == false {
				t.Fatalf("%s: dead %d has a vector", when, id)
			}
		}
	}
	check("in memory")
	l.Close()
	l = mustOpen(t, dir)
	defer l.Close()
	check("after replay")
}
