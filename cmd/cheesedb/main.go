// Command cheesedb lets you try every cheesedb feature from the command line,
// including with a real LLM. Install it once, then run it from any folder:
//
//	go install ./cmd/cheesedb        (puts cheesedb in ~/go/bin; add that to your PATH)
//	cheesedb help
//
// It keeps everything in one home folder, ~/.cheesedb (or $CHEESEDB_DIR):
// the store in data/, each agent's token in tokens/, the demo in demo/. It
// never creates or deletes anything in the folder you run it from. Saving
// tokens in files is fine for trying things out; a real program must keep
// tokens secret, like passwords.
//
// # The API (package github.com/govinargo-labs/cheesedb)
//
// The store: one folder holding many agents.
//
//	cheesedb.Open(dir string) (*Store, error)
//	    Opens or creates the store folder (0700). Call once at startup.
//	(*Store).CreateAgent(name string) (Token, error)
//	    Creates an agent; returns its token ONCE (only a hash is kept).
//	    Names: 1-127 bytes, compared byte by byte. ErrAgentExists, ErrBadName.
//	(*Store).OpenAgent(tok Token) (*Agent, error)
//	    The only way to reach an agent. Opening twice shares one ledger.
//	    ErrBadToken for an unknown token.
//	(*Store).DeleteAgent(tok Token) error
//	    Removes the agent and its files. ErrAgentInUse while it is open.
//	(*Store).Close() error
//	    Closes every open agent; handles still held then return ErrClosed.
//
// Tokens: keep them safe, like passwords.
//
//	tok.String() string                 64 hex characters, to save somewhere
//	cheesedb.ParseToken(s string) (Token, error)
//
// An agent handle. No method takes an agent name or id: it can only reach
// its own agent.
//
//	(*Agent).AddLot(sourceType string, data []byte) (ID, error)
//	    Registers a source (sourceType: your label, 1-255 bytes).
//	(*Agent).Remember(data []byte, legs ...Leg) (ID, error)
//	    Stores a memory. Each Leg is a list of parent ids (lots or memories)
//	    that are ALL needed; several legs are alternatives.
//	    Refused: no legs, an empty leg, a repeated parent, identical legs
//	    (ErrBadLegs), an unknown parent (ErrUnknownID), a recalled parent
//	    (ErrDeadParent).
//	(*Agent).Recall(id ID, mode Mode, reason Reason) ([]ID, error)
//	    Recalls a lot or memory; returns the memories that died.
//	    mode: cheesedb.Fair (dies when no leg is left) or cheesedb.Strict (everything below dies)
//	    reason: cheesedb.Untrusted (text stays on disk until Compact)
//	            cheesedb.Erase (text of it and of everything dead below it is
//	            erased from disk before Recall returns)
//	(*Agent).Compact() (int, error)
//	    Erases the text of every dead record (e.g. Untrusted recalls).
//	(*Agent).Active() ([]ID, error)
//	    The ids of every live memory.
//	(*Agent).Get(id ID) (Item, error)
//	    One lot or memory: Item{ID, Kind (cheesedb.KindLot / cheesedb.KindMemory),
//	    Data, Alive, Redacted}. Data is nil once Redacted.
//	(*Agent).Verify() ([32]byte, error)
//	    Re-checks the whole log on disk; returns the head fingerprint.
//	    Keep a copy elsewhere to detect a rebuilt or shortened log later.
//	(*Agent).Report() Report
//	    What opening found: Records, DroppedBytes, ResumedRollover, FinishedErase.
//	(*Agent).Close() error
//
// Errors to check with errors.Is: ErrBadName, ErrAgentExists, ErrBadToken,
// ErrAgentInUse, ErrClosed, ErrUnknownID, ErrDeadParent, ErrBadLegs,
// ErrBadArgument, ErrCorrupt.
package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/govinargo-labs/cheesedb"
)

