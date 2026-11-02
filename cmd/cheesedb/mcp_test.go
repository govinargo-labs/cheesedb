package main

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/govinargo-labs/cheesedb"
)

// newOpener creates agent alice in a temporary store and returns the store
// folder and an opener for it, as mcpCommand would build from the name.
func newOpener(t *testing.T) (string, opener) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	s, err := cheesedb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateAgent("alice")
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	return dir, func() (*cheesedb.Store, *cheesedb.Agent, error) {
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
}

// session feeds lines to serveMCP and returns every line it wrote. It also
// checks that each output line is one complete JSON message: anything else
// on stdout would break the app's connection.
func session(t *testing.T, open opener, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := serveMCP(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, open); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q", l)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// toolText returns the text of a tools/call answer and whether it is an error.
func toolText(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	c := r["content"].([]any)[0].(map[string]any)
	return c["text"].(string), r["isError"].(bool)
}

func callLine(id, tool, args string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

// The start of every session: the app says hello, we agree on a version,
// the app's "initialized" notification gets no answer, and the tool list
// has the five tools.
func TestMCPHandshake(t *testing.T) {
	_, open := newOpener(t)
	msgs := session(t, open,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`,
	)
	if len(msgs) != 2 {
		t.Fatalf("got %d answers, want 2 (the notification must not be answered): %v", len(msgs), msgs)
	}
	init := msgs[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("version %v, want the client's 2025-06-18", init["protocolVersion"])
	}
	if msgs[1]["id"] != "two" {
		t.Errorf("id %v, want the string id echoed back", msgs[1]["id"])
	}
	if n := len(msgs[1]["result"].(map[string]any)["tools"].([]any)); n != 5 {
		t.Errorf("%d tools, want 5", n)
	}

	// A version we do not speak gets our newest.
	msgs = session(t, open, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := msgs[0]["result"].(map[string]any)["protocolVersion"]; v != mcpVersions[0] {
		t.Errorf("version %v, want %s", v, mcpVersions[0])
	}
}

// The README story through MCP messages, as Claude Code would send them.
func TestMCPReferenceExample(t *testing.T) {
	_, open := newOpener(t)
	msgs := session(t, open,
		callLine("1", "add_lot", `{"source_type":"user_message","text":"I live in Pune"}`),
		callLine("2", "add_lot", `{"source_type":"weather_tool","text":"Pune: rain"}`),
		callLine("3", "remember", `{"text":"user lives in Pune","legs":[[1]]}`),
		callLine("4", "remember", `{"text":"Pune is rainy","legs":[[2]]}`),
		callLine("5", "recall", `{"id":2}`),
		callLine("6", "list_memories", `{}`),
	)
	want := []string{"lot 1", "lot 2", "memory 3", "memory 4", "recalled 2; memories removed: 4", "3: user lives in Pune"}
	for i, m := range msgs {
		if got, isErr := toolText(t, m); got != want[i] || isErr {
			t.Errorf("call %d = %q (isError %v), want %q", i+1, got, isErr, want[i])
		}
	}
}

// What goes wrong must come back as an answer, never as a crash, and the
// session must go on after it.
func TestMCPErrors(t *testing.T) {
	_, open := newOpener(t)
	msgs := session(t, open,
		`this is not json`,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
		callLine("2", "remember", `{"text":"x","legs":[[99]]}`), // unknown parent
		callLine("3", "remember", `{"text":"x","agent":"bob"}`), // no tool takes an agent
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":5,"method":"ping"}`,
	)
	if len(msgs) != 6 {
		t.Fatalf("got %d answers, want 6", len(msgs))
	}
	code := func(m map[string]any) float64 { return m["error"].(map[string]any)["code"].(float64) }
	if code(msgs[0]) != -32700 || msgs[0]["id"] != nil {
		t.Errorf("broken JSON: %v, want parse error with id null", msgs[0])
	}
	if code(msgs[1]) != -32601 {
		t.Errorf("unknown method: %v, want -32601", msgs[1])
	}
	for _, m := range msgs[2:4] {
		// A failed tool is a result the model reads, not a protocol error.
		if text, isErr := toolText(t, m); !isErr {
			t.Errorf("failed tool not marked isError: %q", text)
		}
	}
	if code(msgs[4]) != -32602 {
		t.Errorf("tools/call without a name: %v, want -32602", msgs[4])
	}
	if _, ok := msgs[5]["result"]; !ok {
		t.Errorf("ping: %v, want a result", msgs[5])
	}
}

// A line past the cap gets one error and is skipped whole; the session
// goes on with the next line instead of growing memory or dying.
func TestMCPLineTooLong(t *testing.T) {
	_, open := newOpener(t)
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("x", maxLine) + `"}`
	msgs := session(t, open, huge, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(msgs) != 2 {
		t.Fatalf("got %d answers, want 2: %v", len(msgs), msgs)
	}
	if e := msgs[0]["error"].(map[string]any); e["code"].(float64) != -32700 {
		t.Errorf("long line: %v, want -32700", msgs[0])
	}
	if _, ok := msgs[1]["result"]; !ok || msgs[1]["id"].(float64) != 2 {
		t.Errorf("line after the long one: %v, want a ping result", msgs[1])
	}
}

// Between tool calls the store is closed, so another program (a second app
// window, the cheesedb command) can open it while the session is running.
func TestMCPReleasesStoreBetweenCalls(t *testing.T) {
	dir, open := newOpener(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveMCP(inR, outW, open); outW.Close() }()

	io.WriteString(inW, callLine("1", "add_lot", `{"source_type":"user_message","text":"hi"}`)+"\n")
	if _, err := bufio.NewReader(outR).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// The session is still open (stdin not closed), yet the store is free.
	s, err := cheesedb.Open(dir)
	if err != nil {
		t.Fatalf("store still locked between calls: %v", err)
	}
	s.Close()

	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
