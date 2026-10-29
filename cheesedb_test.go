package cheesedb

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func mustStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestCreateOpenUse: the whole life of an agent, across a store reopen.
func TestCreateOpenUse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cheesedb")
	s := mustStore(t, dir)
	tok := must(s.CreateAgent("alice"))

	a := must(s.OpenAgent(tok))
	lot := must(a.AddLot("user_message", []byte("I live in Pune")))
	m := must(a.Remember([]byte("user lives in Pune"), Leg{lot}))
	if _, err := a.Verify(); err != nil {
		t.Fatal(err)
	}
	a.Close()
	s.Close()

	// The token survives as text, the way a program would keep it.
	tok2, err := ParseToken(tok.String())
	if err != nil || tok2 != tok {
		t.Fatalf("ParseToken(String()) = %v, %v", tok2, err)
	}
	s = mustStore(t, dir)
	a = must(s.OpenAgent(tok2))
	defer a.Close()
	if it := must(a.Get(m)); string(it.Data) != "user lives in Pune" || it.Kind != KindMemory || !it.Alive {
		t.Fatalf("Get after reopen = %+v", it)
	}
	if rep := a.Report(); rep.Records != 2 {
		t.Fatalf("Report = %+v, want 2 records", rep)
	}
}

// TestOpenNeedsTheRightToken: the name is not a key, and neither is a
// wrong, empty or made-up token.
func TestOpenNeedsTheRightToken(t *testing.T) {
	s := mustStore(t, t.TempDir())
	must(s.CreateAgent("alice"))

	var fromName Token
	copy(fromName[:], "alice") // someone tries the name as if it were a token
	for name, tok := range map[string]Token{
		"the name as a token": fromName,
		"the zero token":      {},
		"a made-up token":     {1, 2, 3},
	} {
		if _, err := s.OpenAgent(tok); !errors.Is(err, ErrBadToken) {
			t.Errorf("%s: got %v, want ErrBadToken", name, err)
		}
	}
	for _, text := range []string{"alice", "", "zz", strings.Repeat("ab", 31)} {
		if _, err := ParseToken(text); !errors.Is(err, ErrBadToken) {
			t.Errorf("ParseToken(%q): got %v, want ErrBadToken", text, err)
		}
	}
}

// TestAgentsAreIsolated: Alice and Bob hold records with the same ids.
// Through Alice's handle, every id means Alice's record; nothing reaches
// Bob's data, and Alice's recall does not touch Bob.
func TestAgentsAreIsolated(t *testing.T) {
	s := mustStore(t, t.TempDir())
	alice := must(s.OpenAgent(must(s.CreateAgent("alice"))))
	bob := must(s.OpenAgent(must(s.CreateAgent("bob"))))
	defer alice.Close()
	defer bob.Close()

	aLot := must(alice.AddLot("user_message", []byte("alice's secret")))
	aMem := must(alice.Remember([]byte("alice's memory"), Leg{aLot}))
	bLot := must(bob.AddLot("user_message", []byte("bob's secret")))
	bMem := must(bob.Remember([]byte("bob's memory"), Leg{bLot}))
	if aLot != bLot || aMem != bMem {
		t.Fatal("test setup: ids should coincide, so that only the handle tells them apart")
	}

	for _, id := range []ID{aLot, aMem} {
		if it := must(alice.Get(id)); bytes.Contains(it.Data, []byte("bob")) {
			t.Fatalf("Alice's handle returned Bob's data for id %d", id)
		}
	}
	if _, err := alice.Get(99); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("Get of an id Alice does not have = %v, want ErrUnknownID", err)
	}

	must(alice.Recall(aLot, Fair, Untrusted))
	if got := must(bob.Active()); !slices.Equal(got, []ID{bMem}) {
		t.Fatalf("Bob's memories after Alice's recall = %v, want [%d]", got, bMem)
	}
}

