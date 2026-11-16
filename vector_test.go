package cheesedb_test

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/govinargo-labs/cheesedb"
)

// TestIndexIsolation: two agents with an index of the same name never see
// each other's vectors.
func TestIndexIsolation(t *testing.T) {
	s := must(cheesedb.Open(filepath.Join(t.TempDir(), "store")))
	defer s.Close()
	alice := must(s.OpenAgent(must(s.CreateAgent("alice"))))
	bob := must(s.OpenAgent(must(s.CreateAgent("bob"))))
	for _, ag := range []*cheesedb.Agent{alice, bob} {
		if err := ag.CreateIndex("default", 2, ""); err != nil {
			t.Fatal(err)
		}
	}
	a := must(alice.AddLot("note", []byte("alice's secret")))
	if err := alice.Embed("default", a, []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if hits := must(bob.Search("default", []float32{1, 0}, 5)); len(hits) != 0 {
		t.Fatalf("Bob's search found %v", hits)
	}
	// Bob cannot embed Alice's id either: in his ledger it does not exist.
	if err := bob.Embed("default", a, []float32{1, 0}); !errors.Is(err, cheesedb.ErrUnknownID) {
		t.Fatalf("Bob embedding Alice's id: %v", err)
	}
}

// TestIndexClosedHandle: index methods on a closed handle fail like every
// other method.
func TestIndexClosedHandle(t *testing.T) {
	s := must(cheesedb.Open(filepath.Join(t.TempDir(), "store")))
	defer s.Close()
	ag := must(s.OpenAgent(must(s.CreateAgent("alice"))))
	ag.Close()
	if err := ag.CreateIndex("default", 2, ""); !errors.Is(err, cheesedb.ErrClosed) {
		t.Fatalf("CreateIndex: %v", err)
	}
	if _, err := ag.Search("default", []float32{1, 0}, 1); !errors.Is(err, cheesedb.ErrClosed) {
		t.Fatalf("Search: %v", err)
	}
}

// TestGoldenStoreWithIndex adds an index to the store v0.2.0 wrote, uses it,
// erases through it and drops it. The main log's head only changes when the
// main log gets a record (the Erase recall), never because of the index.
func TestGoldenStoreWithIndex(t *testing.T) {
	dir, tok, wantHead := copyGolden(t)
	s := must(cheesedb.Open(dir))
	defer s.Close()
	ag := must(s.OpenAgent(tok))
	if err := ag.CreateIndex("default", 3, "toy"); err != nil {
		t.Fatal(err)
	}
	if err := ag.EmbedBatch("default", []cheesedb.Embedding{
		{ID: 4, Vector: []float32{1, 0, 0}},
		{ID: 11, Vector: []float32{0, 1, 0}},
		{ID: 13, Vector: []float32{0, 0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if hits := must(ag.Search("default", []float32{0, 1, 0.1}, 1)); hits[0].ID != 11 {
		t.Fatalf("Search = %v", hits)
	}
	head := must(ag.Verify())
	if got := hex.EncodeToString(head[:]); got != wantHead {
		t.Fatalf("head changed by the index: %s, want %s", got, wantHead)
	}
	// Lot A (1) backs M1 (4) and M5 (11) alone; M6 (13) also stands on lot
	// E (12), so a Fair erase of A keeps it.
	must(ag.Recall(1, cheesedb.Fair, cheesedb.Erase))
	if hits := must(ag.Search("default", []float32{1, 1, 1}, 5)); len(hits) != 1 || hits[0].ID != 13 {
		t.Fatalf("after erasing A: %v, want only M6 (alive through lot E)", hits)
	}
	head = must(ag.Verify()) // the RECALL moved it
	if err := ag.DropIndex("default"); err != nil {
		t.Fatal(err)
	}
	if must(ag.Verify()) != head {
		t.Fatal("dropping the index changed the main log's head")
	}
}