// The home folder and the folders inside it, set by setDirs at startup.
var homeDir, dataDir, tokenDir, demoDir string

// makeHome creates the home folder (0700). cheesedb.Open creates only its own
// folder, not the ones above it, so the first `agent create` or `demo` needs
// this.
func makeHome() error {
	return os.MkdirAll(homeDir, 0o700)
}

const usage = `cheesedb: try cheesedb from the command line.
Everything is kept in ~/.cheesedb (change it with CHEESEDB_DIR=/some/folder).

Agents
  cheesedb agent create <agent>               create an agent (token saved in tokens/)
  cheesedb agent delete <agent>               delete an agent and all its files

Sources and memories
  cheesedb lot <agent> <sourceType> "<text>"  add a source, prints its id
  cheesedb remember <agent> "<text>" <legs>  add a memory; each leg is ids joined
                                              by commas: "1 2" = from 1 OR 2,
                                              "4,5" = needs 4 AND 5

Looking
  cheesedb show <agent>                       every lot and memory: alive? erased? text
  cheesedb active <agent>                     live memories only
  cheesedb get <agent> <id>                   one record
  cheesedb find <agent> "<text>"              search the raw files on disk for text

Removing
  cheesedb recall <agent> <id> fair|strict untrusted|erase
  cheesedb compact <agent>                    erase the text of every dead record

Checking
  cheesedb verify <agent>                     check the whole log, print the head fingerprint

With an LLM (any OpenAI-compatible server, set in <home>/llm.conf; see llm.go)
  cheesedb learn <agent>                      the LLM turns live sources into memories
  cheesedb ask <agent> "<question>"           the LLM answers from live memories only
  cheesedb demo                               the full story, compared with a naive store
`

func main() {
	if err := setDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// setDirs picks the home folder: $CHEESEDB_DIR, or ~/.cheesedb. One fixed
// folder means cheesedb reaches the same agents from wherever it runs, and the
// demo's delete-and-recreate only ever touches <home>/demo, never the folder
// you are standing in.
func setDirs() error {
	root := os.Getenv("CHEESEDB_DIR")
	if root == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root = filepath.Join(h, ".cheesedb")
	}
	homeDir = root
	dataDir = filepath.Join(root, "data")
	tokenDir = filepath.Join(root, "tokens")
	demoDir = filepath.Join(root, "demo")
	return nil
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" {
		fmt.Print(usage)
		return nil
	}
	cmd, args := args[0], args[1:]
	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("%s needs %d argument(s); see: cheesedb help", cmd, n)
		}
		return nil
	}

	switch cmd {
	case "demo":
		return demo()
	case "agent":
		if len(args) < 2 || (args[0] != "create" && args[0] != "delete") {
			return errors.New("use: cheesedb agent create <agent>  or  cheesedb agent delete <agent>")
		}
		if args[0] == "create" {
			return create(dataDir, args[1])
		}
		return deleteAgent(args[1])
	}

	if err := need(1); err != nil {
		return err
	}
	s, a, err := openAgent(dataDir, args[0])
	if err != nil {
		return err
	}
	defer s.Close()
	defer a.Close()
	if rep := a.Report(); rep.DroppedBytes > 0 || rep.ResumedRollover || rep.FinishedErase > 0 {
		fmt.Printf("note: opening found %+v\n", rep)
	}
	name, args := args[0], args[1:]

	switch cmd {
	case "lot":
		if len(args) < 2 {
			return errors.New(`lot needs <agent> <sourceType> "<text>"`)
		}
		id, err := a.AddLot(args[0], []byte(args[1]))
		if err == nil {
			fmt.Println("lot", id)
		}
		return err
	case "remember":
		if len(args) < 2 {
			return errors.New(`remember needs <agent> "<text>" <leg>...`)
		}
		legs, err := parseLegs(args[1:])
		if err != nil {
			return err
		}
		id, err := a.Remember([]byte(args[0]), legs...)
		if err == nil {
			fmt.Println("memory", id)
		}
		return err
	case "show":
		return show(a)
	case "active":
		return active(a)
	case "get":
		if len(args) < 1 {
			return errors.New("get needs <agent> <id>")
		}
		id, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			return err
		}
		it, err := a.Get(cheesedb.ID(id))
		if err == nil {
			printItem(it)
		}
		return err
	case "find":
		if len(args) < 1 {
			return errors.New(`find needs <agent> "<text>"`)
		}
		return find(dataDir, name, args[0])
	case "recall":
		if len(args) < 3 {
			return errors.New("recall needs <agent> <id> fair|strict untrusted|erase")
		}
		return recall(a, args[0], args[1], args[2])
	case "compact":
		n, err := a.Compact()
		if err == nil {
			fmt.Println("erased the text of", n, "dead record(s)")
		}
		return err
	case "verify":
		head, err := a.Verify()
		if err == nil {
			fmt.Printf("OK. head fingerprint: %x\n", head)
		}
		return err
	case "learn":
		c, err := newLLM()
		if err != nil {
			return err
		}
		facts, err := learn(c, a)
		if err == nil {
			printFacts(facts)
		}
		return err
	case "ask":
		if len(args) < 1 {
			return errors.New(`ask needs <agent> "<question>"`)
		}
		c, err := newLLM()
		if err != nil {
			return err
		}
		texts, err := liveMemoryTexts(a)
		if err != nil {
			return err
		}
		answer, err := ask(c, texts, args[0])
		if err == nil {
			fmt.Println(answer)
		}
		return err
	}
	return fmt.Errorf("unknown command %q; see: cheesedb help", cmd)
}

