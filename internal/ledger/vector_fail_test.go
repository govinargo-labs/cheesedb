package ledger

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
	"github.com/govinargo-labs/cheesedb/internal/vecindex"
)

// Failure paths of vector indexes: damaged or unwritable index folders,
// crashes, closes and races. Each test names what would go wrong without
// the code it covers.

// needPermissions skips a test that relies on folders the process cannot
// write to: Windows ignores those modes, and root ignores them everywhere.
func needPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the process cannot write to")
	}
}

// eraseGone fails if any text or vector of the reference example's C, M2,
// M3 (what a Fair Erase of C removes) is still anywhere under dir.
func eraseGone(t *testing.T, dir string, r refExample) {
	t.Helper()
	for _, id := range []ID{r.c, r.m2, r.m3} {
		if inTree(t, dir, storedBytes(vecFor(id))) {
			t.Errorf("vector of %d still on disk", id)
		}
	}
	if inTree(t, dir, []byte("Pune: rain")) {
		t.Error("text of C still on disk")
	}
}

// TestOpenWithBrokenIndexLeavesErasePending: a crash interrupted an Erase,
// and the index cannot be loaded at the next Open. Erasing the text alone
// would leave its vectors with nothing on the main log pointing to them,
// so the whole erase stays pending, the agent still opens, and the Report
// says so. Dropping the index and compacting finishes it.
func TestOpenWithBrokenIndexLeavesErasePending(t *testing.T) {
	l, dir, r := newIndexed(t)
	l.Close()
	appendRaw(t, dir, logfile.Entry{Type: record.TypeRecall, Structure: encodeRecall(r.c, Fair, Erase)})
	meta := filepath.Join(dir, indexFolder("default"), metaFile)
	b := must(os.ReadFile(meta))
	b[8] ^= 0xff // a label byte: only the crc notices
	if err := os.WriteFile(meta, b, 0o600); err != nil {
		t.Fatal(err)
	}

	l, rep, err := Open(dir, logfile.Options{})
	if err != nil {
		t.Fatalf("the agent did not open because of its index: %v", err)
	}
	if rep.UnfinishedErase != 3 || rep.FinishedErase != 0 || !errors.Is(rep.EraseErr, logfile.ErrCorrupt) {
		t.Fatalf("Report = %+v, want 3 unfinished, with the index's error", rep)
	}
	if !inTree(t, dir, []byte("Pune: rain")) {
		t.Fatal("text erased although its vectors could not be")
	}
	if must(l.Get(r.c)).Alive {
		t.Fatal("C is alive: the recall itself must stand")
	}
	if _, err := l.Compact(); err == nil {
		t.Fatal("Compact worked around the broken index")
	}
	if err := l.DropIndex("default"); err != nil {
		t.Fatal(err)
	}
	must(l.Compact())
	eraseGone(t, dir, r)
	l.Close()

	l, rep, err = Open(dir, logfile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if rep.UnfinishedErase != 0 || rep.FinishedErase != 0 {
		t.Fatalf("Report = %+v, want nothing pending", rep)
	}
}

// TestOpenWithUnwritableIndex: as above, but the index loads and cannot be
// rewritten. The agent opens, nothing is erased, and the text stays (that
// is the vectors-before-text order of Open). Once the index can be written
// again, Compact finishes the erase.
func TestOpenWithUnwritableIndex(t *testing.T) {
	needPermissions(t)
	l, dir, r := newIndexed(t)
	l.Close()
	appendRaw(t, dir, logfile.Entry{Type: record.TypeRecall, Structure: encodeRecall(r.c, Fair, Erase)})
	ixDir := filepath.Join(dir, indexFolder("default"))
	if err := os.Chmod(ixDir, 0o500); err != nil { // the rewrite cannot create its copy
		t.Fatal(err)
	}
	l, rep, err := Open(dir, logfile.Options{})
	os.Chmod(ixDir, 0o700)
	if err != nil {
		t.Fatalf("the agent did not open because of its index: %v", err)
	}
	defer l.Close()
	if rep.UnfinishedErase != 3 || rep.EraseErr == nil || rep.FinishedErase != 0 {
		t.Fatalf("Report = %+v, want 3 unfinished", rep)
	}
	if !inTree(t, dir, []byte("Pune: rain")) {
		t.Fatal("text erased although its vectors could not be")
	}
	must(l.Compact())
	eraseGone(t, dir, r)
}

// TestCompactVectorsBeforeText: when Compact cannot erase the vectors, it
// must not erase the text either, for the same reason as an Erase.
func TestCompactVectorsBeforeText(t *testing.T) {
	needPermissions(t)
	l, dir, r := newIndexed(t)
	defer l.Close()
	must(l.Recall(r.c, Fair, Untrusted))
	ixDir := filepath.Join(dir, indexFolder("default"))
	if err := os.Chmod(ixDir, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := l.Compact()
	os.Chmod(ixDir, 0o700)
	if err == nil {
		t.Fatal("Compact did not report the failed vector erase")
	}
	if !inTree(t, dir, []byte("Pune: rain")) {
		t.Fatal("text compacted although its vectors were not")
	}
	must(l.Compact())
	eraseGone(t, dir, r)
}

// TestCreateDropIndexDirSyncFails: the rename worked, the folder fsync
// after it failed. The list of indexes must still match the disk, or the
// index is stuck: listed but missing (drop and Erase fail) or present but
// unlisted (create says it exists, use says it does not).
func TestCreateDropIndexDirSyncFails(t *testing.T) {
	needPermissions(t)
	// 0300: names can be added and removed, but the folder cannot be
	// opened for reading, which its fsync needs.
	t.Run("drop", func(t *testing.T) {
		l, dir, r := newIndexed(t)
		defer l.Close()
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		err := l.DropIndex("default")
		os.Chmod(dir, 0o700)
		if err == nil {
			t.Skip("the folder fsync did not fail on this system")
		}
		if got := l.Indexes(); len(got) != 0 {
			t.Fatalf("Indexes = %+v after its folder was renamed away", got)
		}
		must(l.Recall(r.c, Fair, Erase))
		if err := l.CreateIndex("default", testDims, ""); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("create", func(t *testing.T) {
		dir := t.TempDir()
		l := mustOpen(t, dir)
		defer l.Close()
		a := must(l.AddLot("user_message", []byte("hi")))
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		err := l.CreateIndex("default", testDims, "")
		os.Chmod(dir, 0o700)
		if err == nil {
			t.Skip("the folder fsync did not fail on this system")
		}
		if err := l.Embed("default", a, vecFor(a)); err != nil {
			t.Fatalf("Embed into the index whose folder is in place: %v", err)
		}
		if err := l.DropIndex("default"); err != nil {
			t.Fatal(err)
		}
	})
}

// TestDropIndexLeftover: DropIndex renamed the folder away but could not
// delete it. The index is gone from the list (and stays gone after a
// reopen), but its vectors are still on disk, so an Erase is refused until
// the leftover can be removed, and then removes it.
func TestDropIndexLeftover(t *testing.T) {
	needPermissions(t)
	l, dir, r := newIndexed(t)
	stuck := filepath.Join(dir, indexFolder("default"), "stuck")
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil { // its file cannot be removed
		t.Fatal(err)
	}
	unstick := func() {
		found, _ := filepath.Glob(filepath.Join(dir, indexTmpPrefix+"*", "stuck"))
		for _, p := range append(found, stuck) {
			os.Chmod(p, 0o700)
		}
	}
	defer unstick()

	if err := l.DropIndex("default"); err == nil {
		t.Fatal("DropIndex did not report the folder it could not delete")
	}
	if got := l.Indexes(); len(got) != 0 {
		t.Fatalf("Indexes = %+v after DropIndex", got)
	}
	head := must(l.Verify())
	if _, err := l.Recall(r.c, Fair, Erase); err == nil {
		t.Fatal("Erase worked while a dropped index still holds vectors")
	}
	if must(l.Verify()) != head {
		t.Fatal("the refused Erase wrote to the main log")
	}
	l.Close()
	l = mustOpen(t, dir)
	defer l.Close()
	if got := l.Indexes(); len(got) != 0 {
		t.Fatalf("Indexes = %+v after reopen: the dropped index came back", got)
	}

	unstick()
	must(l.Recall(r.c, Fair, Erase))
	eraseGone(t, dir, r)
	if found, _ := filepath.Glob(filepath.Join(dir, indexTmpPrefix+"*")); len(found) != 0 {
		t.Fatalf("leftover not removed: %v", found)
	}
}

// TestIndexChangesAfterClose: nothing touches the disk after Close, and
// Close closes every index log.
func TestIndexChangesAfterClose(t *testing.T) {
	l, dir, r := newIndexed(t)
	ix := l.indexes["default"]
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if ix.log != nil || ix.idx != nil {
		t.Fatal("Close left the index loaded")
	}
	if err := l.CreateIndex("late", 4, ""); !errors.Is(err, logfile.ErrClosed) {
		t.Errorf("CreateIndex after Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, indexFolder("late"))); !errors.Is(err, fs.ErrNotExist) {
		t.Error("CreateIndex after Close created a folder")
	}
	if err := l.DropIndex("default"); !errors.Is(err, logfile.ErrClosed) {
		t.Errorf("DropIndex after Close: %v", err)
	}
	if _, err := os.Stat(ix.dir); err != nil {
		t.Errorf("DropIndex after Close deleted the index: %v", err)
	}
	if err := l.Embed("default", r.a, vecFor(r.a)); !errors.Is(err, logfile.ErrClosed) {
		t.Errorf("Embed after Close: %v", err)
	}
	if _, err := l.Search("default", vecFor(r.a), 1); !errors.Is(err, logfile.ErrClosed) {
		t.Errorf("Search after Close: %v", err)
	}
}

// TestRestoredMainLog: the main log is restored from an older copy and the
// index is not. The next lot gets the id of a lost one; an index that
// checked ids only would hand it the lost item's vector. The tag (the
// start of the target's chain hash) catches it: the index fails to load.
func TestRestoredMainLog(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	a := must(l.AddLot("user_message", []byte("first")))
	if err := l.CreateIndex("default", testDims, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Embed("default", a, vecFor(a)); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(dir, "000001.seg")
	backup := must(os.ReadFile(seg))
	secret := must(l.AddLot("user_message", []byte("old secret")))
	if err := l.Embed("default", secret, vecFor(100)); err != nil {
		t.Fatal(err)
	}
	l.Close()

	if err := os.WriteFile(seg, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	l = mustOpen(t, dir)
	defer l.Close()
	if other := must(l.AddLot("user_message", []byte("something unrelated"))); other != secret {
		t.Fatalf("new lot is %d, want the lost id %d", other, secret)
	}
	if hits, err := l.Search("default", vecFor(100), 1); !errors.Is(err, logfile.ErrCorrupt) {
		t.Fatalf("Search = %v, %v; want ErrCorrupt", hits, err)
	}
	if _, err := l.Recall(a, Fair, Erase); err == nil {
		t.Fatal("Erase worked around an index from another history")
	}
}

// TestSearchSkipsDeadRows: a Recall marks the dead in the state first and
// removes their rows a moment later. A search in between must not return
// them. The test makes that window permanent (a node marked dead, its row
// never removed), which also shows Search gives up retrying.
func TestSearchSkipsDeadRows(t *testing.T) {
	l, _, r := newIndexed(t)
	defer l.Close()
	l.mu.Lock()
	l.st.node(r.m2).dead = true
	l.mu.Unlock()
	for _, k := range []int{1, 100} {
		for _, h := range must(l.Search("default", vecFor(r.m2), k)) {
			if h.ID == r.m2 {
				t.Fatalf("k=%d: Search returned dead %d", k, r.m2)
			}
		}
	}
}

// TestSearchWaitsForRecall: a search that meets a row dying mid-recall
// waits for that recall and searches again, so it still returns k live
// results. The test plays the recall by hand: it holds the write lock,
// marks M2 dead, and removes its row only after the search has (most
// likely) met it. If the search runs late, the test passes without
// proving the retry; it can never fail falsely.
func TestSearchWaitsForRecall(t *testing.T) {
	l, _, r := newIndexed(t)
	defer l.Close()
	l.write.Lock()
	l.mu.Lock()
	l.st.node(r.m2).dead = true
	l.mu.Unlock()
	got := make(chan []Hit)
	go func() { got <- must(l.Search("default", vecFor(r.m2), 1)) }()
	time.Sleep(50 * time.Millisecond)
	l.forgetVectors(r.m2, nil)
	l.write.Unlock()
	if hits := <-got; len(hits) != 1 || hits[0].ID == r.m2 {
		t.Fatalf("Search = %v, want one live hit", hits)
	}
}

// TestReplacedVectorAfterReopen: a vector replaced by a newer one, then a
// reopen. Loading must list the old record as stale, or Compact would
// leave it on disk forever.
func TestReplacedVectorAfterReopen(t *testing.T) {
	l, dir, r := newIndexed(t)
	if err := l.Embed("default", r.a, vecFor(100)); err != nil {
		t.Fatal(err)
	}
	l.Close()
	l = mustOpen(t, dir)
	defer l.Close()
	must(l.Compact())
	if inTree(t, dir, storedBytes(vecFor(r.a))) {
		t.Error("replaced vector still on disk after Compact")
	}
	if !inTree(t, dir, storedBytes(vecFor(100))) {
		t.Error("Compact erased the current vector")
	}
	if hits := must(l.Search("default", vecFor(100), 1)); hits[0].ID != r.a {
		t.Fatalf("Search = %v", hits)
	}
}

// TestLoadAfterDrop: a load that finds its index dropped meanwhile says so,
// and does not bring the folder back (opening a log creates its folder).
func TestLoadAfterDrop(t *testing.T) {
	l, _, _ := newIndexed(t)
	defer l.Close()
	ix := l.indexes["default"]
	if err := l.DropIndex("default"); err != nil {
		t.Fatal(err)
	}
	l.write.Lock()
	err := l.load(ix)
	l.write.Unlock()
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("load of a dropped index: %v", err)
	}
	if _, err := os.Stat(ix.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("load brought the dropped folder back")
	}
}

// TestIndexFolderNames: only a folder named exactly as CreateIndex names it
// is an index. "index-4A" decodes to "J", whose folder is "index-4a"; if
// both counted, one name could have two folders.
func TestIndexFolderNames(t *testing.T) {
	l, dir, _ := newIndexed(t)
	l.Close()
	for _, name := range []string{"index-4A", "index-zz", "index-"} {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeMeta(p, testDims, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, indexFolder("file")), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l = mustOpen(t, dir)
	defer l.Close()
	var names []string
	for _, in := range l.Indexes() {
		names = append(names, in.Name)
	}
	if !slices.Equal(names, []string{"default"}) {
		t.Fatalf("indexes %q, want only \"default\"", names)
	}
}

// TestMultiSegmentIndex: an index log that spans several segments, with the
// main log's segment size. An Erase rewrites a sealed segment; a reopen
// replays them all; damage in a sealed segment is never cut as a torn
// tail: the index fails to load and the agent still opens.
func TestMultiSegmentIndex(t *testing.T) {
	dir := t.TempDir()
	opts := logfile.Options{SegmentSize: logfile.MinSegmentSize}
	l, _, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	const dims = 4096 // 16 KB per vector: about 63 per segment
	if err := l.CreateIndex("big", dims, ""); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(9, 10))
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
	ixDir := filepath.Join(dir, indexFolder("big"))
	if segs, _ := filepath.Glob(filepath.Join(ixDir, "*.seg")); len(segs) < 3 {
		t.Fatalf("%d segments, want at least 3", len(segs))
	}
	first := vectorBytes(must(vecindex.Normalize(items[0].Vector, dims)))
	must(l.Recall(items[0].ID, Fair, Erase)) // its vector is in sealed segment 1
	if inTree(t, dir, first) {
		t.Fatal("erased vector still in its sealed segment")
	}
	l.Close()

	l, _, err = Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if info := l.Indexes()[0]; info.Err != nil || info.Vectors != 149 {
		t.Fatalf("Indexes = %+v", info)
	}
	must(l.VerifyIndex("big"))
	l.Close()

	p := filepath.Join(ixDir, "000001.seg")
	b := must(os.ReadFile(p))
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	l, _, err = Open(dir, opts)
	if err != nil {
		t.Fatalf("the agent did not open because of its index: %v", err)
	}
	defer l.Close()
	if info := l.Indexes()[0]; !errors.Is(info.Err, logfile.ErrCorrupt) {
		t.Fatalf("Indexes = %+v, want ErrCorrupt", info)
	}
}

// TestIndexRacesDrop runs Search and VerifyIndex while the index is
// dropped and created again and again (run with -race). They may find no
// index, but never fail with anything else, such as a file vanishing
// under them.
func TestIndexRacesDrop(t *testing.T) {
	l, _, r := newIndexed(t)
	defer l.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	check := func(what string, err error) {
		if err != nil && !errors.Is(err, ErrNoIndex) {
			t.Errorf("%s: %v", what, err)
		}
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := l.Search("default", vecFor(r.a), 3)
				check("Search", err)
				_, err = l.VerifyIndex("default")
				check("VerifyIndex", err)
			}
		}()
	}
	for range 30 {
		if err := l.DropIndex("default"); err != nil {
			t.Fatal(err)
		}
		if err := l.CreateIndex("default", testDims, ""); err != nil {
			t.Fatal(err)
		}
		embedAll(t, l, r)
	}
	close(stop)
	wg.Wait()
}
