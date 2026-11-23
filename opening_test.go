package cheesedb

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/govinargo-labs/cheesedb/internal/ledger"
	"github.com/govinargo-labs/cheesedb/internal/logfile"
)

// replayGate plays a slow replay of one agent: each replay of its folder
// says so on started, then waits until release is closed. If fail is set
// by then, the replay fails with it instead of opening the ledger.
type replayGate struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	fail    error
	calls   atomic.Int32
	waiters atomic.Int32 // OpenAgent calls waiting for a replay of this store
}

// open lets the gated replays finish. Safe to call twice.
func (g *replayGate) open() { g.once.Do(func() { close(g.release) }) }

func gateReplay(t *testing.T, s *Store, name string) *replayGate {
	t.Helper()
	folder := filepath.Join(s.dir, hex.EncodeToString([]byte(name)))
	g := &replayGate{started: make(chan struct{}, 10), release: make(chan struct{})}
	orig, origWait := openLedger, replayWait
	replayWait = func() { g.waiters.Add(1) }
	openLedger = func(dir string, o logfile.Options) (*ledger.Ledger, logfile.Report, error) {
		if dir != folder {
			return orig(dir, o)
		}
		g.calls.Add(1)
		g.started <- struct{}{}
		<-g.release
		if g.fail != nil {
			return nil, logfile.Report{}, g.fail
		}
		return orig(dir, o)
	}
	// Runs before the store's Close (cleanups run last first), so a failed
	// test reports its failure instead of hanging on a gated replay.
	t.Cleanup(func() { g.open(); openLedger, replayWait = orig, origWait })
	return g
}

// waitWaiters returns once n opens are waiting for the gated replay.
func (g *replayGate) waitWaiters(t *testing.T, n int32) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); g.waiters.Load() < n; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%d of %d opens waiting for the replay", g.waiters.Load(), n)
		}
	}
}

// within fails the test if f does not return within a generous limit: the
// calls it runs must not wait for a replay that never ends.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s waited for another agent's replay", what)
	}
}

type opened struct {
	a   *Agent
	err error
}

func openAsync(s *Store, tok Token) chan opened {
	c := make(chan opened, 1)
	go func() {
		a, err := s.OpenAgent(tok)
		c <- opened{a, err}
	}()
	return c
}

// While Bob's log is replayed, other agents open, work, close and are
// created as usual: the replay does not hold the store's lock.
func TestReplayDoesNotBlockOtherAgents(t *testing.T) {
	s := mustStore(t, filepath.Join(t.TempDir(), "store"))
	alice := must(s.CreateAgent("alice"))
	bob := must(s.CreateAgent("bob"))
	g := gateReplay(t, s, "bob")

	b := openAsync(s, bob)
	<-g.started // Bob's replay is running and will not end until released
	within(t, "Alice", func() {
		a := must(s.OpenAgent(alice))
		must(a.AddLot("note", []byte("while Bob opens")))
		if err := a.Close(); err != nil {
			t.Error(err)
		}
		must(s.CreateAgent("carol"))
	})

	g.open()
	r := <-b
	if r.err != nil {
		t.Fatal(r.err)
	}
	must(r.a.AddLot("note", []byte("Bob is open")))
	r.a.Close()
}

// Opens of the same agent during its replay wait for it and share its
// ledger: the log is replayed once, so there is never a second writer.
func TestSameAgentReplaysOnce(t *testing.T) {
	s := mustStore(t, filepath.Join(t.TempDir(), "store"))
	bob := must(s.CreateAgent("bob"))
	g := gateReplay(t, s, "bob")

	var cs []chan opened
	for range 3 {
		cs = append(cs, openAsync(s, bob))
	}
	<-g.started
	g.waitWaiters(t, 2) // the other two wait for the replay, not start their own
	g.open()

	var hs []*Agent
	for _, c := range cs {
		r := <-c
		if r.err != nil {
			t.Fatal(r.err)
		}
		hs = append(hs, r.a)
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("replayed %d times, want once", n)
	}
	id := must(hs[0].AddLot("note", []byte("seen by all")))
	if it := must(hs[2].Get(id)); string(it.Data) != "seen by all" {
		t.Fatalf("handles do not share one ledger: %q", it.Data)
	}
	for _, h := range hs {
		h.Close()
	}
}

