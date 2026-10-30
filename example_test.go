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

// Strict kills every memory that used the recalled lot, even when another
// leg still keeps it alive under Fair.
func ExampleAgent_Recall_strict() {
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

	// Fair on b: m1 still has leg a, so nothing dies.
	fair, _ := alice.Recall(b, cheesedb.Fair, cheesedb.Untrusted)
	fmt.Println("fair removed:", fair)

	// Strict on the same lot: m1 and everything built on it die anyway.
	strict, _ := alice.Recall(b, cheesedb.Strict, cheesedb.Untrusted)
	active, _ := alice.Active()
	fmt.Println("strict removed:", strict, "expected:", []cheesedb.ID{m1, m3})
	fmt.Println("active:", active, "expected:", []cheesedb.ID{m2})
	// Output:
	// fair removed: []
	// strict removed: [4 6] expected: [4 6]
	// active: [5] expected: [5]
}

// Verify checks the agent's whole log and returns its head fingerprint.
// Keep the head elsewhere to detect a rebuilt or shortened log later.
func ExampleAgent_Verify() {
	tmp, _ := os.MkdirTemp("", "cheesedb-example-")
	defer os.RemoveAll(tmp)

	s, _ := cheesedb.Open(filepath.Join(tmp, "cheesedb"))
	defer s.Close()
	tok, _ := s.CreateAgent("alice")
	alice, _ := s.OpenAgent(tok)

	a, _ := alice.AddLot("user_message", []byte("I live in Pune"))
	_, _ = alice.Remember([]byte("user lives in Pune"), cheesedb.Leg{a})

	head, err := alice.Verify()
	fmt.Println("ok:", err == nil)
	fmt.Println("head bytes:", len(head))
	_ = alice.Close()

	alice, _ = s.OpenAgent(tok)
	defer alice.Close()
	again, err := alice.Verify()
	fmt.Println("reopen ok:", err == nil)
	fmt.Println("same head:", head == again)
	// Output:
	// ok: true
	// head bytes: 32
	// reopen ok: true
	// same head: true
}

// Compact erases content of dead lots and memories that still keep it
// after an Untrusted recall; the hash-chain head stays the same.
func ExampleAgent_Compact() {
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
	_, _ = alice.Remember([]byte("carry an umbrella"), cheesedb.Leg{m1, m2})

	_, _ = alice.Recall(c, cheesedb.Fair, cheesedb.Untrusted)
	before, _ := alice.Verify()

	n, _ := alice.Compact()
	after, _ := alice.Verify()
	fmt.Println("erased:", n) // c, m2, m3 still hold text until Compact
	fmt.Println("head unchanged:", before == after)
	active, _ := alice.Active()
	fmt.Println("active:", active, "expected:", []cheesedb.ID{m1})
	// Output:
	// erased: 3
	// head unchanged: true
	// active: [4] expected: [4]
}
