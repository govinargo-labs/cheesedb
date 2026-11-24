package server

import (
	"crypto/sha256"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/govinargo-labs/cheesedb"
)

// warmSet keeps one extra handle open per agent served recently.
//
// An agent's ledger closes when its last handle closes. Each request opens
// and closes its own handle, so without this an agent would be replayed and
// its index reloaded on every request: the very cost a server is for.
//
// Where it lives: the map m, in RAM only, keyed by the SHA-256 of the token
// (the server keeps no token after a request ends). Nothing is written to
// disk; when the process exits the set is gone and the next request opens
// the agent again.
//
// Closing a warm handle while requests are running is safe: their own
// handles keep the ledger open until they finish.
type warmSet struct {
	store *cheesedb.Store
	idle  time.Duration // close a handle unused this long
	max   int           // keep at most this many; close the least recently used
	now   func() time.Time

	mu     sync.Mutex
	m      map[[32]byte]*warm
	closed bool

	stop chan struct{} // closed by close: ends the idle loop
	done chan struct{} // closed when the idle loop has ended
}

type warm struct {
	a    *cheesedb.Agent
	used time.Time
}

func newWarmSet(s *cheesedb.Store, idle time.Duration, max int) *warmSet {
	return &warmSet{
		store: s, idle: idle, max: max, now: time.Now,
		m:    map[[32]byte]*warm{},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// start runs the idle loop: every tick, close what has been idle too long.
// This is the server's only background goroutine.
func (w *warmSet) start(every time.Duration) {
	go func() {
		defer close(w.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.sweep()
			case <-w.stop:
				return
			}
		}
	}()
}

// keep notes that a request used tok's agent, opening a warm handle for it
// if there is none. It is called while the request holds its own handle,
// so the open shares the loaded ledger and costs a map lookup.
func (w *warmSet) keep(tok cheesedb.Token) {
	key := sha256.Sum256(tok[:])
	w.mu.Lock()
	if e := w.m[key]; e != nil || w.closed {
		if e != nil {
			e.used = w.now()
		}
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	// Outside mu: OpenAgent takes the store's lock, and the warm set must
	// never hold up a request to another agent.
	a, err := w.store.OpenAgent(tok)
	if err != nil {
		return // the request goes on with its own handle; the next one retries
	}
	w.mu.Lock()
	if w.closed || w.m[key] != nil { // shutting down, or another request was first
		w.mu.Unlock()
		a.Close()
		return
	}
	w.m[key] = &warm{a: a, used: w.now()}
	var evict []*cheesedb.Agent
	if len(w.m) > w.max {
		evict = w.oldest(len(w.m) - w.max)
	}
	w.mu.Unlock()
	closeAll(evict)
}

// oldest removes and returns the n least recently used handles. mu held.
func (w *warmSet) oldest(n int) []*cheesedb.Agent {
	keys := make([][32]byte, 0, len(w.m))
	for k := range w.m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y [32]byte) int { return w.m[x].used.Compare(w.m[y].used) })
	var out []*cheesedb.Agent
	for _, k := range keys[:n] {
		out = append(out, w.m[k].a)
		delete(w.m, k)
	}
	return out
}

// sweep closes every handle unused for longer than idle.
func (w *warmSet) sweep() {
	var evict []*cheesedb.Agent
	w.mu.Lock()
	cut := w.now().Add(-w.idle)
	for k, e := range w.m {
		if e.used.Before(cut) {
			evict = append(evict, e.a)
			delete(w.m, k)
		}
	}
	w.mu.Unlock()
	closeAll(evict)
}

// drop closes tok's warm handle, if any, so the agent can be deleted.
func (w *warmSet) drop(tok cheesedb.Token) error {
	key := sha256.Sum256(tok[:])
	w.mu.Lock()
	e := w.m[key]
	delete(w.m, key)
	w.mu.Unlock()
	if e == nil {
		return nil
	}
	return e.a.Close()
}

func (w *warmSet) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.m)
}

// close ends the idle loop and closes every warm handle.
func (w *warmSet) close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	var all []*cheesedb.Agent
	for k, e := range w.m {
		all = append(all, e.a)
		delete(w.m, k)
	}
	w.mu.Unlock()
	close(w.stop)
	<-w.done
	return closeAll(all)
}

// closeAll closes handles outside any lock: closing an agent's last handle
// closes its ledger, which waits for its write in progress.
func closeAll(as []*cheesedb.Agent) error {
	var errs []error
	for _, a := range as {
		errs = append(errs, a.Close())
	}
	return errors.Join(errs...)
}