// tokenPath keeps each agent's token in a file named by the hex of its
// name, the same way the store names folders, so no name can escape.
func tokenPath(name string) string {
	return filepath.Join(tokenDir, hex.EncodeToString([]byte(name))+".token")
}

func create(dir, name string) error {
	if err := makeHome(); err != nil {
		return err
	}
	s, err := cheesedb.Open(dir)
	if err != nil {
		return err
	}
	defer s.Close()
	tok, err := s.CreateAgent(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tokenPath(name), []byte(tok.String()), 0o600); err != nil {
		return err
	}
	fmt.Printf("created %q; token saved in %s\n", name, tokenPath(name))
	return nil
}

func openAgent(dir, name string) (*cheesedb.Store, *cheesedb.Agent, error) {
	b, err := os.ReadFile(tokenPath(name))
	if err != nil {
		return nil, nil, fmt.Errorf("no token for %q (create it first: cheesedb agent create %s): %w", name, name, err)
	}
	tok, err := cheesedb.ParseToken(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, nil, err
	}
	s, err := cheesedb.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	a, err := s.OpenAgent(tok)
	if err != nil {
		s.Close()
		return nil, nil, err
	}
	return s, a, nil
}

func deleteAgent(name string) error {
	b, err := os.ReadFile(tokenPath(name))
	if err != nil {
		return fmt.Errorf("no token for %q: %w", name, err)
	}
	tok, err := cheesedb.ParseToken(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	s, err := cheesedb.Open(dataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.DeleteAgent(tok); err != nil {
		return err
	}
	os.Remove(tokenPath(name))
	fmt.Printf("deleted %q\n", name)
	return nil
}

// parseLegs turns ["1", "2"] into two legs [1] [2], and ["4,5"] into one
// leg [4 5].
func parseLegs(args []string) ([]cheesedb.Leg, error) {
	var legs []cheesedb.Leg
	for _, a := range args {
		var leg cheesedb.Leg
		for _, part := range strings.Split(a, ",") {
			id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("leg %q: ids must be numbers joined by commas", a)
			}
			leg = append(leg, cheesedb.ID(id))
		}
		legs = append(legs, leg)
	}
	return legs, nil
}

func printItem(it cheesedb.Item) {
	kind := "lot   "
	if it.Kind == cheesedb.KindMemory {
		kind = "memory"
	}
	state := "alive"
	if !it.Alive {
		state = "DEAD "
	}
	text := fmt.Sprintf("%q", it.Data)
	if it.Redacted {
		text = "(text erased from disk)"
	}
	fmt.Printf("[%d] %s %s %s\n", it.ID, kind, state, text)
}

// show lists every lot and memory. The handle has no "count" method, but
// this process just opened the agent, so Report().Records is the number of
// records on disk; ids 1..Records cover them (RECALL records are skipped).
func show(a *cheesedb.Agent) error {
	for id := cheesedb.ID(1); id <= cheesedb.ID(a.Report().Records); id++ {
		it, err := a.Get(id)
		if errors.Is(err, cheesedb.ErrUnknownID) {
			continue // a RECALL record
		}
		if err != nil {
			return err
		}
		printItem(it)
	}
	return nil
}

func active(a *cheesedb.Agent) error {
	ids, err := a.Active()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("(no live memories)")
	}
	for _, id := range ids {
		it, err := a.Get(id)
		if err != nil {
			return err
		}
		printItem(it)
	}
	return nil
}

