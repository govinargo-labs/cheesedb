//go:build unix

package ledger

import (
	"errors"
	"os/signal"
	"syscall"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
)

// limitFileSize makes every write past limit bytes into any file fail, the
// way a full disk does: the bytes that fit are written, the rest is
// refused. It is process-wide, so it suits only tests that do not run in
// parallel (none in this package do).
func limitFileSize(t *testing.T, limit uint64) (restore func()) {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ) // report EFBIG instead of killing the process
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: limit, Max: old.Max}); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
			t.Fatal(err)
		}
		signal.Reset(syscall.SIGXFSZ)
	}
}

// TestFailedEmbedThenErase: an EmbedBatch whose write stops part-way leaves
// a whole record on disk that nobody was told about, and a log that
// refuses every write. The index must be read again before its next use,
// or a later Erase would report success while that vector stays on disk,
// and an Erase of anything with a vector would fail half done.
func TestFailedEmbedThenErase(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	defer l.Close()
	r := buildRef(t, l)
	if err := l.CreateIndex("default", testDims, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Embed("default", r.c, vecFor(r.c)); err != nil {
		t.Fatal(err)
	}
	// One VECTOR record of 16 values takes 247 bytes, so a limit of 300
	// more than the record already there lets A's through, not B's.
	restore := limitFileSize(t, 247+300)
	err := l.EmbedBatch("default", []Embedding{{r.a, vecFor(r.a)}, {r.b, vecFor(r.b)}})
	restore()
	if !errors.Is(err, logfile.ErrBroken) {
		t.Fatalf("EmbedBatch = %v, want ErrBroken", err)
	}
	if !inTree(t, dir, storedBytes(vecFor(r.a))) {
		t.Fatal("A's record did not reach the disk; the test sets nothing up")
	}

	if info := l.Indexes(); len(info) != 1 || info[0].Err != nil || info[0].Report.DroppedBytes == 0 {
		t.Fatalf("Indexes = %+v, want the index read again, its torn tail cut", info)
	}
	must(l.Recall(r.a, Fair, Erase)) // A's vector: the one nobody was told about
	if inTree(t, dir, storedBytes(vecFor(r.a))) {
		t.Error("Erase of A returned, but A's vector is still on disk")
	}
	must(l.Recall(r.c, Fair, Erase)) // C's vector: written before the failure
	eraseGone(t, dir, r)
	if err := l.Embed("default", r.b, vecFor(r.b)); err != nil {
		t.Fatalf("Embed after the failure: %v", err)
	}
}
