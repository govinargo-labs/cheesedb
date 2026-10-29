package ledger

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

// Tests for erasing content (Recall with Erase, and Compact). Every
// check on "is the text gone" searches the actual files on disk.

func onDisk(t *testing.T, dir, text string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(text)) {
			return true
		}
	}
	return false
}

// The texts of the reference example, as buildRef writes them.
const (
	textA  = "I live in Pune"
	textB  = "profile page: lives in Pune"
	textC  = "Pune: rain"
	textM1 = "user lives in Pune"
	textM2 = "Pune is rainy"
	textM3 = "carry an umbrella"
)

func expectOnDisk(t *testing.T, dir string, gone, kept []string) {
	t.Helper()
	for _, s := range gone {
		if onDisk(t, dir, s) {
			t.Errorf("%q is still on disk; it should have been erased", s)
		}
	}
	for _, s := range kept {
		if !onDisk(t, dir, s) {
			t.Errorf("%q is gone from disk; it should have been kept", s)
		}
	}
}

// TestEraseRemovesText: Recall(C, Fair, Erase) erases C and the memories
// that died with it (M2, M3). Everything else keeps its text. The state
// after a reopen matches the live one exactly.
func TestEraseRemovesText(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	expectOnDisk(t, dir, nil, []string{textC, textM2, textM3}) // before: all there

	removed := must(l.Recall(r.c, Fair, Erase))
	if !reflect.DeepEqual(removed, []ID{r.m2, r.m3}) {
		t.Fatalf("removed %v", removed)
	}
	expectOnDisk(t, dir, []string{textC, textM2, textM3}, []string{textA, textB, textM1})

	if it := must(l.Get(r.m2)); !it.Redacted || it.Data != nil || it.Alive {
		t.Errorf("Get(M2) = %+v, want redacted, no data, dead", it)
	}
	if it := must(l.Get(r.m1)); it.Redacted || string(it.Data) != textM1 {
		t.Errorf("Get(M1) = %+v, want its text", it)
	}
	live := l.st
	l.Close()

	l2, rep, err := Open(dir, logfile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if rep.FinishedErase != 0 || !reflect.DeepEqual(l2.st, live) {
		t.Fatalf("after reopen: FinishedErase %d, state equal: %v", rep.FinishedErase, reflect.DeepEqual(l2.st, live))
	}
}

// TestEraseKeepsAliveMemories: erasing B with Fair leaves M1's text,
// because M1 is still learned from A.
func TestEraseKeepsAliveMemories(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	defer l.Close()
	r := buildRef(t, l)
	must(l.Recall(r.b, Fair, Erase))
	expectOnDisk(t, dir, []string{textB}, []string{textA, textC, textM1, textM2, textM3})
}

// TestStrictErase: with Strict, everything below B dies, so its text goes.
func TestStrictErase(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	defer l.Close()
	r := buildRef(t, l)
	must(l.Recall(r.b, Strict, Erase))
	expectOnDisk(t, dir, []string{textB, textM1, textM3}, []string{textA, textC, textM2})
}

// TestEraseReachesAlreadyDead: memories that died in an earlier Untrusted
// recall are erased by a later Erase recall of the same source.
func TestEraseReachesAlreadyDead(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	defer l.Close()
	r := buildRef(t, l)
	must(l.Recall(r.c, Fair, Untrusted))
	expectOnDisk(t, dir, nil, []string{textC, textM2, textM3}) // Untrusted keeps the text

	if removed := must(l.Recall(r.c, Fair, Erase)); len(removed) != 0 {
		t.Fatalf("second recall removed %v; they were already dead", removed)
	}
	expectOnDisk(t, dir, []string{textC, textM2, textM3}, []string{textA, textB, textM1})
}

// TestEraseAMemory: erasing a memory erases it and what died with it, not
// its sources.
func TestEraseAMemory(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	defer l.Close()
	r := buildRef(t, l)
	must(l.Recall(r.m1, Fair, Erase))
	expectOnDisk(t, dir, []string{textM1, textM3}, []string{textA, textB, textC, textM2})
}

// TestCompact tests compaction: it erases the content of
// every dead record, and the state is identical afterwards: same family
// tree, counters and liveness, same head fingerprint, and after a reopen.
func TestCompact(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	must(l.Recall(r.c, Fair, Untrusted))
	head := must(l.Verify())
	active := l.Active()

	if n := must(l.Compact()); n != 3 { // C, M2, M3
		t.Fatalf("Compact erased %d records, want 3", n)
	}
	expectOnDisk(t, dir, []string{textC, textM2, textM3}, []string{textA, textB, textM1})
	if got := must(l.Verify()); got != head {
		t.Fatal("the head fingerprint changed: compaction must not touch the chain")
	}
	if !reflect.DeepEqual(l.Active(), active) {
		t.Fatal("Active changed")
	}
	if n := must(l.Compact()); n != 0 {
		t.Fatalf("a second Compact erased %d, want 0", n)
	}
	live := l.st
	l.Close()

	l2 := mustOpen(t, dir)
	defer l2.Close()
	if !reflect.DeepEqual(l2.st, live) {
		t.Fatal("the state after reopen differs from before")
	}
}

// TestInterruptedEraseFinishedOnOpen: the RECALL record reached the disk but
// the crash came before the content was erased. Open finishes the job.
func TestInterruptedEraseFinishedOnOpen(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	l.Close()

	// Append the Erase recall straight to the log, as if the program died
	// right after it was saved.
	raw, _, err := logfile.Open(dir, logfile.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.AppendBatch([]logfile.Entry{{Type: record.TypeRecall, Structure: encodeRecall(r.c, Fair, Erase)}}); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	expectOnDisk(t, dir, nil, []string{textC, textM2, textM3})

	l2, rep, err := Open(dir, logfile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if rep.FinishedErase != 3 {
		t.Fatalf("FinishedErase = %d, want 3", rep.FinishedErase)
	}
	expectOnDisk(t, dir, []string{textC, textM2, textM3}, []string{textA, textB, textM1})
}
