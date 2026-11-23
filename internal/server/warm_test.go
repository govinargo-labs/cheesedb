package server

import (
	"errors"
	"io"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/govinargo-labs/cheesedb"
)

// clock is a time the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newWarmEnv is newEnv with a hand-moved clock. The idle loop ticks every
// minute at most, so in a test only sweep (called by hand) closes anything.
func newWarmEnv(t *testing.T, o Options) (*env, *clock) {
	e := newEnv(t, o)
	c := &clock{t: time.Unix(1_000_000, 0)}
	e.srv.warm.mu.Lock()
	e.srv.warm.now = c.now
	e.srv.warm.mu.Unlock()
	return e, c
}

// isOpen says whether the store still has the agent open: DeleteAgent
// refuses an open agent. A closed agent is deleted, so only ask once.
func isOpen(t *testing.T, e *env, tok string) bool {
	t.Helper()
	k, err := cheesedb.ParseToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	err = e.store.DeleteAgent(k)
	if err != nil && !errors.Is(err, cheesedb.ErrAgentInUse) {
		t.Fatal(err)
	}
	return err != nil
}

// The agent stays open after its request ends, so the next request finds
// its ledger (and indexes) loaded.
func TestAgentStaysOpen(t *testing.T) {
	e, _ := newWarmEnv(t, Options{})
	tok := e.agent("alice")
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"x"}`))
	if !isOpen(t, e, tok) {
		t.Fatal("agent closed when its request ended")
	}
}

func TestIdleAgentsClose(t *testing.T) {
	e, c := newWarmEnv(t, Options{IdleClose: 10 * time.Minute})
	alice, bob := e.agent("alice"), e.agent("bob")
	e.ok(e.req("GET", "/v1/memories", alice, ""))
	c.add(6 * time.Minute)
	e.ok(e.req("GET", "/v1/memories", bob, ""))
	c.add(6 * time.Minute) // Alice idle 12 minutes, Bob 6
	e.srv.warm.sweep()
	if e.srv.warm.len() != 1 {
		t.Fatalf("%d warm agents, want 1 (Bob)", e.srv.warm.len())
	}
	if isOpen(t, e, alice) {
		t.Fatal("idle Alice still open")
	}
	if !isOpen(t, e, bob) {
		t.Fatal("Bob closed before his idle time")
	}
}

// Beyond MaxOpenAgents, the least recently used agent is closed.
func TestLeastRecentlyUsedCloses(t *testing.T) {
	e, c := newWarmEnv(t, Options{MaxOpenAgents: 2})
	a, b, d := e.agent("a"), e.agent("b"), e.agent("d")
	e.ok(e.req("GET", "/v1/memories", a, ""))
	c.add(time.Second)
	e.ok(e.req("GET", "/v1/memories", b, ""))
	c.add(time.Second)
	e.ok(e.req("GET", "/v1/memories", a, "")) // a is now newer than b
	c.add(time.Second)
	e.ok(e.req("GET", "/v1/memories", d, ""))
	if e.srv.warm.len() != 2 {
		t.Fatalf("%d warm agents, want 2", e.srv.warm.len())
	}
	if isOpen(t, e, b) {
		t.Fatal("b, the least recently used, still open")
	}
	if !isOpen(t, e, a) || !isOpen(t, e, d) {
		t.Fatal("a recently used agent was closed")
	}
}

// Closing the warm handle while a request is running does not break the
// request: its own handle keeps the ledger open until it ends.
func TestWarmCloseDuringRequest(t *testing.T) {
	e, c := newWarmEnv(t, Options{IdleClose: time.Minute})
	tok := e.agent("alice")
	pr, pw := io.Pipe()
	r := httptest.NewRequest("POST", "http://"+host+"/v1/lots", pr)
	done := make(chan reply)
	go func() { done <- e.send(r, tok) }()
	waitFor(t, "the request to hold a warm handle", func() bool { return e.srv.warm.len() == 1 })

	c.add(time.Hour)
	e.srv.warm.sweep() // closes the warm handle mid-request
	if e.srv.warm.len() != 0 {
		t.Fatal("sweep left the warm handle")
	}
	pw.Write([]byte(`{"source_type":"doc","text":"still works"}`))
	pw.Close()
	e.ok(<-done)
	if isOpen(t, e, tok) {
		t.Fatal("agent left open after its last request and warm handle closed")
	}
}

// Close closes every warm handle, so the store can close and its folder
// can be opened again.
func TestCloseClosesWarm(t *testing.T) {
	e, _ := newWarmEnv(t, Options{})
	tok := e.agent("alice")
	e.ok(e.req("GET", "/v1/memories", tok, ""))
	if err := e.srv.Close(); err != nil {
		t.Fatal(err)
	}
	if isOpen(t, e, tok) {
		t.Fatal("agent open after Server.Close")
	}
	e.srv.warm.keep(mustToken(t, e.agent("bob"))) // after Close: keeps nothing
	if e.srv.warm.len() != 0 {
		t.Fatal("warm handle opened after Close")
	}
}

func mustToken(t *testing.T, s string) cheesedb.Token {
	t.Helper()
	k, err := cheesedb.ParseToken(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
