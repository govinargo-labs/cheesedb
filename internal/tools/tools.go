// Package tools turns one agent's cheesedb memory into tools a model can
// call: add_lot, remember, recall, get and list_memories, plus search when
// the host has an embedding model.
//
// A model cannot call Go functions. It sends a tool name and its arguments
// as JSON, and reads text back. List describes the tools; Call runs one.
// The package knows nothing about MCP or any LLM API, so the same tools can
// be served over MCP (cmd/cheesedb) or handed to a model through function
// calling.
//
// The host opens the agent (Host.Open) and no tool takes an agent name or
// id, so a model can only ever reach the agent it was given.
//
// Search: with an embedding model, remember also stores a vector of each
// memory in the agent's index "default" (created on first use, labelled
// with the model's name), and search finds memories by meaning. The model
// is called by the host, never by the cheesedb library, and always before
// the store is opened, so a slow model never keeps the store locked.
package tools

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/govinargo-labs/cheesedb"
)

// Index is the name of the index that remember and search use.
const Index = "default"

// Host is what the tools need from the program serving them.
type Host struct {
	// Open opens the agent for one call; done closes it again.
	Open func() (a *cheesedb.Agent, done func(), err error)
	// Embed is the host's embedding model, or nil if it has none. Without
	// one, there is no search tool and remember stores no vector.
	Embed Embedder
	// Confirm asks the person, not the model, a yes or no question, or is
	// nil when the host has no way to. recall asks it before removing
	// anything. It is called while the store is closed, since a person
	// may take minutes to answer.
	Confirm func(question string) (yes bool, err error)
}

// Embedder turns texts into vectors, one per text, in order.
type Embedder interface {
	Model() string // kept as the index's label, to notice a model change
	Embed(texts []string) ([][]float32, error)
}

// Tool describes one tool for a model: its name, what it is for, and its
// arguments as a JSON Schema.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// List returns the tools: five, and search when h has an embedding model.
// The descriptions are written for the model: they are what makes it
// record its sources, so word changes matter.
func List(h Host) []Tool {
	list := []Tool{
		{
			Name: "add_lot",
			Description: "Register a source you just read (a user message, a web page, a tool result) " +
				"and get its id. Add a lot before remembering anything learned from it. " +
				"Only for new information from outside: never for your own conclusions or for a restatement of earlier memories. " +
				"Use remember with legs for those, or a recall of the original source will miss the copy.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"source_type":{"type":"string","description":"what kind of source, e.g. user_message, web_page, weather_tool"},` +
				`"text":{"type":"string","description":"the source's content"}},` +
				`"required":["source_type","text"],"additionalProperties":false}`),
		},
		{
			Name: "remember",
			Description: "Store something you learned, with the ids of the lots or memories it came from. " +
				"legs lists the independent ways you learned it; each leg needs all of its ids. " +
				`[[3]] means "from 3"; [[1],[2]] means "from 1, or separately from 2"; [[4,6]] means "needs both 4 and 6". ` +
				"If a source is recalled, everything learned only from it is removed.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"text":{"type":"string","description":"what you learned"},` +
				`"legs":{"type":"array","minItems":1,"items":{"type":"array","minItems":1,"items":{"type":"integer","minimum":1}}}},` +
				`"required":["text","legs"],"additionalProperties":false}`),
		},
		{
			Name: "recall",
			Description: "A lot or memory turned out to be wrong, poisoned or private: remove it and every memory that depended on it. " +
				"Permanent. mode fair (default) keeps memories still backed by another source; strict removes everything it ever touched. " +
				"The text of what is removed is also deleted from disk, unless you pass erase false. " +
				"It takes two calls: the first removes nothing and returns what would be removed and a confirm code; " +
				"show that to the user, and only if they agree, call again with the same arguments and the code. " +
				"Some apps also ask the user directly.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"id":{"type":"integer","minimum":1},` +
				`"mode":{"type":"string","enum":["fair","strict"]},` +
				`"erase":{"type":"boolean","description":"default true; false keeps the removed text on disk (hidden from you) until a later compaction"},` +
				`"confirm":{"type":"string","description":"the code from the first call; leave it out the first time"}},` +
				`"required":["id"],"additionalProperties":false}`),
		},
		{
			Name:        "get",
			Description: "Read one lot or memory by id, and whether it is still alive.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"id":{"type":"integer","minimum":1}},` +
				`"required":["id"],"additionalProperties":false}`),
		},
		{
			Name: "list_memories",
			Description: "List the memories you still have, newest first, one per line as id: text. " +
				"If there are more, the last line says which before value fetches the next page.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"limit":{"type":"integer","minimum":1,"maximum":200,"description":"default 50"},` +
				`"before":{"type":"integer","minimum":1,"description":"only memories with a smaller id"}},` +
				`"additionalProperties":false}`),
		},
	}
	if h.Embed != nil {
		list = append(list, Tool{
			Name: "search",
			Description: "Find the memories closest in meaning to a question or topic, best match first, one per line as id: text. " +
				"Recalled memories never appear. Use it before answering from memory.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"query":{"type":"string","description":"what you want to know about"},` +
				`"limit":{"type":"integer","minimum":1,"maximum":50,"description":"default 10"}},` +
				`"required":["query"],"additionalProperties":false}`),
		})
	}
	return list
}