func recall(a *cheesedb.Agent, idArg, modeArg, reasonArg string) error {
	id, err := strconv.ParseUint(idArg, 10, 64)
	if err != nil {
		return err
	}
	mode := map[string]cheesedb.Mode{"fair": cheesedb.Fair, "strict": cheesedb.Strict}[modeArg]
	reason := map[string]cheesedb.Reason{"untrusted": cheesedb.Untrusted, "erase": cheesedb.Erase}[reasonArg]
	if mode == 0 || reason == 0 {
		return errors.New("recall needs fair|strict and untrusted|erase")
	}
	removed, err := a.Recall(cheesedb.ID(id), mode, reason)
	if err != nil {
		return err
	}
	fmt.Printf("recalled %d; memories removed: %v\n", id, removed)
	if reason == cheesedb.Untrusted {
		fmt.Println("(untrusted: their text is still on disk until `compact`; try `find`)")
	}
	return nil
}

func find(dir, name, text string) error {
	hits, err := findHits(dir, name, text)
	if err != nil {
		return err
	}
	for _, h := range hits {
		fmt.Printf("  found in %s at byte %d: …%s…\n", h.file, h.at, h.context)
	}
	if len(hits) == 0 {
		fmt.Printf("  %q: not found anywhere in %s (searched every file, byte by byte)\n",
			text, filepath.Join(dir, hex.EncodeToString([]byte(name))))
	}
	return nil
}

// hit is one place where findHits found the text.
type hit struct {
	file    string
	at      int    // byte offset in the file
	context string // the bytes around it, unprintable ones shown as '.'
}

// findHits searches every file of an agent, byte by byte and ignoring case,
// for text: the honest test of whether something is still on disk. It reads
// the files directly, not through cheesedb.
func findHits(dir, name, text string) ([]hit, error) {
	folder := filepath.Join(dir, hex.EncodeToString([]byte(name)))
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	needle := asciiLower([]byte(text))
	var hits []hit
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(folder, e.Name()))
		if err != nil {
			return hits, err
		}
		lower := asciiLower(b)
		for i := 0; ; {
			j := bytes.Index(lower[i:], needle)
			if j < 0 {
				break
			}
			at := i + j
			from, to := max(0, at-25), min(len(b), at+len(needle)+25)
			hits = append(hits, hit{e.Name(), at, printable(b[from:to])})
			i = at + len(needle)
		}
	}
	return hits, nil
}

// asciiLower lower-cases A-Z only and keeps every other byte, so positions
// in the result match positions in the file. (bytes.ToLower treats input as
// UTF-8 text; on binary data it can change the length and shift positions.)
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 32 || c > 126 {
			c = '.'
		}
		out[i] = c
	}
	return string(out)
}
