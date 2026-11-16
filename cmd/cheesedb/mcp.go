package main

// `cheesedb mcp <agent>` serves one agent's memory to an MCP app (Claude
// Code, Cursor, ...) as the tools of internal/tools: five, and search when
// an embedding model is set up (embed.go).
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
	"strconv"
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
			if rep := a.Report(); rep.DroppedBytes > 0 || rep.ResumedRollover || rep.FinishedErase > 0 || rep.UnfinishedErase > 0 {
				fmt.Fprintf(os.Stderr, "cheesedb: opening found %+v\n", rep)
			}
		}
		return s, a, err
	}
	// Open once now, so a wrong agent name or bad settings fail at start
	// (the app shows stderr) instead of on the model's first tool call.
	s, a, err := openRetry(open)
	if err != nil {
		return err
	}
	a.Close()
	s.Close()
	emb, err := newEmbedder()
	if err != nil {
		return err
	}
	return serveMCP(os.Stdin, os.Stdout, host(open, emb))
}

// host gives the tools the agent and the embedding model, if there is one.
// The store is opened for each tool call and closed at once: it allows one
// open at a time per folder, so holding it for the whole session would lock
// out a second app window and the cheesedb command; a small agent opens in
// milliseconds. The tools call the model before opening it.
func host(open opener, emb *embedder) tools.Host {
	h := tools.Host{Open: func() (*cheesedb.Agent, func(), error) {
		s, a, err := openRetry(open)
		if err != nil {
			return nil, nil, err
		}
		return a, func() { a.Close(); s.Close() }, nil
	}}
	if emb != nil { // a nil *embedder in the interface would not be a nil Embedder
		h.Embed = emb
	}
	return h
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"` // a number or a string; absent for a notification
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	// Set only in a reply to a request of ours, which must not be answered.
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
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

// mcpSession is one connection to an app.
type mcpSession struct {
	r      *bufio.Reader
	enc    *json.Encoder   // Encode ends each message with '\n'
	h      tools.Host      // h.Confirm is set once the app says it can ask the person
	queue  [][]byte        // messages that came in while we waited for the person
	callID json.RawMessage // the tools/call being run, so the app's cancel of it can be seen
	asked  int             // numbers our own requests to the app
	// cancelled is set when the app cancels the running tools/call: it
	// then gets no answer.
	cancelled bool
}

// serveMCP answers one message per line from in, until in is closed (the
// app ends the session). Messages are handled one at a time: cheesedb
// allows one write at a time per agent anyway.
func serveMCP(in io.Reader, out io.Writer, h tools.Host) error {
	// Lines, not a json.Decoder over the whole stream: after one broken
	// line a Decoder cannot find the next message, but a line reader just
	// moves on. (bufio.Scanner would also do, but stops at 64 KiB, which a
	// long memory could pass.)
	s := &mcpSession{r: bufio.NewReader(in), enc: json.NewEncoder(out), h: h}
	for {
		var line []byte
		var tooLong bool
		var err error
		if len(s.queue) > 0 {
			line, s.queue = s.queue[0], s.queue[1:]
		} else {
			line, tooLong, err = readLine(s.r)
		}
		var resp *rpcResponse
		switch {
		case tooLong:
			resp = &rpcResponse{JSONRPC: "2.0", Error: &rpcError{-32700, "message too long"}}
		case len(line) > 0:
			resp = s.handle(line)
		}
		if resp != nil {
			if err := s.enc.Encode(resp); err != nil {
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

// maxWaiting caps the messages kept while the person decides: without it,
// a broken app could send without end and grow memory until we die.
const maxWaiting = 64

// ask puts a yes or no question to the person through the app (MCP
// "elicitation"): the app shows a form to the person, and the model cannot
// fill it in. It runs inside a tool call, so it reads the app's messages
// itself until the answer comes. A ping is answered at once (an app may
// drop a session whose pings go unanswered); other requests wait in the
// queue, and the app cancelling the tool call we are in ends the wait.
// There is no timeout: the store is closed meanwhile, so waiting blocks
// nobody else.
func (s *mcpSession) ask(question string) (bool, error) {
	s.asked++
	id := json.RawMessage(strconv.Quote(fmt.Sprintf("cheesedb-ask-%d", s.asked)))
	err := s.enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "elicitation/create",
		"params": map[string]any{
			"message": question,
			"requestedSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{"confirm": map[string]any{
					"type": "boolean", "title": "Yes, recall it for good", "default": false,
				}},
				"required": []string{"confirm"},
			},
		},
	})
	if err != nil {
		return false, err
	}
	for {
		line, tooLong, err := readLine(s.r)
		if tooLong {
			if err := s.enc.Encode(&rpcResponse{JSONRPC: "2.0", Error: &rpcError{-32700, "message too long"}}); err != nil {
				return false, err
			}
		} else if len(bytes.TrimSpace(line)) > 0 {
			// Only the envelope first: a reply whose result has an
			// unexpected shape must still be seen as the reply it is.
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &m) == nil && m.Method == "" {
				if !bytes.Equal(m.ID, id) {
					continue // an answer to a question we no longer wait for
				}
				if len(m.Error) > 0 && string(m.Error) != "null" {
					return false, errors.New("the app could not ask the person")
				}
				var r struct {
					Action  string `json:"action"` // accept, decline or cancel
					Content struct {
						Confirm bool `json:"confirm"`
					} `json:"content"`
				}
				// Only a ticked box counts. An unticked or unreadable answer is a no.
				return json.Unmarshal(m.Result, &r) == nil && r.Action == "accept" && r.Content.Confirm, nil
			}
			if err := s.whileAsking(line, m.ID, m.Method, m.Params, id); err != nil {
				return false, err
			}
		}
		if err != nil {
			return false, err // includes the app closing the connection
		}
	}
}

