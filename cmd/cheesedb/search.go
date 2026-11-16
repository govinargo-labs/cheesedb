package main

// Search by meaning from the command line: `remember` also stores a vector
// when an embedding model is set up, `search` finds memories, `embed`
// (re)builds the index from every live memory, and `index` lists or drops
// indexes. remember and search run the same code as the MCP tools, so the
// terminal shows exactly what a model would see. `ask` is search, then the
// chat model; `forget` is search, then a recall after a yes.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/govinargo-labs/cheesedb"
	"github.com/govinargo-labs/cheesedb/internal/tools"
)

// stdin is where forget reads its yes or no, and stdout where it writes
// its preview; tests replace them.
var (
	stdin  io.Reader = os.Stdin
	stdout io.Writer = os.Stdout
)

const embedHelp = `search needs an embedding model. Add to %s/llm.conf, for example:
  embed_base_url = http://localhost:11434/v1
  embed_model    = nomic-embed-text
(or set EMBED_BASE_URL and EMBED_MODEL), then run: cheesedb embed <agent>`

// agentHost opens agent name for each step, like the MCP server, so the
// model is never called while the store is open.
func agentHost(name string) (tools.Host, error) {
	emb, err := newEmbedder()
	if err != nil {
		return tools.Host{}, err
	}
	open := func() (*cheesedb.Store, *cheesedb.Agent, error) {
		s, a, err := openAgent(dataDir, name)
		if err == nil {
			if rep := a.Report(); rep.DroppedBytes > 0 || rep.ResumedRollover || rep.FinishedErase > 0 || rep.UnfinishedErase > 0 {
				fmt.Printf("note: opening found %+v\n", rep)
			}
		}
		return s, a, err
	}
	return host(open, emb), nil
}

// rememberCommand: cheesedb remember <agent> "<text>" <legs>...
func rememberCommand(args []string) error {
	if len(args) < 3 {
		return errors.New(`remember needs <agent> "<text>" and at least one <leg>`)
	}
	legs, err := parseLegs(args[2:])
	if err != nil {
		return err
	}
	h, err := agentHost(args[0])
	if err != nil {
		return err
	}
	in, _ := json.Marshal(map[string]any{"text": args[1], "legs": legs})
	out, err := tools.Call(h, "remember", in)
	if err == nil {
		fmt.Println(out)
	}
	return err
}

// searchArgs reads <agent> "<question>" [k] for search and ask.
func searchArgs(cmd string, args []string) (h tools.Host, limit int, err error) {
	if len(args) < 2 {
		return h, 0, fmt.Errorf(`%s needs <agent> "<question>" and optionally how many memories`, cmd)
	}
	limit = 10
	if len(args) > 2 {
		if limit, err = strconv.Atoi(args[2]); err != nil {
			return h, 0, fmt.Errorf("how many memories: %w", err)
		}
		if limit < 1 || limit > 50 {
			return h, 0, errors.New("how many memories: 1 to 50")
		}
	}
	if h, err = agentHost(args[0]); err != nil {
		return h, 0, err
	}
	if h.Embed == nil {
		return h, 0, fmt.Errorf(embedHelp, homeDir)
	}
	return h, limit, nil
}

// askCommand: cheesedb ask <agent> "<question>" [k]. Retrieval, then
// generation (RAG): search picks the k live memories closest to the
// question, and the chat model answers from those only. Sending every
// memory instead would not fit a model's input once an agent has
// thousands. The store is closed before the chat model is called.
func askCommand(args []string) error {
	h, limit, err := searchArgs("ask", args)
	if err != nil {
		return err
	}
	c, err := newLLM()
	if err != nil {
		return err
	}
	found, err := tools.Find(h, args[1], limit)
	if errors.Is(err, cheesedb.ErrNoIndex) {
		return errors.New(tools.NothingYet)
	} else if err != nil {
		return err
	}
	texts := make([]string, len(found))
	for i, f := range found {
		texts[i] = f.Text
	}
	answer, err := ask(c, texts, args[1])
	if err == nil {
		fmt.Println(answer)
	}
	return err
}

// forgetCommand: cheesedb forget <agent> "<what to forget>". It finds the
// live memory closest in meaning, shows it and every memory that would go
// with it, and erases them only after a yes. Plain English is a guess and
// a recall cannot be undone, so the person checks the guess first.
func forgetCommand(args []string) error {
	if len(args) != 2 {
		return errors.New(`use: cheesedb forget <agent> "<what to forget>"`)
	}
	h, _, err := searchArgs("forget", args)
	if err != nil {
		return err
	}
	found, err := tools.Find(h, args[1], 1)
	if errors.Is(err, cheesedb.ErrNoIndex) {
		return errors.New(tools.NothingYet)
	} else if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Fprintln(stdout, "no memory matches; nothing erased")
		return nil
	}
	best := found[0]
	gone, err := preview(args[0], best.ID)
	if err != nil {
		return err
	}
	// Texts are quoted (%q): a memory learned from a web page could hold
	// control characters that fake or hide lines of this preview.
	fmt.Fprintf(stdout, "Closest memory (match %.2f):\n  %d: %q\n", best.Score, best.ID, best.Text)
	in := bufio.NewReader(stdin)
	for {
		if len(gone) > 1 {
			fmt.Fprintln(stdout, "These go with it (nothing else backs them):")
			for _, line := range gone[1:] {
				fmt.Fprintln(stdout, "  "+line)
			}
		}
		// Erase also wipes text that earlier Untrusted recalls left on disk
		// below it (eraseSet), which WouldRecall does not list.
		fmt.Fprintln(stdout, "Erasing also removes any text still on disk from earlier recalls below them.")
		// The store is closed while we wait, so an MCP app or another
		// command can use the agent meanwhile.
		fmt.Fprint(stdout, "Erase these, text included? This cannot be undone. (y/n) ")
		answer, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "nothing erased")
			return nil
		}
		done, err := eraseIfUnchanged(args[0], best.ID, &gone)
		if done || err != nil {
			return err
		}
		if len(gone) == 0 {
			fmt.Fprintln(stdout, "That memory was recalled meanwhile; nothing erased.")
			return nil
		}
		// Something came to depend on it while the person read the list:
		// show the new list, so nothing is erased that was not shown.
		fmt.Fprintf(stdout, "What would be erased changed meanwhile. Now:\n  %s\n", gone[0])
	}
}

