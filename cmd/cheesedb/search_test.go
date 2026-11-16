package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/govinargo-labs/cheesedb"
	"github.com/govinargo-labs/cheesedb/internal/tools"
)

// fakeEmbedServer answers /embeddings like an OpenAI-compatible server, in
// reverse order (as some servers may), with one value per known word. It
// records the Authorization header it last saw.
func fakeEmbedServer(t *testing.T, auth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		fakeEmbed(w, r, auth)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeEmbed(w http.ResponseWriter, r *http.Request, auth *string) {
	words := []string{"pune", "rain", "tea", "friday"}
	if auth != nil {
		*auth = r.Header.Get("Authorization")
	}
	var in struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	var data []item
	for i := len(in.Input) - 1; i >= 0; i-- {
		v := make([]float32, len(words)+1)
		v[len(words)] = 0.1
		for j, w := range words {
			if strings.Contains(strings.ToLower(in.Input[i]), w) {
				v[j] = 1
			}
		}
		data = append(data, item{i, v})
	}
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// withHome points the command at a temporary home with this llm.conf, and
// clears the settings' environment variables, which would win.
func withHome(t *testing.T, conf string) {
	t.Helper()
	old := homeDir
	homeDir = t.TempDir()
	t.Cleanup(func() { homeDir = old })
	for _, env := range llmSettings {
		t.Setenv(env, "")
	}
	if err := os.WriteFile(filepath.Join(homeDir, "llm.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedderSettings(t *testing.T) {
	withHome(t, "base_url = http://chat.example/v1\nmodel = chat\napi_key = chat-key\n")
	if e, err := newEmbedder(); e != nil || err != nil {
		t.Fatalf("no embed_model: got %v, %v; want search off", e, err)
	}

	// Same server as chat: its key is used.
	withHome(t, "base_url = http://chat.example/v1\napi_key = chat-key\nembed_model = emb\n")
	e, err := newEmbedder()
	if err != nil || e.base != "http://chat.example/v1" || e.key != "chat-key" {
		t.Fatalf("got %+v, %v", e, err)
	}

	// Another server: the chat key must never be sent there.
	withHome(t, "base_url = http://chat.example/v1\napi_key = chat-key\nembed_model = emb\nembed_base_url = http://other.example/v1\n")
	if e, err := newEmbedder(); err != nil || e.key != "" {
		t.Fatalf("the chat key went to another server: %+v, %v", e, err)
	}

	withHome(t, "embed_model = emb\n")
	if _, err := newEmbedder(); err == nil {
		t.Fatal("a model with no server was accepted")
	}
}

// The client sends the key and puts the vectors back in text order, even
// when the server answers in another order.
func TestEmbedderOrderAndKey(t *testing.T) {
	var auth string
	srv := fakeEmbedServer(t, &auth)
	e := &embedder{base: srv.URL + "/v1", key: "k", model: "m", client: srv.Client()}
	vecs, err := e.Embed([]string{"rain", "tea"})
	if err != nil {
		t.Fatal(err)
	}
	if vecs[0][1] != 1 || vecs[1][2] != 1 {
		t.Fatalf("vectors out of order: %v", vecs)
	}
	if auth != "Bearer k" {
		t.Fatalf("Authorization = %q", auth)
	}
}

// Over MCP with an embedding model: search is listed, remember makes a
// memory searchable, search finds it, and a recall removes it.
func TestMCPSearch(t *testing.T) {
	_, open := newOpener(t)
	srv := fakeEmbedServer(t, nil)
	s := startSession(t, host(open, &embedder{base: srv.URL + "/v1", model: "fake", client: srv.Client()}))
	s.send(`{"jsonrpc":"2.0","id":"0","method":"tools/list"}`)
	if list, _ := json.Marshal(s.next()); !strings.Contains(string(list), `"name":"search"`) {
		t.Fatalf("tools/list has no search: %s", list)
	}
	s.tool("1", "add_lot", `{"source_type":"weather_tool","text":"Pune: rain on Friday"}`)
	if got := s.tool("2", "remember", `{"text":"Pune is rainy on Friday","legs":[[1]]}`); got != "memory 2" {
		t.Errorf("remember = %q", got)
	}
	s.tool("3", "add_lot", `{"source_type":"user_message","text":"I like tea"}`)
	s.tool("4", "remember", `{"text":"the user likes tea","legs":[[3]]}`)
	if got := s.tool("5", "search", `{"query":"is it raining?","limit":1}`); !strings.HasPrefix(got, "2: Pune is rainy on Friday (match ") {
		t.Errorf("search = %q", got)
	}
	s.recall("6", `{"id":1}`)
	if got := s.tool("7", "search", `{"query":"is it raining?","limit":1}`); !strings.HasPrefix(got, "4: the user likes tea (match ") {
		t.Errorf("search after the recall = %q", got)
	}
}

// ask is search, then the chat model: the model must be given only the
// memories search found, and never a recalled one.
func TestAskSendsSearchResults(t *testing.T) {
	var sent string // the last question and memories the chat model got
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) { fakeEmbed(w, r, nil) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		sent = in.Messages[len(in.Messages)-1].Content
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "ok"}}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	useHome(t, "base_url = "+srv.URL+"/v1\nmodel = chat\nembed_model = words\n")
	for _, args := range [][]string{
		{"agent", "create", "alice"},
		{"lot", "alice", "user_message", "I like tea"},
		{"lot", "alice", "weather_tool", "Pune: rain on Friday"},
		{"remember", "alice", "the user likes tea", "1"},
		{"remember", "alice", "Pune is rainy on Friday", "2"},
		{"ask", "alice", "is it raining?", "1"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if !strings.Contains(sent, "Pune is rainy on Friday") || strings.Contains(sent, "tea") {
		t.Fatalf("the model got %q, want only the rain memory", sent)
	}

	for _, args := range [][]string{
		{"recall", "alice", "2", "fair", "erase"},
		{"ask", "alice", "is it raining?", "1"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if strings.Contains(sent, "rainy") || !strings.Contains(sent, "Question: is it raining?") {
		t.Fatalf("after the recall the model got %q", sent)
	}

	// Without an embedding model, ask says how to set one up.
	if err := os.WriteFile(filepath.Join(homeDir, "llm.conf"), []byte("base_url = "+srv.URL+"/v1\nmodel = chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"ask", "alice", "is it raining?"}); err == nil || !strings.Contains(err.Error(), "needs an embedding model") {
		t.Fatalf("ask without an embedding model: %v", err)
	}
}

// useHome runs the command in a temporary CHEESEDB_DIR with this llm.conf,
// and clears the settings' environment variables, which would win.
func useHome(t *testing.T, conf string) {
	t.Helper()
	old := [4]string{homeDir, dataDir, tokenDir, demoDir}
	t.Cleanup(func() { homeDir, dataDir, tokenDir, demoDir = old[0], old[1], old[2], old[3] })
	t.Setenv("CHEESEDB_DIR", t.TempDir())
	for _, env := range llmSettings {
		t.Setenv(env, "")
	}
	if err := setDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, "llm.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
}

// forget erases only after a yes, and then removes the closest memory and
// what was learned from it, but not memories that do not depend on it, nor
// the source: forget works on memories. Its preview lists what goes, with
// texts quoted so control characters cannot fake lines.
func TestForget(t *testing.T) {
	srv := fakeEmbedServer(t, nil)
	useHome(t, "embed_base_url = "+srv.URL+"/v1\nembed_model = words\n")
	for _, args := range [][]string{
		{"agent", "create", "alice"},
		{"lot", "alice", "weather_tool", "Pune: rain on Friday"},
		{"lot", "alice", "user_message", "I like tea"},
		{"remember", "alice", "Pune is rainy on Friday", "1"},           // 3
		{"remember", "alice", "take an umbrella on Friday\x1b[2K", "3"}, // 4, learned from 3
		{"remember", "alice", "the user likes tea", "2"},                // 5
	} {
		if err := run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	alive := func() []cheesedb.ID {
		t.Helper()
		s, a, err := openAgent(dataDir, "alice")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		defer a.Close()
		ids, err := a.Active()
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	oldIn, oldOut := stdin, stdout
	t.Cleanup(func() { stdin, stdout = oldIn, oldOut })
	var out strings.Builder
	stdout = &out

	stdin = strings.NewReader("n\n")
	if err := run([]string{"forget", "alice", "is it rainy in Pune?"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`3: "Pune is rainy on Friday"`, `4: "take an umbrella on Friday\x1b[2K"`, "nothing erased"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("preview has no %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "likes tea") {
		t.Errorf("preview printed a raw control character, or a memory that stays:\n%s", out.String())
	}
	if got := alive(); !slices.Equal(got, []cheesedb.ID{3, 4, 5}) {
		t.Fatalf("after no: alive %v, want all three", got)
	}

	stdin = strings.NewReader("y\n")
	if err := run([]string{"forget", "alice", "is it rainy in Pune?"}); err != nil {
		t.Fatal(err)
	}
	if got := alive(); !slices.Equal(got, []cheesedb.ID{5}) {
		t.Fatalf("after yes: alive %v, want only the tea memory", got)
	}
	s, a, err := openAgent(dataDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer a.Close()
	for _, id := range []cheesedb.ID{3, 4} {
		if it, err := a.Get(id); err != nil || !it.Redacted {
			t.Fatalf("memory %d: %+v, %v; want its text erased", id, it, err)
		}
	}
	if it, err := a.Get(1); err != nil || !it.Alive || it.Redacted {
		t.Fatalf("lot 1: %+v, %v; forget must leave the source alive, text included", it, err)
	}
}

// answers is a stdin that runs an action before giving each line, to play
// something happening while the person reads a question.
type answers []func() string

func (a *answers) Read(p []byte) (int, error) {
	if len(*a) == 0 {
		return 0, io.EOF
	}
	line := (*a)[0]()
	*a = (*a)[1:]
	return copy(p, line), nil
}

// If something comes to depend on the memory while the person reads the
// list, forget must not erase it unseen: it shows the new list and asks
// again.
func TestForgetShowsWhatChanged(t *testing.T) {
	srv := fakeEmbedServer(t, nil)
	useHome(t, "embed_base_url = "+srv.URL+"/v1\nembed_model = words\n")
	for _, args := range [][]string{
		{"agent", "create", "alice"},
		{"lot", "alice", "weather_tool", "Pune: rain on Friday"},
		{"remember", "alice", "Pune is rainy on Friday", "1"}, // 2
	} {
		if err := run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	oldIn, oldOut := stdin, stdout
	t.Cleanup(func() { stdin, stdout = oldIn, oldOut })
	var out strings.Builder
	stdout = &out
	stdin = &answers{
		func() string { // an MCP app adds a memory built on 2 meanwhile
			if err := run([]string{"remember", "alice", "pack a raincoat", "2"}); err != nil {
				t.Fatal(err)
			}
			return "y\n"
		},
		func() string { return "y\n" },
	}
	if err := run([]string{"forget", "alice", "is it rainy in Pune?"}); err != nil {
		t.Fatal(err)
	}
	first, second, ok := strings.Cut(out.String(), "changed meanwhile")
	if !ok || strings.Contains(first, "erased memories") || !strings.Contains(second, `3: "pack a raincoat"`) ||
		!strings.Contains(second, "erased memories [2 3]") {
		t.Fatalf("output:\n%s\nwant no erase before the new list, then the new list and the erase", out.String())
	}
}

// What learn adds must be searchable, or another app sharing the memory
// would never find it (learn used to save memories without a vector).
func TestLearnIsSearchable(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) { fakeEmbed(w, r, nil) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		calls++
		reply := `{"facts": []}` // pass 2: no conclusions
		if calls == 1 {
			reply = `{"facts": [{"text": "It will rain in Pune on Friday", "sources": [[1]]}]}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	useHome(t, "base_url = "+srv.URL+"/v1\nmodel = chat\nembed_model = words\n")
	oldOut := stdout
	t.Cleanup(func() { stdout = oldOut })
	for _, args := range [][]string{
		{"agent", "create", "alice"},
		{"lot", "alice", "weather_tool", "Pune: rain on Friday"},
		{"learn", "alice"},
	} {
		if err := run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	found, err := tools.Find(hostForTest(t, "alice"), "is it raining in Pune?", 5)
	if err != nil || len(found) != 1 || found[0].Text != "It will rain in Pune on Friday" {
		t.Fatalf("search after learn = %+v, %v; want the learned memory", found, err)
	}
}

// hostForTest is agentHost for a test's home.
func hostForTest(t *testing.T, name string) tools.Host {
	t.Helper()
	h, err := agentHost(name)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A server's answer is read only up to a cap, and a URL's user name and
// password never reach a message.
func TestServerLimits(t *testing.T) {
	if _, err := readBody(strings.NewReader(strings.Repeat("x", 11)), 10); err == nil {
		t.Error("an answer past the cap was accepted")
	}
	if raw, err := readBody(strings.NewReader("0123456789"), 10); err != nil || len(raw) != 10 {
		t.Errorf("an answer at the cap: %d bytes, %v", len(raw), err)
	}
	if got := safeURL("https://me:secret@llm.example/v1"); strings.Contains(got, "secret") || strings.Contains(got, "me@") {
		t.Errorf("safeURL kept the credentials: %q", got)
	}
}
