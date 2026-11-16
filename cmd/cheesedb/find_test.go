package main

import (
	"path/filepath"
	"testing"

	"github.com/govinargo-labs/cheesedb"
)

// TestFindHitsWithIndex: `cheesedb find` searches an agent that has a
// vector index (a subfolder) instead of failing on it, and looks inside.
func TestFindHitsWithIndex(t *testing.T) {
	dir := t.TempDir()
	s, err := cheesedb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := s.CreateAgent("alice")
	ag, _ := s.OpenAgent(tok)
	if _, err := ag.AddLot("note", []byte("Pune is rainy")); err != nil {
		t.Fatal(err)
	}
	if err := ag.CreateIndex("default", 2, "MARKER-model"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	hits, err := findHits(dir, "alice", "pune")
	if err != nil || len(hits) != 1 || hits[0].file != "000001.seg" {
		t.Fatalf("find pune: %+v, %v", hits, err)
	}
	hits, err = findHits(dir, "alice", "marker-model")
	want := filepath.Join("index-64656661756c74", "index.meta")
	if err != nil || len(hits) != 1 || hits[0].file != want {
		t.Fatalf("find in the index folder: %+v, %v", hits, err)
	}
}
