package cheesedb_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/govinargo-labs/cheesedb"
	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

// The golden store is a small store written by cheesedb v0.2.0 and kept in
// testdata. Every later version must open it, verify it with the same head
// fingerprint and read back the same answers: proof that a change to the
// code did not change the meaning of files already on disk.
//
// No token is kept with it: a token is a secret, even a test one. Each
// test gives its copy a fresh token instead (see copyGolden).
const goldenDir = "testdata/golden-v0.2.0"

var goldenAgent = hex.EncodeToString([]byte("golden")) // the agent's folder

// must unwraps a (value, error) pair; see the copy in cheesedb_test.go,
// which this external test package cannot see.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// goldenItem is what Get must return for one id of the golden store.
type goldenItem struct {
	id       cheesedb.ID
	data     string // "" once redacted
	alive    bool
	redacted bool
}

var goldenItems = []goldenItem{
	{1, "I live in Pune", true, false},                 // lot A
	{2, "", false, true},                               // lot B, compacted
	{3, "", false, true},                               // lot C, compacted
	{4, "user lives in Pune", true, false},             // M1: [A] or [B]
	{5, "", false, true},                               // M2: [C]
	{6, "", false, true},                               // M3: [M1, M2]
	{9, "", false, true},                               // lot D, erased
	{10, "", false, true},                              // M4: [D], erased
	{11, "the user likes tea", true, false},            // M5: [A]
	{12, "notes from the meeting", true, false},        // lot E
	{13, "the meeting is on Friday", true, false},      // M6: [E] or [A]
	{14, "the user likes tea on Fridays", true, false}, // M7: [M5, M6]
}

// writeGolden runs the fixed script that made the golden store.
func writeGolden(t *testing.T, dir string) [32]byte {
	t.Helper()
	s, err := cheesedb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tok := must(s.CreateAgent("golden"))
	ag := must(s.OpenAgent(tok))
	defer ag.Close()

	a := must(ag.AddLot("user_message", []byte("I live in Pune")))
	b := must(ag.AddLot("web_page", []byte("profile page: lives in Pune")))
	c := must(ag.AddLot("weather_tool", []byte("Pune: rain")))
	m1 := must(ag.Remember([]byte("user lives in Pune"), cheesedb.Leg{a}, cheesedb.Leg{b}))
	m2 := must(ag.Remember([]byte("Pune is rainy"), cheesedb.Leg{c}))
	must(ag.Remember([]byte("carry an umbrella"), cheesedb.Leg{m1, m2}))
	must(ag.Recall(b, cheesedb.Fair, cheesedb.Untrusted))   // 7: M1 survives on A
	must(ag.Recall(c, cheesedb.Strict, cheesedb.Untrusted)) // 8: M2, M3 die
	d := must(ag.AddLot("tool_output", []byte("secret token 1234")))
	must(ag.Remember([]byte("the token is 1234"), cheesedb.Leg{d}))
	m5 := must(ag.Remember([]byte("the user likes tea"), cheesedb.Leg{a}))
	e := must(ag.AddLot("document", []byte("notes from the meeting")))
	m6 := must(ag.Remember([]byte("the meeting is on Friday"), cheesedb.Leg{e}, cheesedb.Leg{a}))
	must(ag.Remember([]byte("the user likes tea on Fridays"), cheesedb.Leg{m5, m6}))
	must(ag.Recall(d, cheesedb.Fair, cheesedb.Erase)) // 15
	must(ag.Compact())
	return must(ag.Verify())
}