// A failed replay gives its error to every open waiting for it, and
// leaves nothing behind: the agent is not stuck as opening.
func TestReplayErrorReachesWaiters(t *testing.T) {
	s := mustStore(t, filepath.Join(t.TempDir(), "store"))
	bob := must(s.CreateAgent("bob"))
	g := gateReplay(t, s, "bob")
	g.fail = errors.New("disk on fire")

	c1, c2 := openAsync(s, bob), openAsync(s, bob)
	<-g.started
	g.waitWaiters(t, 1)
	g.open()
	for _, c := range []chan opened{c1, c2} {
		if r := <-c; !errors.Is(r.err, g.fail) {
			t.Fatalf("OpenAgent = %v, want the replay's error", r.err)
		}
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("replayed %d times, want once: the waiter must get the first replay's error", n)
	}
	s.mu.Lock()
	left := len(s.opening)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d agents still listed as opening", left)
	}
	if err := s.DeleteAgent(bob); err != nil {
		t.Fatalf("DeleteAgent after a failed open: %v", err)
	}
}

// An agent being replayed cannot be deleted, and the refusal comes at
// once instead of after the replay.
func TestDeleteRefusedWhileReplaying(t *testing.T) {
	s := mustStore(t, filepath.Join(t.TempDir(), "store"))
	bob := must(s.CreateAgent("bob"))
	g := gateReplay(t, s, "bob")

	b := openAsync(s, bob)
	<-g.started
	within(t, "DeleteAgent", func() {
		if err := s.DeleteAgent(bob); !errors.Is(err, ErrAgentInUse) {
			t.Errorf("DeleteAgent while replaying: %v, want ErrAgentInUse", err)
		}
	})
	g.open()
	r := <-b
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.a.Close()
	if err := s.DeleteAgent(bob); err != nil {
		t.Fatalf("DeleteAgent after the open: %v", err)
	}
}

// Close waits for a replay in progress, which then closes its own ledger,
// so the folder is released with no file of it still open.
func TestCloseWaitsForReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s := mustStore(t, dir)
	bob := must(s.CreateAgent("bob"))
	a := must(s.OpenAgent(bob))
	must(a.AddLot("note", []byte("before")))
	a.Close()
	g := gateReplay(t, s, "bob")

	b := openAsync(s, bob)
	<-g.started
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a replay was still running")
	case <-time.After(50 * time.Millisecond):
	}
	g.open()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if r := <-b; !errors.Is(r.err, ErrClosed) {
		t.Fatalf("OpenAgent during Close = %v, want ErrClosed", r.err)
	}

	// The folder is free and whole: open it again and read Bob back.
	s2 := mustStore(t, dir)
	a2 := must(s2.OpenAgent(bob))
	defer a2.Close()
	if it := must(a2.Get(1)); string(it.Data) != "before" {
		t.Fatalf("after reopen: %q", it.Data)
	}
}

// A second Close while the first waits for a replay returns only after
// the first has finished, so the folder is free when either returns.
func TestSecondCloseWaitsForFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s := mustStore(t, dir)
	bob := must(s.CreateAgent("bob"))
	g := gateReplay(t, s, "bob")
	b := openAsync(s, bob)
	<-g.started

	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- s.Close() }()
	time.Sleep(20 * time.Millisecond) // the first Close is waiting for the replay
	go func() { second <- s.Close() }()
	select {
	case <-second:
		t.Fatal("second Close returned while the first was still waiting")
	case <-time.After(50 * time.Millisecond):
	}
	g.open()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	<-b
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("folder not free after both Close calls: %v", err)
	}
	s2.Close()
}
