// Command vector-search shows cheesedb's vector index end to end: store
// sources and what was learned from them, search by meaning, recall a bad
// source, and watch its memories and their vectors disappear.
//
//	go run ./examples/vector-search
//
// It runs offline with a toy embedder that hashes words, so results follow
// shared words rather than meaning. For real embeddings, point it at any
// OpenAI-compatible server (Ollama, LM Studio, vLLM, OpenAI):
//
//	EMBED_BASE_URL=http://localhost:11434/v1 EMBED_MODEL=nomic-embed-text \
//	  go run ./examples/vector-search
//
// EMBED_API_KEY is sent as a Bearer token when set. The store lives in a
// temporary folder that is deleted at the end; -keep leaves it for a look.
//
// cheesedb itself never calls a model: computing vectors is the caller's
// job, which is what this program does.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/govinargo-labs/cheesedb"
)

func main() {
	keep := flag.Bool("keep", false, "keep the store folder and print its path")
	flag.Parse()
	if err := run(*keep); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(keep bool) error {
	emb := newEmbedder()
	fmt.Printf("embedder: %s\n", emb.label)
	if emb.client == nil {
		fmt.Println("(a stand-in that matches shared words; set EMBED_BASE_URL and EMBED_MODEL for a real model)")
	}
	fmt.Println()

	tmp, err := os.MkdirTemp("", "cheesedb-vector-search-")
	if err != nil {
		return err
	}
	if keep {
		fmt.Println("store folder:", tmp)
	} else {
		defer os.RemoveAll(tmp)
	}
	s, err := cheesedb.Open(filepath.Join(tmp, "store"))
	if err != nil {
		return err
	}
	defer s.Close()
	tok, err := s.CreateAgent("demo")
	if err != nil {
		return err
	}
	ag, err := s.OpenAgent(tok)
	if err != nil {
		return err
	}
	defer ag.Close()

	// Sources (lots), as the agent received them.
	var lots []cheesedb.ID
	for _, l := range []struct{ source, text string }{
		{"user_message", "I am vegetarian and I live in Pune."},
		{"web_page", "Pune food guide: everyone must try the chicken biryani at Hotel Kesar."},
		{"weather_tool", "Pune forecast: heavy rain on Friday evening."},
		{"calendar", "Friday 8 pm: dinner with Asha in Koregaon Park."},
	} {
		id, err := ag.AddLot(l.source, []byte(l.text))
		if err != nil {
			return err
		}
		lots = append(lots, id)
	}
	user, web, weather, calendar := lots[0], lots[1], lots[2], lots[3]

	// What the agent learned, each with the sources it came from (legs).
	memories := []struct {
		text string
		legs []cheesedb.Leg
	}{
		{"The user is vegetarian.", []cheesedb.Leg{{user}}},
		{"The user lives in Pune.", []cheesedb.Leg{{user}}},
		{"Suggest chicken biryani at Hotel Kesar for dinner in Pune.", []cheesedb.Leg{{web}}},
		{"Take an umbrella to Friday's dinner with Asha: rain is expected.", []cheesedb.Leg{{weather, calendar}}},
		{"Friday dinner is with Asha in Koregaon Park.", []cheesedb.Leg{{calendar}}},
	}
	var ids []cheesedb.ID
	var texts []string
	for _, m := range memories {
		id, err := ag.Remember([]byte(m.text), m.legs...)
		if err != nil {
			return err
		}
		ids = append(ids, id)
		texts = append(texts, m.text)
	}

	// Embed every memory, in one batch. (Lots could have vectors too; this
	// demo searches what the agent learned.)
	vecs, err := emb.embed(texts)
	if err != nil {
		return err
	}
	if err := ag.CreateIndex("default", len(vecs[0]), emb.label); err != nil {
		return err
	}
	var items []cheesedb.Embedding
	for i, id := range ids {
		items = append(items, cheesedb.Embedding{ID: id, Vector: vecs[i]})
	}
	if err := ag.EmbedBatch("default", items); err != nil {
		return err
	}
	fmt.Printf("stored %d sources and %d memories; each memory has a vector of %d values\n\n", len(lots), len(ids), len(vecs[0]))

	questions := []string{"What should I eat for dinner in Pune?", "Do I need an umbrella on Friday?"}
	ask := func() error {
		for _, q := range questions {
			if err := search(ag, emb, q); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ask(); err != nil {
		return err
	}

	// The web page was wrong (the user is vegetarian). Recall it with Erase:
	// every memory built on it dies, and its text and vectors leave the disk.
	fmt.Println("── recall the web page (Fair, Erase) ──")
	removed, err := ag.Recall(web, cheesedb.Fair, cheesedb.Erase)
	if err != nil {
		return err
	}
	fmt.Printf("memories removed with it: %v\n\n", removed)
	if err := ask(); err != nil {
		return err
	}

	// Proof on disk: the erased text is in no file of the agent.
	agentDir := filepath.Join(tmp, "store", fmt.Sprintf("%x", "demo"))
	fmt.Printf("\"biryani\" anywhere on disk: %v\n", contains(agentDir, []byte("biryani")))
	head, err := ag.Verify()
	if err != nil {
		return err
	}
	ihead, err := ag.VerifyIndex("default")
	if err != nil {
		return err
	}
	fmt.Printf("main log verifies, head  %x...\n", head[:8])
	fmt.Printf("index verifies,    head  %x...\n", ihead[:8])
	infos, err := ag.Indexes()
	if err != nil {
		return err
	}
	for _, in := range infos {
		fmt.Printf("index %q: %d live vectors of %d values (%s)\n", in.Name, in.Vectors, in.Dims, in.Label)
	}
	return nil
}

func search(ag *cheesedb.Agent, emb *embedder, q string) error {
	v, err := emb.embed([]string{q})
	if err != nil {
		return err
	}
	hits, err := ag.Search("default", v[0], 2)
	if err != nil {
		return err
	}
	fmt.Printf("Q: %s\n", q)
	for _, h := range hits {
		it, err := ag.Get(h.ID)
		if err != nil {
			return err
		}
		fmt.Printf("   %.3f  [%d] %s\n", h.Score, h.ID, it.Data)
	}
	fmt.Println()
	return nil
}

// contains reports whether b appears in any file under dir.
func contains(dir string, b []byte) bool {
	found := false
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			found = found || bytes.Contains(data, b)
		}
		return nil
	})
	return found
}

