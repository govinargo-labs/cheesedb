package cheesedb_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/govinargo-labs/cheesedb"
)

// The README's example, checked by `go test`, so the documentation cannot
// drift from the code.
func Example() {
	tmp, _ := os.MkdirTemp("", "cheesedb-example-")
	defer os.RemoveAll(tmp)

	s, _ := cheesedb.Open(filepath.Join(tmp, "cheesedb"))
	defer s.Close()
	tok, _ := s.CreateAgent("alice")
	alice, _ := s.OpenAgent(tok)
	defer alice.Close()

	a, _ := alice.AddLot("user_message", []byte("I live in Pune"))
	b, _ := alice.AddLot("web_page", []byte("profile: lives in Pune"))
	c, _ := alice.AddLot("weather_tool", []byte("Pune: rain"))

	m1, _ := alice.Remember([]byte("user lives in Pune"), cheesedb.Leg{a}, cheesedb.Leg{b})
	m2, _ := alice.Remember([]byte("Pune is rainy"), cheesedb.Leg{c})
	m3, _ := alice.Remember([]byte("carry an umbrella"), cheesedb.Leg{m1, m2})

	removed, _ := alice.Recall(c, cheesedb.Fair, cheesedb.Untrusted)
	active, _ := alice.Active()
	fmt.Println("removed:", removed, "expected:", []cheesedb.ID{m2, m3})
	fmt.Println("active:", active, "expected:", []cheesedb.ID{m1})
	// Output:
	// removed: [5 6] expected: [5 6]
	// active: [4] expected: [4]
}
