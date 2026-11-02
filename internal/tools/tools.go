// Package tools turns one agent's cheesedb memory into five tools a model
// can call: add_lot, remember, recall, get and list_memories.
//
// A model cannot call Go functions. It sends a tool name and its arguments
// as JSON, and reads text back. List describes the tools; Call runs one.
// The package knows nothing about MCP or any LLM API, so the same tools can
// be served over MCP (cmd/cheesedb) or handed to a model through function
// calling.
//
// Call takes the agent handle from its caller and no tool takes an agent
// name or id, so a model can only ever reach the agent it was given.
package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/govinargo-labs/cheesedb"
)

// Tool describes one tool for a model: its name, what it is for, and its
// arguments as a JSON Schema.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// List returns the five tools. The descriptions are written for the model:
// they are what makes it record its sources, so word changes matter.
func List() []Tool {
	return []Tool{
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
				"The text of what is removed is also deleted from disk, unless you pass erase false.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{` +
				`"id":{"type":"integer","minimum":1},` +
				`"mode":{"type":"string","enum":["fair","strict"]},` +
				`"erase":{"type":"boolean","description":"default true; false keeps the removed text on disk (hidden from you) until a later compaction"}},` +
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
}

// Call runs tool name with its JSON arguments on agent a and returns the
// text answer for the model. An error is also meant for the model to read
// (a dead parent, a missing argument), so it can correct its next call.
func Call(a *cheesedb.Agent, name string, args []byte) (string, error) {
	switch name {
	case "add_lot":
		return addLot(a, args)
	case "remember":
		return remember(a, args)
	case "recall":
		return recall(a, args)
	case "get":
		return get(a, args)
	case "list_memories":
		return listMemories(a, args)
	}
	return "", fmt.Errorf("unknown tool %q", name)
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

func remember(a *cheesedb.Agent, args []byte) (string, error) {
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
	// The engine itself rejects empty legs, repeated ids and dead parents.
	id, err := a.Remember([]byte(in.Text), in.Legs...)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("memory %d", id), nil
}

func recall(a *cheesedb.Agent, args []byte) (string, error) {
	var in struct {
		ID    cheesedb.ID `json:"id"`
		Mode  string      `json:"mode"`
		Erase *bool       `json:"erase"` // nil when absent, so we can tell "not given" from false
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
	removed, err := a.Recall(in.ID, mode, reason)
	if err != nil {
		return "", err
	}
	if len(removed) == 0 {
		return fmt.Sprintf("recalled %d; no memories removed", in.ID), nil
	}
	return fmt.Sprintf("recalled %d; memories removed: %s", in.ID, joinIDs(removed)), nil
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
	return fmt.Sprintf("%s %d (%s): %s", kind, it.ID, state, show(it)), nil
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
		fmt.Fprintf(&b, "%d: %s\n", id, show(it))
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

func joinIDs(ids []cheesedb.ID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatUint(uint64(id), 10)
	}
	return strings.Join(s, ", ")
}