// TestWriteGolden regenerates the golden store. It is skipped unless
// CHEESEDB_WRITE_GOLDEN=1, and must only ever be run with the version the
// folder is named after.
func TestWriteGolden(t *testing.T) {
	if os.Getenv("CHEESEDB_WRITE_GOLDEN") != "1" {
		t.Skip("set CHEESEDB_WRITE_GOLDEN=1 to regenerate " + goldenDir)
	}
	if err := os.RemoveAll(goldenDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(goldenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	head := writeGolden(t, filepath.Join(goldenDir, "store"))
	// The LOCK file is released by Close but stays on disk; it is not data.
	if err := os.Remove(filepath.Join(goldenDir, "store", "LOCK")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goldenDir, "head"), []byte(hex.EncodeToString(head[:])+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// copyGolden copies the golden store into a temporary folder, because
// opening a store writes to it (the LOCK file) and tests must never change
// testdata. The copy's agent.key then gets the hash of a fresh token; the
// segments, which hold the data and the head, stay as v0.2.0 wrote them.
func copyGolden(t *testing.T) (dir string, tok cheesedb.Token, head string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "store")
	src := filepath.Join(goldenDir, "store")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	tok = rekey(t, filepath.Join(dir, goldenAgent, "agent.key"))
	b, err := os.ReadFile(filepath.Join(goldenDir, "head"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, tok, strings.TrimSpace(string(b))
}

// rekey replaces the token hash in an agent.key file with the hash of a
// fresh token, and returns the token. The layout is the one v0.2.0 wrote:
// version (1), SHA-256 of the token, CRC-32C of both. The file must
// already be valid in that layout, so the golden one is checked too.
func rekey(t *testing.T, path string) cheesedb.Token {
	t.Helper()
	castagnoli := crc32.MakeTable(crc32.Castagnoli)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 37 || b[0] != 1 || crc32.Checksum(b[:33], castagnoli) != binary.LittleEndian.Uint32(b[33:]) {
		t.Fatalf("%s is not a valid v0.2.0 agent.key", path)
	}
	var raw [32]byte
	rand.Read(raw[:])
	tok, err := cheesedb.ParseToken(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256(raw[:])
	copy(b[1:], key[:])
	binary.LittleEndian.PutUint32(b[33:], crc32.Checksum(b[:33], castagnoli))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestGoldenStore opens the store v0.2.0 wrote and checks every answer.
func TestGoldenStore(t *testing.T) {
	dir, tok, wantHead := copyGolden(t)
	s := must(cheesedb.Open(dir))
	defer s.Close()
	ag := must(s.OpenAgent(tok))

	if rep := ag.Report(); rep.DroppedBytes != 0 || rep.FinishedErase != 0 {
		t.Fatalf("Report = %+v, want a clean open", rep)
	}
	head := must(ag.Verify())
	if got := hex.EncodeToString(head[:]); got != wantHead {
		t.Fatalf("head = %s, want %s", got, wantHead)
	}
	if got, want := must(ag.Active()), []cheesedb.ID{4, 11, 13, 14}; !slices.Equal(got, want) {
		t.Fatalf("Active = %v, want %v", got, want)
	}
	for _, w := range goldenItems {
		it := must(ag.Get(w.id))
		if string(it.Data) != w.data || it.Alive != w.alive || it.Redacted != w.redacted {
			t.Errorf("Get(%d) = {data %q alive %v redacted %v}, want %+v", w.id, it.Data, it.Alive, it.Redacted, w)
		}
	}
}

// TestUnknownFolderInAgentIgnored pins down what vector indexes rely on:
// a folder the log does not know, inside an agent's folder, is left alone
// and does not stop the agent opening or verifying.
func TestUnknownFolderInAgentIgnored(t *testing.T) {
	dir, tok, wantHead := copyGolden(t)
	agentDir := filepath.Join(dir, goldenAgent)
	unknown := filepath.Join(agentDir, "index-74657374")
	if err := os.MkdirAll(unknown, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknown, "000001.seg"), []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := must(cheesedb.Open(dir))
	defer s.Close()
	ag := must(s.OpenAgent(tok))
	head := must(ag.Verify())
	if got := hex.EncodeToString(head[:]); got != wantHead {
		t.Fatalf("head = %s, want %s", got, wantHead)
	}
	if _, err := os.Stat(filepath.Join(unknown, "000001.seg")); err != nil {
		t.Fatalf("the unknown folder was touched: %v", err)
	}
}

// TestWritesSameRecords runs the golden script with this version and
// compares what it writes with what v0.2.0 wrote: the same files, and the
// same records in them (type, id, structure, data, size, redacted or not).
// The files cannot match byte for byte: every record has a random salt,
// and every hash depends on it. Everything else must be identical.
func TestWritesSameRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	writeGolden(t, dir)
	golden, _, _ := copyGolden(t)

	type rec struct {
		typ       record.Type
		id        uint64
		structure []byte
		data      []byte
		size      int
		redacted  bool
	}
	read := func(store string) (files []string, recs []rec) {
		agentDir := filepath.Join(store, goldenAgent)
		entries, err := os.ReadDir(agentDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			files = append(files, e.Name())
		}
		lg, _, err := logfile.Open(agentDir, logfile.Options{}, func(r *record.Record) error {
			recs = append(recs, rec{r.Type, r.ID, r.Structure, r.Data(), r.Size(), r.Redacted()})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		lg.Close()
		return files, recs
	}
	wantFiles, want := read(golden)
	gotFiles, got := read(dir)
	if !slices.Equal(gotFiles, wantFiles) {
		t.Fatalf("files %v, v0.2.0 wrote %v", gotFiles, wantFiles)
	}
	if len(got) != len(want) {
		t.Fatalf("%d records, v0.2.0 wrote %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.typ != w.typ || g.id != w.id || !bytes.Equal(g.structure, w.structure) ||
			!bytes.Equal(g.data, w.data) || g.size != w.size || g.redacted != w.redacted {
			t.Errorf("record %d = %+v, v0.2.0 wrote %+v", w.id, g, w)
		}
	}
}