// Instructions returns the server instructions: how the tools fit
// together, which no single tool description can say. Like List, it
// depends on h, so it never mentions a tool the model does not have.
// It is kept next to the tool descriptions so all the text a model reads
// is in one place.
func Instructions(h Host) string {
	s := "This is your long-term memory. Before you remember anything, add each new source you read as a lot. " +
		"Always give remember the ids it was learned from; a conclusion links to the memories it is based on, never to a new lot. " +
		"If a source turns out to be wrong, recall it."
	if h.Embed != nil {
		s += " To find what you already know about something, use search." +
			" When the user asks you to forget something described in words, search for it, show the user what you found," +
			" and recall it only after they confirm: a recall cannot be undone." +
			" To remove a source, recall the id add_lot gave it: search finds memories, not sources."
	}
	return s
}

// Call runs tool name with its JSON arguments on h's agent and returns the
// text answer for the model. An error is also meant for the model to read
// (a dead parent, a missing argument), so it can correct its next call.
func Call(h Host, name string, args []byte) (string, error) {
	var run func(*cheesedb.Agent, []byte) (string, error)
	switch name {
	case "add_lot":
		run = addLot
	case "recall":
		return recall(h, args) // opens the store itself, around asking the person
	case "get":
		run = get
	case "list_memories":
		run = listMemories
	case "remember":
		return remember(h, args) // these two call the model first
	case "search":
		if h.Embed != nil {
			return search(h, args)
		}
	}
	if run == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	a, done, err := h.Open()
	if err != nil {
		return "", err
	}
	defer done()
	return run(a, args)
}