// TestTrickyNames: every name becomes a folder directly inside the store,
// distinct from every other, whatever characters it holds.
func TestTrickyNames(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	names := []string{
		"a:b", "../x", "..", ".", "/", "a/b", "/etc/passwd", "a\x00b",
		"A", "a", // different agents, even on case-insensitive macOS
		"é", "é", // é as one character, and e + accent: different bytes
		strings.Repeat("z", MaxNameBytes),
	}
	toks := map[Token]string{}
	for _, n := range names {
		tok, err := s.CreateAgent(n)
		if err != nil {
			t.Fatalf("CreateAgent(%q): %v", n, err)
		}
		toks[tok] = n
	}

	entries := folderEntries(t, dir)
	if len(entries) != len(names) {
		t.Fatalf("%d entries in the store folder, want %d (one per name, nothing outside it)", len(entries), len(names))
	}
	for _, e := range entries {
		b, err := hex.DecodeString(e.Name())
		if err != nil || !slices.Contains(names, string(b)) {
			t.Fatalf("unexpected folder %q", e.Name())
		}
	}
	for tok, n := range toks {
		a := must(s.OpenAgent(tok))
		must(a.AddLot("tool", []byte(n)))
		a.Close()
	}
	// The longest name must survive the temporary names used while
	// deleting too (a bug this test found: they once exceeded 255 bytes).
	for tok, n := range toks {
		if len(n) == MaxNameBytes {
			if err := s.DeleteAgent(tok); err != nil {
				t.Fatalf("DeleteAgent(longest name): %v", err)
			}
		}
	}

	for _, bad := range []string{"", strings.Repeat("z", MaxNameBytes+1)} {
		if _, err := s.CreateAgent(bad); !errors.Is(err, ErrBadName) {
			t.Errorf("CreateAgent(%d bytes): got %v, want ErrBadName", len(bad), err)
		}
	}
}

// TestSameNameTwice: creating an existing agent is refused, and the
// existing agent keeps its token and data.
func TestSameNameTwice(t *testing.T) {
	s := mustStore(t, t.TempDir())
	tok := must(s.CreateAgent("alice"))
	a := must(s.OpenAgent(tok))
	must(a.AddLot("tool", []byte("kept")))
	a.Close()

	if _, err := s.CreateAgent("alice"); !errors.Is(err, ErrAgentExists) {
		t.Fatalf("second CreateAgent = %v, want ErrAgentExists", err)
	}
	a = must(s.OpenAgent(tok))
	defer a.Close()
	if it := must(a.Get(1)); string(it.Data) != "kept" {
		t.Fatalf("existing agent's data = %q", it.Data)
	}
}

// TestTokenNeverStored: no file in the store holds the token, as bytes or
// as text; agent.key holds only its SHA-256.
func TestTokenNeverStored(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	tok := must(s.CreateAgent("alice"))
	a := must(s.OpenAgent(tok))
	must(a.AddLot("tool", []byte("x")))
	a.Close()

	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(path)
		if bytes.Contains(b, tok[:]) || bytes.Contains(b, []byte(tok.String())) {
			t.Errorf("%s contains the token", path)
		}
		return nil
	})
	key := keyOf(tok)
	b, _ := os.ReadFile(filepath.Join(dir, hex.EncodeToString([]byte("alice")), keyFile))
	if !bytes.Contains(b, key[:]) {
		t.Error("agent.key does not hold the token's SHA-256")
	}
}

// TestCrashDuringCreate: a crash before the rename leaves only a temporary
// folder. Open removes it, and nobody ever received a token for it.
func TestCrashDuringCreate(t *testing.T) {
	dir := t.TempDir()
	half := filepath.Join(dir, tmpPrefix+"0123456789abcdef")
	os.MkdirAll(half, 0o700)
	os.WriteFile(filepath.Join(half, keyFile), []byte("half-written"), 0o600)

	s := mustStore(t, dir)
	if _, err := os.Stat(half); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the half-made agent folder is still there")
	}
	if _, err := s.CreateAgent("alice"); err != nil {
		t.Fatalf("CreateAgent after the crash: %v", err)
	}
}

// TestDamagedKeyIsCorrupt: an agent folder whose key is missing or damaged
// is reported, not silently skipped (skipping would hide its data).
func TestDamagedKeyIsCorrupt(t *testing.T) {
	for name, damage := range map[string]func(path string){
		"key deleted": func(p string) { os.Remove(p) },
		"key changed": func(p string) {
			b, _ := os.ReadFile(p)
			b[5] ^= 1
			os.WriteFile(p, b, 0o600)
		},
	} {
		dir := t.TempDir()
		s := mustStore(t, dir)
		must(s.CreateAgent("alice"))
		s.Close()
		damage(filepath.Join(dir, hex.EncodeToString([]byte("alice")), keyFile))
		if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Open = %v, want ErrCorrupt", name, err)
		}
	}
}

