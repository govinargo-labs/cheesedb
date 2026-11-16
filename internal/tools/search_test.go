package tools

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/govinargo-labs/cheesedb"
)

// wordEmbedder is a stand-in model with one value per known word, so texts
// that share words point the same way. It can be made to fail, and it
// checks it is never called while the store is open.
type wordEmbedder struct {
	model  string // default "words"
	extra  int    // extra values, to play a model with other dims
	fail   error
	isOpen *bool // set by hostWith while the agent is open
	t      *testing.T
}

var vocab = []string{"pune", "rain", "umbrella", "tea", "friday", "meeting", "lives"}

func (e *wordEmbedder) Model() string {
	if e.model == "" {
		return "words"
	}
	return e.model
}

func (e *wordEmbedder) Embed(texts []string) ([][]float32, error) {
	if e.isOpen != nil && *e.isOpen {
		e.t.Error("the model was called while the store was open")
	}
	if e.fail != nil {
		return nil, e.fail
	}
	var out [][]float32
	for _, text := range texts {
		v := make([]float32, len(vocab)+1+e.extra)
		v[len(vocab)] = 0.1 // never all zero
		for i, w := range vocab {
			if strings.Contains(strings.ToLower(text), w) {
				v[i] = 1
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// hostWith serves agent a with model e, and tells e when a is "open".
func hostWith(t *testing.T, a *cheesedb.Agent, e *wordEmbedder) Host {
	open := false
	e.isOpen, e.t = &open, t
	return Host{
		Open: func() (*cheesedb.Agent, func(), error) {
			open = true
			return a, func() { open = false }, nil
		},
		Embed: e,
	}
}

func callH(t *testing.T, h Host, name, args string) string {
	t.Helper()
	out, err := confirmed(h, name, args)
	if err != nil {
		t.Fatalf("%s %s: %v", name, args, err)
	}
	return out
}

// The reference example with search: remember makes each memory
// searchable, and a recall takes the dead out of the results at once.
func TestSearchFindsAndForgets(t *testing.T) {
	a := newAgent(t)
	h := hostWith(t, a, &wordEmbedder{})
	callH(t, h, "add_lot", `{"source_type":"user_message","text":"I live in Pune"}`)
	callH(t, h, "add_lot", `{"source_type":"web_page","text":"profile: lives in Pune"}`)
	callH(t, h, "add_lot", `{"source_type":"weather_tool","text":"Pune: rain"}`)
	for _, m := range []string{
		`{"text":"user lives in Pune","legs":[[1],[2]]}`,
		`{"text":"Pune is rainy","legs":[[3]]}`,
		`{"text":"carry an umbrella in the rain","legs":[[4,5]]}`,
	} {
		if out := callH(t, h, "remember", m); strings.Contains(out, "search will not find") {
			t.Fatal(out)
		}
	}
	out := callH(t, h, "search", `{"query":"will there be rain?","limit":2}`)
	if lines := strings.Split(out, "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "5: Pune is rainy (match ") {
		t.Fatalf("search = %q, want memory 5 first, two lines", out)
	}

	callH(t, h, "recall", `{"id":3}`)
	out = callH(t, h, "search", `{"query":"will there be rain?"}`)
	if strings.Contains(out, "5:") || strings.Contains(out, "6:") || !strings.HasPrefix(out, "4: user lives in Pune") {
		t.Fatalf("after the recall, search = %q, want only memory 4", out)
	}
}

// Before anything was remembered with a model there is no index, and
// search says so instead of failing.
func TestSearchNothingYet(t *testing.T) {
	h := hostWith(t, newAgent(t), &wordEmbedder{})
	if out := callH(t, h, "search", `{"query":"rain"}`); !strings.HasPrefix(out, "nothing is searchable yet") {
		t.Fatalf("search = %q", out)
	}
	if _, err := Call(h, "search", []byte(`{"query":""}`)); err == nil {
		t.Fatal("an empty query worked")
	}
}

// A model that is down must not lose the memory: it is saved, the answer
// says it is not searchable, and EmbedAll fixes that later.
func TestRememberWhenModelFails(t *testing.T) {
	a := newAgent(t)
	e := &wordEmbedder{fail: errors.New("connection refused")}
	h := hostWith(t, a, e)
	callH(t, h, "add_lot", `{"source_type":"weather_tool","text":"Pune: rain"}`)
	out := callH(t, h, "remember", `{"text":"Pune is rainy","legs":[[1]]}`)
	if !strings.HasPrefix(out, "memory 2 (saved, but search will not find it: connection refused") {
		t.Fatalf("remember = %q", out)
	}
	if _, err := Call(h, "search", []byte(`{"query":"rain"}`)); err == nil {
		t.Fatal("search worked with the model down")
	}

	e.fail = nil
	if n, err := EmbedAll(h); err != nil || n != 1 {
		t.Fatalf("EmbedAll = %d, %v", n, err)
	}
	if out := callH(t, h, "search", `{"query":"rain"}`); !strings.HasPrefix(out, "2: Pune is rainy") {
		t.Fatalf("search = %q", out)
	}
}

// Vectors of two models cannot be compared. After a model change, remember
// and search say to rebuild; EmbedAll rebuilds with the new model.
func TestModelChange(t *testing.T) {
	a := newAgent(t)
	old := hostWith(t, a, &wordEmbedder{})
	callH(t, old, "add_lot", `{"source_type":"weather_tool","text":"Pune: rain"}`)
	callH(t, old, "remember", `{"text":"Pune is rainy","legs":[[1]]}`)

	h := hostWith(t, a, &wordEmbedder{model: "words-2", extra: 3})
	if out := callH(t, h, "remember", `{"text":"rain on friday","legs":[[1]]}`); !strings.Contains(out, "rebuild it with: cheesedb embed") {
		t.Fatalf("remember after a model change = %q", out)
	}
	if _, err := Call(h, "search", []byte(`{"query":"rain"}`)); err == nil || !strings.Contains(err.Error(), "built with words") {
		t.Fatalf("search after a model change: %v", err)
	}
	if n, err := EmbedAll(h); err != nil || n != 2 {
		t.Fatalf("EmbedAll = %d, %v", n, err)
	}
	infos, _ := a.Indexes()
	if len(infos) != 1 || infos[0].Label != "words-2" || infos[0].Vectors != 2 {
		t.Fatalf("Indexes = %+v", infos)
	}
	if out := callH(t, h, "search", `{"query":"friday","limit":1}`); !strings.HasPrefix(out, "3: rain on friday") {
		t.Fatalf("search = %q", out)
	}
}

// EmbedAll embeds live memories only, and an agent with none ends with no
// index (its old one, from another model, is dropped).
func TestEmbedAllLiveOnly(t *testing.T) {
	a := newAgent(t)
	h := hostWith(t, a, &wordEmbedder{})
	callH(t, hostFor(a, nil), "add_lot", `{"source_type":"user_message","text":"tea"}`)
	callH(t, hostFor(a, nil), "remember", `{"text":"likes tea","legs":[[1]]}`)
	callH(t, hostFor(a, nil), "remember", `{"text":"tea on friday","legs":[[1]]}`)
	callH(t, hostFor(a, nil), "recall", `{"id":3}`)
	if n, err := EmbedAll(h); err != nil || n != 1 {
		t.Fatalf("EmbedAll = %d, %v; want 1 (memory 3 is recalled)", n, err)
	}
	callH(t, h, "recall", `{"id":1}`)
	if n, err := EmbedAll(h); err != nil || n != 0 {
		t.Fatalf("EmbedAll with nothing alive = %d, %v", n, err)
	}
	if infos, _ := a.Indexes(); len(infos) != 0 {
		t.Fatalf("Indexes = %+v, want none", infos)
	}
}

// The instructions name search exactly when the search tool is listed, so
// the model is never told to use a tool it does not have.
func TestInstructionsMatchTools(t *testing.T) {
	for _, h := range []Host{{}, hostFor(nil, &wordEmbedder{})} {
		listed := slices.ContainsFunc(List(h), func(tool Tool) bool { return tool.Name == "search" })
		if told := strings.Contains(Instructions(h), "search"); told != listed {
			t.Errorf("model %v: search listed %v, instructions mention it %v", h.Embed != nil, listed, told)
		}
	}
}

// Without a model there is no search tool, and remember stores no vector.
func TestNoModel(t *testing.T) {
	a := newAgent(t)
	h := hostFor(a, nil)
	callH(t, h, "add_lot", `{"source_type":"user_message","text":"tea"}`)
	callH(t, h, "remember", `{"text":"likes tea","legs":[[1]]}`)
	if infos, _ := a.Indexes(); len(infos) != 0 {
		t.Fatalf("an index was made without a model: %+v", infos)
	}
	if _, err := Call(h, "search", []byte(`{"query":"tea"}`)); err == nil || !strings.HasPrefix(err.Error(), "unknown tool") {
		t.Fatalf("search without a model: %v", err)
	}
	if _, err := EmbedAll(h); err == nil {
		t.Fatal("EmbedAll worked without a model")
	}
}