// errCancelled ends ask when the app cancels the tool call that asked.
var errCancelled = errors.New("the app cancelled the call")

// whileAsking handles a request, a notification or a broken line that
// arrives while ask waits for the person (method is "" for a broken line).
func (s *mcpSession) whileAsking(line []byte, msgID json.RawMessage, method string, params json.RawMessage, askID json.RawMessage) error {
	switch {
	case method == "ping" && msgID != nil:
		return s.enc.Encode(&rpcResponse{JSONRPC: "2.0", ID: msgID, Result: struct{}{}})
	case method == "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		json.Unmarshal(params, &p)
		if s.callID == nil || !bytes.Equal(p.RequestID, s.callID) {
			return nil // about another request, already answered
		}
		// Let the app close the form, and answer the call no more: the
		// spec says a cancelled request gets no response.
		s.cancelled = true
		if err := s.enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
			"params": map[string]any{"requestId": askID, "reason": "the tool call was cancelled"}}); err != nil {
			return err
		}
		return errCancelled
	case len(s.queue) < maxWaiting:
		s.queue = append(s.queue, line) // answered after this call
	case msgID != nil && method != "":
		return s.enc.Encode(&rpcResponse{JSONRPC: "2.0", ID: msgID, Error: &rpcError{-32603, "busy: waiting for the person to answer a question"}})
	}
	return nil // a notification or broken line past the cap: dropped
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
func (s *mcpSession) handle(line []byte) *rpcResponse {
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
	if req.Method == "" && (req.Result != nil || req.Error != nil) {
		return nil // a reply, such as a late answer to a question ask stopped waiting for
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = s.initialize(req.Params)
	case "ping":
		resp.Result = struct{}{}
	case "tools/list":
		resp.Result = listTools(s.h)
	case "tools/call":
		s.callID = req.ID
		result, err := callTool(req.Params, s.h)
		s.callID = nil
		if s.cancelled {
			s.cancelled = false
			return nil
		}
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
// otherwise our newest (the client then decides whether to go on). It also
// notes whether the app can ask the person a question (elicitation, since
// 2025-06-18), which recall then does before removing anything.
func (s *mcpSession) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Elicitation *struct {
				Form *struct{} `json:"form"`
				URL  *struct{} `json:"url"`
			} `json:"elicitation"`
		} `json:"capabilities"`
	}
	json.Unmarshal(params, &p) // a missing version just gets our newest
	v := mcpVersions[0]
	for _, mv := range mcpVersions {
		if mv == p.ProtocolVersion {
			v = mv
		}
	}
	// An empty elicitation object means forms; an app that offers only
	// URLs (a web page of its own) cannot show our yes or no question.
	s.h.Confirm = nil
	if e := p.Capabilities.Elicitation; e != nil && v != "2025-03-26" && (e.Form != nil || e.URL == nil) {
		s.h.Confirm = s.ask
	}
	return map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "cheesedb", "version": version()},
		"instructions":    tools.Instructions(s.h),
	}
}

func listTools(h tools.Host) any {
	type tool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	var list []tool
	for _, t := range tools.List(h) {
		list = append(list, tool{t.Name, t.Description, t.InputSchema})
	}
	return map[string]any{"tools": list}
}

// callTool runs one tool. A tool that fails (a dead parent, a bad
// argument) is still a result, marked isError, so the model reads why and
// can correct its next call; only an unreadable request is a protocol error.
func callTool(params json.RawMessage, h tools.Host) (any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, errors.New("tools/call needs a tool name")
	}
	text, err := tools.Call(h, p.Name, p.Arguments)
	if err != nil {
		text = err.Error()
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": err != nil,
	}, nil
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