// TestOpenTwiceSharesOneLedger: two handles on one agent see each other's
// writes immediately (one ledger, one writer), and the ledger stays open
// until the last handle closes.
func TestOpenTwiceSharesOneLedger(t *testing.T) {
	s := mustStore(t, t.TempDir())
	tok := must(s.CreateAgent("alice"))
	h1 := must(s.OpenAgent(tok))
	h2 := must(s.OpenAgent(tok))
	if h1.sh != h2.sh {
		t.Fatal("two opens of one agent made two ledgers: two writers on the same files")
	}
	lot := must(h1.AddLot("tool", []byte("x")))
	must(h2.Remember([]byte("m"), Leg{lot}))

	h1.Close()
	if _, err := h1.Active(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed handle: %v, want ErrClosed", err)
	}
	if got := must(h2.Active()); len(got) != 1 {
		t.Fatalf("other handle after one Close: Active = %v", got)
	}
	h2.Close()
	if len(s.open) != 0 {
		t.Fatal("the ledger is still open after its last handle closed")
	}
}

// TestDeleteAgent: refused while open; afterwards the folder is gone, the
// token no longer works, and the name can be created again with a new token.
func TestDeleteAgent(t *testing.T) {
	dir := t.TempDir()
	s := mustStore(t, dir)
	tok := must(s.CreateAgent("alice"))
	a := must(s.OpenAgent(tok))
	must(a.AddLot("tool", []byte("x")))

	if err := s.DeleteAgent(tok); !errors.Is(err, ErrAgentInUse) {
		t.Fatalf("DeleteAgent while open = %v, want ErrAgentInUse", err)
	}
	a.Close()
	if err := s.DeleteAgent(tok); err != nil {
		t.Fatal(err)
	}
	if entries := folderEntries(t, dir); len(entries) != 0 {
		t.Fatalf("store folder still holds %d entries after the delete", len(entries))
	}
	if _, err := s.OpenAgent(tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("old token after delete = %v, want ErrBadToken", err)
	}
	tok2 := must(s.CreateAgent("alice"))
	if tok2 == tok {
		t.Fatal("re-created agent got the old token")
	}
	a = must(s.OpenAgent(tok2))
	defer a.Close()
	if got := must(a.Active()); len(got) != 0 {
		t.Fatal("re-created agent sees the deleted agent's data")
	}
}

// TestStoreCloseStopsHandles: after the store closes, handles still held
// refuse every call, including ones that only read memory.
func TestStoreCloseStopsHandles(t *testing.T) {
	s := mustStore(t, t.TempDir())
	a := must(s.OpenAgent(must(s.CreateAgent("alice"))))
	s.Close()
	if _, err := a.Active(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Active after the store closed = %v, want ErrClosed", err)
	}
	if _, err := s.OpenAgent(Token{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("OpenAgent after Close = %v, want ErrClosed", err)
	}
	a.Close() // must not panic or double-close
}

// TestConcurrentAgents: many goroutines create, open, write and close
// different agents, and some open the same agent at once. Run with -race.
func TestConcurrentAgents(t *testing.T) {
	s := mustStore(t, t.TempDir())
	shared := must(s.CreateAgent("shared"))
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			own, err := s.CreateAgent(string(rune('a' + w)))
			if err != nil {
				t.Error(err)
				return
			}
			for _, tok := range []Token{own, shared} {
				a, err := s.OpenAgent(tok)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := a.AddLot("tool", []byte("x")); err != nil {
					t.Error(err)
				}
				a.Close()
			}
		})
	}
	wg.Wait()
	a := must(s.OpenAgent(shared))
	defer a.Close()
	if a.Report().Records != 8 {
		t.Fatalf("shared agent holds %d records, want 8", a.Report().Records)
	}
}

// folderEntries lists the store folder, leaving out the LOCK file that every
// open store holds.
func folderEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	all, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []os.DirEntry
	for _, e := range all {
		if e.Name() != lockName {
			out = append(out, e)
		}
	}
	return out
}