// eraseIfUnchanged erases id if a Fair recall of it would still remove
// exactly *shown, checked in the same open as the recall. Otherwise it
// erases nothing and puts the new list in *shown.
func eraseIfUnchanged(name string, id cheesedb.ID, shown *[]string) (done bool, err error) {
	s, a, err := openAgent(dataDir, name)
	if err != nil {
		return false, err
	}
	defer s.Close()
	defer a.Close()
	now, err := previewOf(a, id)
	if err != nil || !slices.Equal(now, *shown) {
		*shown = now
		return false, err
	}
	removed, err := a.Recall(id, cheesedb.Fair, cheesedb.Erase)
	if err == nil {
		fmt.Fprintf(stdout, "erased memories %v\n", removed)
	}
	return true, err
}

// preview returns "id: text" for each memory a Fair recall of id would
// remove, id first.
func preview(name string, id cheesedb.ID) ([]string, error) {
	s, a, err := openAgent(dataDir, name)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	defer a.Close()
	return previewOf(a, id)
}

func previewOf(a *cheesedb.Agent, id cheesedb.ID) ([]string, error) {
	ids, err := a.WouldRecall(id, cheesedb.Fair)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(ids))
	for _, x := range ids {
		it, err := a.Get(x)
		if err != nil {
			return nil, err
		}
		line := fmt.Sprintf("%d: %q", x, it.Data)
		if x == id {
			lines = append([]string{line}, lines...)
		} else {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// learnCommand: cheesedb learn <agent>. The chat model turns live sources
// into memories, as before; then, with an embedding model set up, the new
// memories are made searchable like remember's, with the store closed while
// the model runs. Without that step, another app searching the same memory
// would never find what learn added.
func learnCommand(args []string) error {
	if len(args) < 1 {
		return errors.New("learn needs <agent>")
	}
	c, err := newLLM()
	if err != nil {
		return err
	}
	h, err := agentHost(args[0])
	if err != nil {
		return err
	}
	s, a, err := openAgent(dataDir, args[0])
	if err != nil {
		return err
	}
	facts, err := learn(c, a)
	a.Close()
	s.Close()
	if err != nil {
		return err
	}
	printFacts(facts)
	if h.Embed == nil {
		return nil
	}
	var ids []cheesedb.ID
	var texts []string
	for _, f := range facts {
		if f.err == nil {
			ids, texts = append(ids, f.id), append(texts, f.text)
		}
	}
	if err := tools.EmbedTexts(h, ids, texts); err != nil {
		return fmt.Errorf("the memories are saved, but search will not find them yet (%v); run: cheesedb embed %s", err, args[0])
	}
	return nil
}

// searchCommand: cheesedb search <agent> "<question>" [k]
func searchCommand(args []string) error {
	h, limit, err := searchArgs("search", args)
	if err != nil {
		return err
	}
	in, _ := json.Marshal(map[string]any{"query": args[1], "limit": limit})
	out, err := tools.Call(h, "search", in)
	if err == nil {
		fmt.Println(out)
	}
	return err
}

// embedCommand: cheesedb embed <agent>
func embedCommand(args []string) error {
	if len(args) < 1 {
		return errors.New("embed needs <agent>")
	}
	h, err := agentHost(args[0])
	if err != nil {
		return err
	}
	if h.Embed == nil {
		return fmt.Errorf(embedHelp, homeDir)
	}
	n, err := tools.EmbedAll(h)
	if err == nil {
		fmt.Printf("%d live memories are searchable (index %q, model %s)\n", n, tools.Index, h.Embed.Model())
	}
	return err
}

// indexCommand: cheesedb index <agent> [drop <name>]
func indexCommand(args []string) error {
	if len(args) != 1 && (len(args) != 3 || args[1] != "drop") {
		return errors.New("use: cheesedb index <agent>  or  cheesedb index <agent> drop <name>")
	}
	s, a, err := openAgent(dataDir, args[0])
	if err != nil {
		return err
	}
	defer s.Close()
	defer a.Close()
	if len(args) == 3 {
		if err := a.DropIndex(args[2]); err != nil {
			return err
		}
		fmt.Println("dropped index", args[2])
		return nil
	}
	infos, err := a.Indexes()
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Println("no indexes (cheesedb embed <agent> creates one)")
	}
	for _, in := range infos {
		if in.Err != nil {
			fmt.Printf("%s: %s\n", in.Name, red("cannot be loaded: "+in.Err.Error()+" (drop it, then embed again)"))
			continue
		}
		fmt.Printf("%s: %d vectors of %d values, model %s\n", in.Name, in.Vectors, in.Dims, in.Label)
	}
	return nil
}