// embedder turns texts into vectors: with a real model when EMBED_BASE_URL
// and EMBED_MODEL are set, otherwise with the toy word hasher.
type embedder struct {
	label            string
	base, model, key string
	client           *http.Client
}

func newEmbedder() *embedder {
	e := &embedder{
		base:  strings.TrimRight(os.Getenv("EMBED_BASE_URL"), "/"),
		model: os.Getenv("EMBED_MODEL"),
		key:   os.Getenv("EMBED_API_KEY"),
	}
	if e.base == "" || e.model == "" {
		e.label = "toy-word-hasher"
		return e
	}
	e.label = e.model
	e.client = &http.Client{Timeout: 2 * time.Minute}
	return e
}

func (e *embedder) embed(texts []string) ([][]float32, error) {
	if e.client == nil {
		out := make([][]float32, len(texts))
		for i, t := range texts {
			out[i] = toyEmbed(t)
		}
		return out, nil
	}
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": texts})
	req, err := http.NewRequest("POST", e.base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings server: %s", resp.Status)
	}
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings server returned %d vectors for %d texts", len(r.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embeddings server returned index %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// toyEmbed is a stand-in for a model: each word adds +1 or -1 to one of
// 256 slots chosen by its hash ("feature hashing"). Texts that share words
// point in similar directions. It knows nothing about meaning.
func toyEmbed(text string) []float32 {
	const dims = 256
	v := make([]float32, dims)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) })
	for _, w := range words {
		if len(w) < 3 || stop[w] {
			continue
		}
		w = strings.TrimSuffix(w, "s")
		h := fnv.New32a()
		h.Write([]byte(w))
		x := h.Sum32()
		if x>>31 == 0 { // sign from a bit the slot does not use
			v[x%dims]++
		} else {
			v[x%dims]--
		}
	}
	v[0] += 0.01 // never all zero: cheesedb refuses a vector with no direction
	return v
}

var stop = map[string]bool{"the": true, "and": true, "for": true, "with": true, "what": true, "should": true, "need": true, "everyone": true, "must": true}
