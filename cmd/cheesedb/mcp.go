package main

// `cheesedb mcp <agent>` serves one agent's memory to an MCP app (Claude
// Code, Cursor, ...) as the five tools of internal/tools.
//
// The app starts this program and talks to it over stdin and stdout: one
// JSON-RPC 2.0 message per line. Only protocol messages may go to stdout;
// one stray print there breaks the session, so notes go to stderr.
//
// The agent is fixed by the command line, which the app reads from its
// settings. The model only picks a tool and its arguments, and no tool
// takes an agent, so the model cannot switch to another agent through
// these tools. A model that can also run shell commands as you is another
// matter: it can skip the tools and read every agent's files, so isolation
// holds only when the tools are the model's only way to the data (see the
// MCP section of THREAT_MODEL.md).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/govinargo-labs/cheesedb"
	"github.com/govinargo-labs/cheesedb/internal/tools"
)

// The MCP versions this server speaks, newest first.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// opener opens the store and the agent for one tool call.
type opener func() (*cheesedb.Store, *cheesedb.Agent, error)

func mcpCommand(name string) error {
	open := func() (*cheesedb.Store, *cheesedb.Agent, error) {
		s, a, err := openAgent(dataDir, name)
		if err == nil {
			if rep := a.Report(); rep.DroppedBytes > 0 || rep.ResumedRollover || rep.FinishedErase > 0 {
				fmt.Fprintf(os.Stderr, "cheesedb: opening found %+v\n", rep)
			}
		}
		return s, a, err
	}
	// Open once now, so a wrong agent name fails at start (the app shows
	// stderr) instead of on the model's first tool call.
	s, a, err := openRetry(open)
	if err != nil {
		return err
	}
	a.Close()
	s.Close()
	return serveMCP(os.Stdin, os.Stdout, open)
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"` // a number or a string; absent for a notification
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // echoed unchanged; null if the request was unreadable
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// maxLine is the longest message we accept. A memory body is at most 1 MiB,
// and JSON escaping can make text up to six times longer ("\u0000" for one
// byte), so 8 MiB fits any valid call. Without a cap, one endless line from
// a broken app would grow in memory until the process dies.
const maxLine = 8 << 20

// serveMCP answers one message per line from in, until in is closed (the
// app ends the session). Messages are handled one at a time: cheesedb
// allows one write at a time per agent anyway.
func serveMCP(in io.Reader, out io.Writer, open opener) error {
	// Lines, not a json.Decoder over the whole stream: after one broken
	// line a Decoder cannot find the next message, but a line reader just
	// moves on. (bufio.Scanner would also do, but stops at 64 KiB, which a
	// long memory could pass.)
	r := bufio.NewReader(in)
	enc := json.NewEncoder(out) // Encode ends each message with '\n'
	for {
		line, tooLong, err := readLine(r)
		var resp *rpcResponse
		switch {
		case tooLong:
			resp = &rpcResponse{JSONRPC: "2.0", Error: &rpcError{-32700, "message too long"}}
		case len(line) > 0:
			resp = handle(line, open)
		}
		if resp != nil {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readLine reads up to the next '\n'. A line longer than maxLine is read to
// its end and thrown away (tooLong), so the next message starts cleanly.
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > maxLine {
				line, tooLong = nil, true
			} else {
				line = append(line, chunk...)
			}
		}
		if err != bufio.ErrBufferFull { // a chunk filled the buffer: keep reading the same line
			return line, tooLong, err
		}
	}
}

// handle answers one message, or returns nil when no answer is due.
func handle(line []byte, open opener) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil // a blank line
		}
		return &rpcResponse{JSONRPC: "2.0", Error: &rpcError{-32700, "parse error: " + err.Error()}}
	}
	if req.ID == nil {
		return nil // a notification (such as notifications/initialized): never answered
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = initialize(req.Params)
	case "ping":
		resp.Result = struct{}{}
	case "tools/list":
		resp.Result = listTools()
	case "tools/call":
		result, err := callTool(req.Params, open)
		if err != nil {
			resp.Error = &rpcError{-32602, err.Error()}
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp
}

// initialize agrees on a protocol version: the client's if we speak it,
// otherwise our newest (the client then decides whether to go on).
func initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(params, &p) // a missing version just gets our newest
	v := mcpVersions[0]
	for _, s := range mcpVersions {
		if s == p.ProtocolVersion {
			v = s
		}
	}
	return map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "cheesedb", "version": version()},
		"instructions": "This is your long-term memory. Before you remember anything, add each new source you read as a lot. " +
			"Always give remember the ids it was learned from; a conclusion links to the memories it is based on, never to a new lot. " +
			"If a source turns out to be wrong, recall it.",
	}
}

func listTools() any {
	type tool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	var list []tool
	for _, t := range tools.List() {
		list = append(list, tool{t.Name, t.Description, t.InputSchema})
	}
	return map[string]any{"tools": list}
}

// callTool runs one tool. A tool that fails (a dead parent, a bad
// argument) is still a result, marked isError, so the model reads why and
// can correct its next call; only an unreadable request is a protocol error.
func callTool(params json.RawMessage, open opener) (any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, errors.New("tools/call needs a tool name")
	}
	text, err := runTool(p.Name, p.Arguments, open)
	if err != nil {
		text = err.Error()
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": err != nil,
	}, nil
}

// runTool opens the store just for this call and closes it at once. The
// store allows one open at a time per folder, so holding it for the whole
// session would lock out a second app window and the cheesedb command; a
// small agent opens in milliseconds.
func runTool(name string, args json.RawMessage, open opener) (string, error) {
	s, a, err := openRetry(open)
	if err != nil {
		return "", err
	}
	defer s.Close()
	defer a.Close()
	return tools.Call(a, name, args)
}

// openRetry retries for up to a second while another program has the
// store open: two windows calling at the same instant just take turns.
func openRetry(open opener) (*cheesedb.Store, *cheesedb.Agent, error) {
	deadline := time.Now().Add(time.Second)
	for {
		s, a, err := open()
		if !errors.Is(err, cheesedb.ErrLocked) || time.Now().After(deadline) {
			return s, a, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