func addLot(a *cheesedb.Agent, args []byte) (string, error) {
	var in struct {
		SourceType string `json:"source_type"`
		Text       string `json:"text"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	if in.SourceType == "" || in.Text == "" {
		return "", errors.New("add_lot needs source_type and text")
	}
	id, err := a.AddLot(in.SourceType, []byte(in.Text))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("lot %d", id), nil
}

func remember(h Host, args []byte) (string, error) {
	var in struct {
		Text string         `json:"text"`
		Legs []cheesedb.Leg `json:"legs"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	if in.Text == "" || len(in.Legs) == 0 {
		return "", errors.New("remember needs text and at least one leg, e.g. legs [[3]]")
	}
	var vec []float32
	var embedErr error
	if h.Embed != nil {
		vec, embedErr = embedOne(h.Embed, in.Text)
	}
	a, done, err := h.Open()
	if err != nil {
		return "", err
	}
	defer done()
	// The engine itself rejects empty legs, repeated ids and dead parents.
	id, err := a.Remember([]byte(in.Text), in.Legs...)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("memory %d", id)
	if h.Embed == nil {
		return out, nil
	}
	// A memory is worth keeping even when the model is down, so a failed
	// vector is a note, not an error; `cheesedb embed` adds it later.
	if embedErr == nil {
		if embedErr = useIndex(a, h.Embed.Model(), len(vec), true); embedErr == nil {
			embedErr = a.Embed(Index, id, vec)
		}
	}
	if embedErr != nil {
		return fmt.Sprintf("%s (saved, but search will not find it: %v)", out, embedErr), nil
	}
	return out, nil
}

func search(h Host, args []byte) (string, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	if in.Query == "" {
		return "", errors.New("search needs a query")
	}
	if in.Limit == 0 {
		in.Limit = 10
	}
	if in.Limit < 0 || in.Limit > 50 {
		return "", errors.New("limit must be 1 to 50")
	}
	found, err := Find(h, in.Query, in.Limit)
	if errors.Is(err, cheesedb.ErrNoIndex) {
		return NothingYet, nil
	} else if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "no matches", nil
	}
	var b strings.Builder
	for _, f := range found {
		fmt.Fprintf(&b, "%d: %s (match %.2f)\n", f.ID, oneLine(f.Text), f.Score)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// NothingYet is what search says when the agent has no index yet.
const NothingYet = "nothing is searchable yet: new memories become searchable as they are remembered, older ones after `cheesedb embed <agent>`"

// Found is one search result: a live memory and how close it is in
// meaning to the query (cosine similarity, 1 = same direction).
type Found struct {
	ID    cheesedb.ID
	Text  string
	Score float32
}

// Find returns up to limit live memories closest in meaning to query, best
// first. It is search without the formatting, for hosts that hand the
// results to a model themselves (cheesedb ask). It returns
// cheesedb.ErrNoIndex when nothing is searchable yet.
func Find(h Host, query string, limit int) ([]Found, error) {
	if h.Embed == nil {
		return nil, errors.New("search needs an embedding model")
	}
	vec, err := embedOne(h.Embed, query)
	if err != nil {
		return nil, err
	}
	a, done, err := h.Open()
	if err != nil {
		return nil, err
	}
	defer done()
	if err := useIndex(a, h.Embed.Model(), len(vec), false); err != nil {
		return nil, err
	}
	hits, err := a.Search(Index, vec, limit)
	if err != nil {
		return nil, err
	}
	found := make([]Found, 0, len(hits))
	for _, hit := range hits {
		it, err := a.Get(hit.ID)
		if err != nil {
			return nil, err
		}
		found = append(found, Found{hit.ID, show(it), hit.Score})
	}
	return found, nil
}

// useIndex checks that the search index exists and was built with model
// (vectors of dims values), creating it if create is set. Vectors from
// different models cannot be compared, so a model change needs a rebuild.
func useIndex(a *cheesedb.Agent, model string, dims int, create bool) error {
	infos, err := a.Indexes()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(infos, func(in cheesedb.IndexInfo) bool { return in.Name == Index })
	switch {
	case i < 0 && create:
		return a.CreateIndex(Index, dims, model)
	case i < 0:
		return cheesedb.ErrNoIndex
	case infos[i].Err != nil:
		return fmt.Errorf("the search index is damaged (%v); rebuild it with: cheesedb embed <agent>", infos[i].Err)
	case infos[i].Label != model || infos[i].Dims != dims:
		return fmt.Errorf("the search index was built with %s (%d values), not %s (%d values); rebuild it with: cheesedb embed <agent>",
			infos[i].Label, infos[i].Dims, model, dims)
	}
	return nil
}

func embedOne(e Embedder, text string) ([]float32, error) {
	vecs, err := e.Embed([]string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("embedding model returned %d vectors for 1 text", len(vecs))
	}
	return vecs[0], nil
}

// embedChunk is how many texts go to the model in one request.
const embedChunk = 64

// EmbedTexts makes memories searchable that were saved without a vector,
// for example by a host's own learning step (cheesedb learn). The model is
// called first; then the store is opened once for the whole batch.
func EmbedTexts(h Host, ids []cheesedb.ID, texts []string) error {
	if h.Embed == nil {
		return errors.New("no embedding model is set up")
	}
	if len(ids) == 0 {
		return nil
	}
	vecs, err := h.Embed.Embed(texts)
	if err != nil {
		return err
	}
	if len(vecs) != len(texts) || len(vecs[0]) == 0 {
		return fmt.Errorf("embedding model returned %d vectors for %d texts", len(vecs), len(texts))
	}
	a, done, err := h.Open()
	if err != nil {
		return err
	}
	defer done()
	if err := useIndex(a, h.Embed.Model(), len(vecs[0]), true); err != nil {
		return err
	}
	items := make([]cheesedb.Embedding, len(ids))
	for i, id := range ids {
		items[i] = cheesedb.Embedding{ID: id, Vector: vecs[i]}
	}
	return a.EmbedBatch(Index, items)
}

// EmbedAll (re)builds the search index with h's model from every live
// memory, and returns how many it embedded. Use it to make older memories
// searchable, or after changing the model. The model runs while the store
// is closed; memories remembered meanwhile are picked up before it ends.
func EmbedAll(h Host) (int, error) {
	if h.Embed == nil {
		return 0, errors.New("no embedding model is set up")
	}
	done := map[cheesedb.ID]bool{}
	for first := true; ; first = false {
		ids, texts, err := memoriesToEmbed(h, done)
		if err != nil {
			return len(done), err
		}
		if len(ids) == 0 && !first {
			return len(done), nil
		}
		var vecs [][]float32
		for start := 0; start < len(texts); start += embedChunk {
			v, err := h.Embed.Embed(texts[start:min(start+embedChunk, len(texts))])
			if err != nil {
				return len(done), err
			}
			vecs = append(vecs, v...)
		}
		if len(vecs) != len(texts) {
			return len(done), fmt.Errorf("embedding model returned %d vectors for %d texts", len(vecs), len(texts))
		}
		if err := storeAll(h, first, ids, vecs, done); err != nil {
			return len(done), err
		}
		if len(ids) == 0 {
			return 0, nil // nothing to embed; the old index is gone
		}
	}
}

// memoriesToEmbed returns the live memories not in done, with their text.
func memoriesToEmbed(h Host, done map[cheesedb.ID]bool) ([]cheesedb.ID, []string, error) {
	a, closeAgent, err := h.Open()
	if err != nil {
		return nil, nil, err
	}
	defer closeAgent()
	active, err := a.Active()
	if err != nil {
		return nil, nil, err
	}
	var ids []cheesedb.ID
	var texts []string
	for _, id := range active {
		if done[id] {
			continue
		}
		it, err := a.Get(id)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		texts = append(texts, string(it.Data))
	}
	return ids, texts, nil
}

// storeAll writes the vectors, replacing the index on the first round.
// A memory recalled while the model ran is skipped: it is dead now.
func storeAll(h Host, fresh bool, ids []cheesedb.ID, vecs [][]float32, done map[cheesedb.ID]bool) error {
	a, closeAgent, err := h.Open()
	if err != nil {
		return err
	}
	defer closeAgent()
	if fresh {
		if err := a.DropIndex(Index); err != nil && !errors.Is(err, cheesedb.ErrNoIndex) {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := a.CreateIndex(Index, len(vecs[0]), h.Embed.Model()); err != nil {
			return err
		}
	}
	var items []cheesedb.Embedding
	for i, id := range ids {
		it, err := a.Get(id)
		if err != nil {
			return err
		}
		if it.Alive {
			items = append(items, cheesedb.Embedding{ID: id, Vector: vecs[i]})
		}
		done[id] = true
	}
	if len(items) == 0 {
		return nil
	}
	return a.EmbedBatch(Index, items)
}

// recall takes two calls, because a recall cannot be undone. The first
// removes nothing: it answers with what would be removed and a confirm
// code. The second, with that code, recalls, after asking the person when
// the host can (h.Confirm).
//
// The code is a keyed hash of exactly what would be removed, so if
// anything changes in between (a new memory comes to depend on the
// target), the old code no longer fits and the model gets a fresh preview
// instead. The two calls make the model look before it erases, but a model
// set on skipping the user can still make both. Only h.Confirm, answered
// by the person, is a real lock.
func recall(h Host, args []byte) (string, error) {
	var in struct {
		ID      cheesedb.ID `json:"id"`
		Mode    string      `json:"mode"`
		Erase   *bool       `json:"erase"` // nil when absent, so we can tell "not given" from false
		Confirm string      `json:"confirm"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	// Fair unless asked: a model's mistake removes as few memories as
	// possible. But the text is erased unless erase is false, because the
	// point of recalling a poisoned or private source is that it is gone,
	// and text left in the files is what a later leak would find.
	mode := cheesedb.Fair
	switch in.Mode {
	case "", "fair":
	case "strict":
		mode = cheesedb.Strict
	default:
		return "", fmt.Errorf("mode must be fair or strict, not %q", in.Mode)
	}
	reason := cheesedb.Erase
	if in.Erase != nil && !*in.Erase {
		reason = cheesedb.Untrusted
	}
	plan := func(a *cheesedb.Agent) (recallPlan, error) { return planRecall(a, in.ID, mode, reason) }
	const stale = "Nothing recalled: that confirm code does not fit. What the recall would remove has changed, or the code is from another preview.\n"
	if in.Confirm == "" {
		p, err := withAgent(h, plan)
		if err != nil {
			return "", err
		}
		return p.forModel(), nil
	}
	if h.Confirm != nil {
		p, err := withAgent(h, plan)
		if err != nil {
			return "", err
		}
		if p.code != in.Confirm { // check before bothering the person
			return stale + p.forModel(), nil
		}
		yes, err := h.Confirm(p.forPerson())
		if err != nil {
			return "", fmt.Errorf("nothing recalled: the user could not be asked: %w", err)
		}
		if !yes {
			return fmt.Sprintf("Nothing recalled: the user said no to recalling %d.", in.ID), nil
		}
	}
	// Plan again in the same open as the recall, so nothing can change in
	// between (the store was closed while the person decided).
	return withAgent(h, func(a *cheesedb.Agent) (string, error) {
		p, err := plan(a)
		if err != nil {
			return "", err
		}
		if p.code != in.Confirm {
			return stale + p.forModel(), nil
		}
		removed, err := a.Recall(in.ID, mode, reason)
		if err != nil {
			return "", err
		}
		if len(removed) == 0 {
			return fmt.Sprintf("recalled %d; no memories removed", in.ID), nil
		}
		return fmt.Sprintf("recalled %d; memories removed: %s", in.ID, joinIDs(removed)), nil
	})
}

// withAgent opens h's agent, runs f on it and closes it again.
func withAgent[T any](h Host, f func(*cheesedb.Agent) (T, error)) (T, error) {
	a, done, err := h.Open()
	if err != nil {
		var zero T
		return zero, err
	}
	defer done()
	return f(a)
}

// confirmKey signs recall's confirm codes. It is random for each process,
// so a code can only come from a preview this program gave; after a
// restart the model simply gets a new preview.
var confirmKey = func() []byte {
	k := make([]byte, 32)
	rand.Read(k) // never fails: since Go 1.24 it crashes the program instead
	return k
}()

// previewLines caps the memories a preview lists; the code covers them all.
const previewLines = 20

// recallPlan is what a recall would do if it ran now.
type recallPlan struct {
	what  string   // the target, as `lot 3 "I live in Pune"`
	n     int      // how many memories it would remove
	lines []string // the first previewLines of them, as `4: "Pune is rainy"`
	erase bool
	code  string
}

func planRecall(a *cheesedb.Agent, id cheesedb.ID, mode cheesedb.Mode, reason cheesedb.Reason) (recallPlan, error) {
	removed, err := a.WouldRecall(id, mode) // also refuses an id that is not a lot or memory
	if err != nil {
		return recallPlan{}, err
	}
	it, err := a.Get(id)
	if err != nil {
		return recallPlan{}, err
	}
	kind := "memory"
	if it.Kind == cheesedb.KindLot {
		kind = "lot"
	}
	// Texts are quoted, so a memory cannot fake a line of the preview.
	p := recallPlan{what: fmt.Sprintf("%s %d %q", kind, id, clip(show(it))), n: len(removed), erase: reason == cheesedb.Erase}
	for _, r := range removed[:min(len(removed), previewLines)] {
		ri, err := a.Get(r)
		if err != nil {
			return recallPlan{}, err
		}
		p.lines = append(p.lines, fmt.Sprintf("%d: %q", r, clip(show(ri))))
	}
	b := binary.LittleEndian.AppendUint64(nil, uint64(id))
	b = append(b, byte(mode), byte(reason))
	for _, r := range removed {
		b = binary.LittleEndian.AppendUint64(b, uint64(r))
	}
	mac := hmac.New(sha256.New, confirmKey)
	mac.Write(b)
	p.code = hex.EncodeToString(mac.Sum(nil)[:6])
	return p, nil
}

// effect says what the recall would do, for the model and the person alike.
func (p recallPlan) effect() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Recalling %s would remove ", p.what)
	switch p.n {
	case 0:
		b.WriteString("no memories.")
	case 1:
		b.WriteString("1 memory:")
	default:
		fmt.Fprintf(&b, "%d memories:", p.n)
	}
	for _, l := range p.lines {
		b.WriteString("\n  " + l)
	}
	if more := p.n - len(p.lines); more > 0 {
		fmt.Fprintf(&b, "\n  and %d more", more)
	}
	if p.erase {
		b.WriteString("\nIts text, and the text of everything removed with it, is deleted from disk.")
	} else {
		b.WriteString("\nThe text stays on disk, hidden, until a compaction.")
	}
	return b.String()
}

func (p recallPlan) forModel() string {
	return "Nothing recalled yet. " + p.effect() +
		"\nThis cannot be undone. Show the user what would be removed. Only if they agree, call recall again with the same arguments and confirm \"" +
		p.code + "\"."
}

func (p recallPlan) forPerson() string {
	return "Your AI assistant wants to recall something from its cheesedb memory.\n" + p.effect() + "\nThis cannot be undone."
}

// clip shortens s to 80 characters for a preview line.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= 80 {
		return s
	}
	return string([]rune(s)[:80]) + "…"
}

func get(a *cheesedb.Agent, args []byte) (string, error) {
	var in struct {
		ID cheesedb.ID `json:"id"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	it, err := a.Get(in.ID)
	if err != nil {
		return "", err
	}
	kind, state := "memory", "alive"
	if it.Kind == cheesedb.KindLot {
		kind = "lot"
	}
	if !it.Alive {
		state = "recalled"
	}
	return fmt.Sprintf("%s %d (%s): %s", kind, it.ID, state, oneLine(show(it))), nil
}

func listMemories(a *cheesedb.Agent, args []byte) (string, error) {
	var in struct {
		Limit  int         `json:"limit"`
		Before cheesedb.ID `json:"before"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 0 || in.Limit > 200 {
		return "", errors.New("limit must be 1 to 200")
	}
	ids, err := a.Active() // live memories, oldest first
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for i := len(ids) - 1; i >= 0; i-- { // newest first
		id := ids[i]
		if in.Before != 0 && id >= in.Before {
			continue
		}
		if n == in.Limit {
			fmt.Fprintf(&b, "more: call again with before=%d", ids[i+1])
			return b.String(), nil
		}
		it, err := a.Get(id)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%d: %s\n", id, oneLine(show(it)))
		n++
	}
	if n == 0 {
		return "no memories", nil
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// decode reads a tool's arguments strictly: an unknown field (say "from"
// instead of "legs") is an error the model can see, not silently ignored.
func decode(args []byte, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = []byte("{}") // a tool called with no arguments at all
	}
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

// show returns an item's content as text for the model. The text of a
// recalled item is never shown, even while it is still on disk (recalled
// with erase false, or by a Go program with Untrusted): it was recalled
// because it was wrong or poisoned, and showing it would hand it back to
// the model that was meant to forget it.
func show(it cheesedb.Item) string {
	switch {
	case it.Redacted:
		return "(erased)"
	case !it.Alive:
		return "(withheld: recalled)"
	case !utf8.Valid(it.Data):
		// Written by a Go program, not through these tools (they store
		// only text); raw bytes would garble the model's input.
		return fmt.Sprintf("(binary, %d bytes)", len(it.Data))
	}
	return string(it.Data)
}

// oneLine keeps a text on its own line in a tool's answer. Text with a
// line break, another control character or a bidi override is quoted, so a
// memory cannot fake extra lines (another id, or the paging hint) and lead
// the model to the wrong one. Plain text is shown as it is.
func oneLine(s string) string {
	if strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control, unicode.Zl, unicode.Zp)
	}) {
		return strconv.Quote(s)
	}
	return s
}

func joinIDs(ids []cheesedb.ID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatUint(uint64(id), 10)
	}
	return strings.Join(s, ", ")
}
