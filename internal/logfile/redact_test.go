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

// filesContain reports whether any file in dir holds b.
func filesContain(t *testing.T, dir string, b []byte) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if bytes.Contains(readFile(t, filepath.Join(dir, e.Name())), b) {
			return true
		}
	}
	return false
}

// TestRedactErasesContentKeepsEverythingElse: after redacting records in a
// sealed segment and in the active one, their text is gone from every file,
// the other records are untouched, the head fingerprint and every seal are
// unchanged, and the log reopens and verifies.
func TestRedactErasesContentKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	data := []string{big("secret-one"), big("keep-two"), big("keep-three"), big("keep-four"), big("secret-five")}
	writeLog(t, dir, small, data...) // segments: [1 2] [3 4] [5]
	l, _, _ := mustOpen(t, dir, small)
	head, err := l.Verify()
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Redact([]uint64{1, 5, 5}); err != nil { // sealed and active; a duplicate is fine
		t.Fatal(err)
	}
	for _, s := range []string{"secret-one", "secret-five"} {
		if filesContain(t, dir, []byte(s)) {
			t.Errorf("%q is still on disk after redaction", s)
		}
	}
	for _, s := range []string{"keep-two", "keep-three", "keep-four"} {
		if !filesContain(t, dir, []byte(s)) {
			t.Errorf("%q was lost, but it was not redacted", s)
		}
	}
	for id := uint64(1); id <= 5; id++ {
		r, err := l.Read(id) // offsets after the redacted records moved
		if err != nil {
			t.Fatalf("Read(%d) after redaction: %v", id, err)
		}
		if want := id == 1 || id == 5; r.Redacted() != want {
			t.Errorf("record %d: redacted = %v, want %v", id, r.Redacted(), want)
		}
	}
	if got, err := l.Verify(); err != nil || got != head {
		t.Fatalf("Verify after redaction: head changed or err %v", err)
	}

	// Appends continue in the rewritten active segment.
	if id := mustAppend(t, l, "after"); id != 6 {
		t.Fatalf("Append after redaction = id %d, want 6", id)
	}
	l.Close()
	l, rep, recs := mustOpen(t, dir, small)
	defer l.Close()
	if rep.Records != 6 || !recs[0].Redacted() || !recs[4].Redacted() || string(recs[5].Data()) != "after" {
		t.Fatalf("after reopen: report %+v", rep)
	}
}

// TestRedactTwiceChangesNothing: redaction is idempotent; erasing an
// already-erased record leaves the file exactly as it was.
func TestRedactTwiceChangesNothing(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A", "B")
	l, _, _ := mustOpen(t, dir, Options{})
	defer l.Close()
	if err := l.Redact([]uint64{1}); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, segPath(dir, 1))
	if err := l.Redact([]uint64{1}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readFile(t, segPath(dir, 1))) {
		t.Fatal("redacting an already-redacted record changed the file")
	}
	if err := l.Redact([]uint64{9}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Redact of an unknown id = %v, want ErrNotFound", err)
	}
}

// TestCrashBeforeRenameLeavesOldSegment: a crash while the copy is being
// written leaves a .tmp file next to the untouched segment. Open removes it.
func TestCrashBeforeRenameLeavesOldSegment(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, Options{}, "A", "B")
	writeFile(t, segPath(dir, 1)+".tmp", []byte("half a copy"))

	l, rep, recs := mustOpen(t, dir, Options{})
	defer l.Close()
	if rep.Records != 2 || recs[0].Redacted() {
		t.Fatalf("report %+v: the original segment should be intact", rep)
	}
	if _, err := os.Stat(segPath(dir, 1) + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the leftover .tmp file was not removed")
	}
}

// TestRedactWhileReadingAndAppending: reads and appends running during a
// redaction never see a half-swapped file or a stale offset. Run with -race.
func TestRedactWhileReadingAndAppending(t *testing.T) {
	dir := t.TempDir()
	l, _, _ := mustOpen(t, dir, Options{})
	defer l.Close()
	for i := range 50 {
		mustAppend(t, l, string(rune('a'+i%26)))
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for id := uint64(1); id <= 50; id++ {
				if _, err := l.Read(id); err != nil {
					t.Error(err)
					return
				}
			}
		}
	})
	wg.Go(func() {
		for range 20 {
			if _, err := l.Append(record.TypeLot, []byte("more")); err != nil {
				t.Error(err)
			}
		}
	})
	for id := uint64(1); id <= 50; id += 7 {
		if err := l.Redact([]uint64{id}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if _, err := l.Verify(); err != nil {
		t.Fatal(err)
	}
}
