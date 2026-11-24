package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/govinargo-labs/cheesedb"
	"github.com/govinargo-labs/cheesedb/internal/logfile"
)

const host = "127.0.0.1:7700"

// env is one store served by one Server, reached through its handler (no
// sockets: httptest calls the handler directly).
type env struct {
	t     *testing.T
	store *cheesedb.Store
	srv   *Server
	h     http.Handler
	admin string
	log   *syncBuffer
}

func newEnv(t *testing.T, o Options) *env {
	t.Helper()
	s, err := cheesedb.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, store: s, admin: strings.Repeat("ad", 32), log: &syncBuffer{}}
	if o.AdminKey == nil {
		k := AdminKeyOf(e.admin)
		o.AdminKey = &k
	}
	o.Log = e.log
	e.srv = New(s, o)
	e.h = e.srv.Handler([]string{host, "localhost:7700"})
	t.Cleanup(func() {
		if err := e.srv.Close(); err != nil {
			t.Error(err)
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return e
}

// agent creates an agent through the store and returns its token as text.
func (e *env) agent(name string) string {
	e.t.Helper()
	tok, err := e.store.CreateAgent(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok.String()
}

type reply struct {
	status int
	body   map[string]any
	header http.Header
}

func (r reply) code() string { s, _ := r.body["error"].(string); return s }

// id reads a number field, as JSON numbers decode to float64.
func (r reply) num(field string) uint64 { f, _ := r.body[field].(float64); return uint64(f) }

func (e *env) req(method, path, tok, body string, hdr ...string) reply {
	e.t.Helper()
	var b io.Reader
	if body != "" {
		b = strings.NewReader(body)
	}
	return e.send(httptest.NewRequest(method, "http://"+host+path, b), tok, hdr...)
}

func (e *env) send(r *http.Request, tok string, hdr ...string) reply {
	e.t.Helper()
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			r.Host = hdr[i+1]
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	out := reply{status: w.Code, header: w.Header()}
	if err := json.Unmarshal(w.Body.Bytes(), &out.body); err != nil {
		e.t.Fatalf("%s %s: answer is not a JSON object: %q", r.Method, r.URL.Path, w.Body.String())
	}
	return out
}

// ok fails the test unless the answer is 200.
func (e *env) ok(r reply) reply {
	e.t.Helper()
	if r.status != http.StatusOK {
		e.t.Fatalf("status %d: %v", r.status, r.body)
	}
	return r
}

func (e *env) want(r reply, status int, code string) {
	e.t.Helper()
	if r.status != status || r.code() != code {
		e.t.Fatalf("got %d %q (%v), want %d %q", r.status, r.code(), r.body["message"], status, code)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// The reference example of the project brief, over HTTP: lots A, B, C;
// M1 from A; M2 from B or C; M3 from M1 and M2. Recalling B (fair) leaves
// M2 alive through C; recalling A kills M1 and then M3.
func TestReferenceExample(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	lot := func(text string) uint64 {
		return e.ok(e.req("POST", "/v1/lots", tok, fmt.Sprintf(`{"source_type":"doc","text":%q}`, text))).num("id")
	}
	a, b, c := lot("A"), lot("B"), lot("C")
	mem := func(text, legs string) uint64 {
		return e.ok(e.req("POST", "/v1/memories", tok, fmt.Sprintf(`{"text":%q,"legs":%s}`, text, legs))).num("id")
	}
	m1 := mem("M1", fmt.Sprintf("[[%d]]", a))
	m2 := mem("M2", fmt.Sprintf("[[%d],[%d]]", b, c))
	m3 := mem("M3", fmt.Sprintf("[[%d,%d]]", m1, m2))

	r := e.ok(e.req("POST", "/v1/recall/preview", tok, fmt.Sprintf(`{"id":%d,"mode":"fair"}`, a)))
	if got := fmt.Sprint(r.body["removed"]); got != fmt.Sprintf("[%d %d]", m1, m3) {
		t.Fatalf("preview of A removes %s", got)
	}
	r = e.ok(e.req("POST", "/v1/recall", tok, fmt.Sprintf(`{"id":%d,"mode":"fair","reason":"untrusted"}`, b)))
	if got := fmt.Sprint(r.body["removed"]); got != "[]" {
		t.Fatalf("recalling B removed %s, want nothing (M2 lives through C)", got)
	}
	r = e.ok(e.req("POST", "/v1/recall", tok, fmt.Sprintf(`{"id":%d,"mode":"fair","reason":"erase"}`, a)))
	if got := fmt.Sprint(r.body["removed"]); got != fmt.Sprintf("[%d %d]", m1, m3) {
		t.Fatalf("recalling A removed %s", got)
	}

	r = e.ok(e.req("GET", fmt.Sprintf("/v1/items/%d", m2), tok, ""))
	if r.body["alive"] != true || r.body["text"] != "M2" || r.body["kind"] != "memory" {
		t.Fatalf("M2: %v", r.body)
	}
	r = e.ok(e.req("GET", fmt.Sprintf("/v1/items/%d", m1), tok, ""))
	if r.body["alive"] != false || r.body["redacted"] != true || r.body["text"] != nil {
		t.Fatalf("M1 after an erase: %v", r.body)
	}
	r = e.ok(e.req("GET", "/v1/memories", tok, ""))
	if got := fmt.Sprint(r.body["ids"]); got != fmt.Sprintf("[%d]", m2) {
		t.Fatalf("live memories %s", got)
	}
	e.ok(e.req("POST", "/v1/verify", tok, ""))
	r = e.ok(e.req("POST", "/v1/compact", tok, "{}"))
	if r.num("erased") != 1 { // B: recalled as untrusted, its text still on disk
		t.Fatalf("compact erased %v", r.body["erased"])
	}
}

// Alice's token reaches only Alice's records, even for ids Bob has.
func TestIsolation(t *testing.T) {
	e := newEnv(t, Options{})
	alice, bob := e.agent("alice"), e.agent("bob")
	e.ok(e.req("POST", "/v1/lots", bob, `{"source_type":"doc","text":"Bob's secret"}`))
	e.ok(e.req("POST", "/v1/lots", bob, `{"source_type":"doc","text":"Bob's second"}`))
	e.ok(e.req("POST", "/v1/lots", alice, `{"source_type":"doc","text":"Alice's own"}`))

	if r := e.ok(e.req("GET", "/v1/items/1", alice, "")); r.body["text"] != "Alice's own" {
		t.Fatalf("Alice's id 1 is %v", r.body["text"])
	}
	e.want(e.req("GET", "/v1/items/2", alice, ""), 404, "unknown_id")
	e.want(e.req("POST", "/v1/recall", alice, `{"id":2,"mode":"strict","reason":"erase"}`), 404, "unknown_id")
	if r := e.ok(e.req("GET", "/v1/items/2", bob, "")); r.body["alive"] != true {
		t.Fatal("Alice's recall reached Bob's record")
	}
}

// No route or body field takes an agent: an agent named in a path is no
// route at all, and an "agent" field is an unknown field.
func TestNoRouteTakesAnAgent(t *testing.T) {
	e := newEnv(t, Options{})
	alice := e.agent("alice")
	e.agent("bob")
	for _, p := range []string{"/v1/agents/bob/items/1", "/v1/bob/lots", "/v1/items/1?agent=bob"} {
		r := e.req("GET", p, alice, "")
		if p == "/v1/items/1?agent=bob" {
			// The query string is ignored: this reads Alice's own id 1.
			e.want(r, 404, "unknown_id")
			continue
		}
		e.want(r, 404, "not_found")
	}
	e.want(e.req("POST", "/v1/lots", alice, `{"agent":"bob","source_type":"doc","text":"x"}`), 400, "bad_request")
	e.want(e.req("POST", "/v1/memories", alice, `{"text":"x","legs":[[1]],"token":"`+strings.Repeat("0", 64)+`"}`), 400, "bad_request")
}

// Every token problem gets the same 401, so a caller cannot tell an
// unknown token from a malformed one, or probe which agents exist.
func TestUnauthorized(t *testing.T) {
	e := newEnv(t, Options{})
	e.agent("alice")
	var msgs []string
	for _, tok := range []string{"", "nonsense", strings.Repeat("0", 64), e.admin} {
		r := e.req("GET", "/v1/memories", tok, "")
		e.want(r, 401, "unauthorized")
		msgs = append(msgs, r.body["message"].(string))
	}
	for _, m := range msgs[1:] {
		if m != msgs[0] {
			t.Fatalf("different 401 messages: %q", msgs)
		}
	}
	// An agent's token is not the admin token.
	e.want(e.req("POST", "/v1/admin/agents", e.agent("bob"), `{"name":"carol"}`), 401, "unauthorized")
}

func TestBadBodies(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	for _, body := range []string{
		`{"source_type":"doc","text":"x","extra":1}`, // unknown field
		`{"source_type":"doc","text":"x"} {}`,        // two values
		`{"source_type":"doc","text":"x"}}`,          // junk after the value
		`{"source_type":"doc","text":"x"}]`,
		`{"source_type":"doc"`,                                  // cut off
		`{"source_type":"doc"}`,                                 // no content
		`{"source_type":"doc","text":"x","data_base64":"eA=="}`, // both
		`{"source_type":"doc","data_base64":"%%%"}`,             // not base64
	} {
		e.want(e.req("POST", "/v1/lots", tok, body), 400, "bad_request")
	}
	e.want(e.req("POST", "/v1/compact", tok, `{"x":1}`), 400, "bad_request")
	e.want(e.req("GET", "/v1/items/abc", tok, ""), 400, "bad_request")
	e.want(e.req("GET", "/v1/items/0", tok, ""), 400, "bad_request")
	e.want(e.req("GET", "/v1/memories?limit=0", tok, ""), 400, "bad_request")
	e.want(e.req("POST", "/v1/recall", tok, `{"id":1,"mode":"fair"}`), 400, "bad_request")    // no reason
	e.want(e.req("POST", "/v1/recall", tok, `{"id":1,"reason":"erase"}`), 400, "bad_request") // no mode
	e.want(e.req("DELETE", "/v1/lots", tok, ""), 404, "not_found")
}

func TestTooLarge(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	huge := fmt.Sprintf(`{"source_type":"doc","text":%q}`, strings.Repeat("x", maxBody))
	e.want(e.req("POST", "/v1/lots", tok, huge), 413, "too_large")
	// Under the body limit but over a record's 1 MiB: the engine refuses it.
	big := fmt.Sprintf(`{"source_type":"doc","text":%q}`, strings.Repeat("x", 1<<20+1))
	e.want(e.req("POST", "/v1/lots", tok, big), 413, "too_large")
}

// Browser defences: any Origin is refused, and so is a Host that is
// not this server's own (DNS rebinding), even on the health route.
func TestBrowserChecks(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	e.want(e.req("GET", "/v1/memories", tok, "", "Origin", "https://evil.example"), 403, "origin_refused")
	e.want(e.req("GET", "/v1/health", "", "", "Origin", "null"), 403, "origin_refused")
	e.want(e.req("GET", "/v1/memories", tok, "", "Host", "evil.example:7700"), 403, "bad_host")
	e.want(e.req("GET", "/v1/health", "", "", "Host", "127.0.0.1:7701"), 403, "bad_host")
	e.ok(e.req("GET", "/v1/health", "", "", "Host", "LOCALHOST:7700"))

	// A Unix socket handler has no Host check (a browser cannot reach it),
	// but still refuses Origin.
	h := e.srv.Handler(nil)
	r := httptest.NewRequest("GET", "http://anything/v1/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("unix handler health: %d", w.Code)
	}
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("unix handler with Origin: %d", w.Code)
	}
}

func TestEngineErrors(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"A"}`))
	e.ok(e.req("POST", "/v1/recall", tok, `{"id":1,"mode":"fair","reason":"untrusted"}`))
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"B"}`))

	e.want(e.req("POST", "/v1/memories", tok, `{"text":"m","legs":[[1]]}`), 409, "dead_parent")
	e.want(e.req("POST", "/v1/memories", tok, `{"text":"m","legs":[]}`), 400, "bad_legs")
	e.want(e.req("POST", "/v1/memories", tok, `{"text":"m","legs":[[2,2]]}`), 400, "bad_legs")
	e.want(e.req("POST", "/v1/memories", tok, `{"text":"m","legs":[[99]]}`), 404, "unknown_id")
	e.want(e.req("POST", "/v1/lots", tok, `{"source_type":"","text":"x"}`), 400, "bad_request")

	e.want(e.req("POST", "/v1/indexes/v/search", tok, `{"vector":[1,0,0]}`), 404, "no_index")
	e.ok(e.req("PUT", "/v1/indexes/v", tok, `{"dims":3,"label":"test"}`))
	e.want(e.req("PUT", "/v1/indexes/v", tok, `{"dims":3}`), 409, "index_exists")
	e.want(e.req("POST", "/v1/indexes/v/vectors", tok, `{"items":[{"id":2,"vector":[0,0,0]}]}`), 400, "bad_vector")
	e.want(e.req("POST", "/v1/indexes/v/vectors", tok, `{"items":[{"id":1,"vector":[1,0,0]}]}`), 409, "recalled")
	e.want(e.req("POST", "/v1/indexes/v/search", tok, `{"vector":[1,0,0],"k":0}`), 400, "bad_request")
	e.want(e.req("POST", "/v1/indexes/v/search", tok, `{"vector":[1,0,0],"k":1001}`), 400, "bad_request")

	e.want(e.req("POST", "/v1/admin/agents", e.admin, `{"name":"alice"}`), 409, "agent_exists")
	e.want(e.req("POST", "/v1/admin/agents", e.admin, `{"name":""}`), 400, "bad_name")
}

// The statuses the tests above cannot easily cause, checked on the mapping.
func TestErrStatus(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: disk full", logfile.ErrBroken), 503, "broken"},
		{cheesedb.ErrClosed, 503, "shutting_down"},
		{cheesedb.ErrCorrupt, 500, "corrupt"},
		{cheesedb.ErrAgentInUse, 409, "agent_in_use"},
		{cheesedb.ErrBadToken, 401, "unauthorized"},
		{errors.New("something else"), 500, "internal"},
	} {
		if s, code := errStatus(c.err); s != c.status || code != c.code {
			t.Errorf("%v: got %d %s, want %d %s", c.err, s, code, c.status, c.code)
		}
	}
}

// A failure on the server's side never sends its error text (it names
// files: the store folder, the agent's folder in hex), and the log keeps
// the error without the paths.
func TestServerErrorText(t *testing.T) {
	e := newEnv(t, Options{})
	agentFolder := fmt.Sprintf("%x", "alice") // how the store names alice's folder
	for _, err := range []error{
		fmt.Errorf("open /secret/store/%s/000001.seg: permission denied", agentFolder),
		fmt.Errorf("%w: write /secret/store/%s/000002.seg: no space left on device", cheesedb.ErrBroken, agentFolder),
	} {
		w := httptest.NewRecorder()
		e.srv.writeErr(w, err)
		if strings.Contains(w.Body.String(), "/secret") || strings.Contains(w.Body.String(), agentFolder) {
			t.Fatalf("answer shows the error: %s", w.Body.String())
		}
	}
	log := e.log.String()
	if strings.Contains(log, "/secret") || strings.Contains(log, agentFolder) {
		t.Fatalf("log shows a path:\n%s", log)
	}
	if !strings.Contains(log, "error: internal: open <path>: permission denied") || !strings.Contains(log, "error: broken: ") {
		t.Fatalf("log lacks the errors:\n%s", log)
	}
}

// A line break in the path cannot forge a second log line.
func TestLogInjection(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	r := httptest.NewRequest("GET", "http://"+host+"/v1/items/1%0A2026-01-01T00:00:00Z%20POST%20/v1/admin/agents%20200%201ms%20deadbeef", nil)
	e.send(r, tok)
	if lines := strings.Count(e.log.String(), "\n"); lines != 1 {
		t.Fatalf("one request wrote %d log lines:\n%s", lines, e.log.String())
	}
}

func TestSearch(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"x"}`))
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"y"}`))
	e.ok(e.req("PUT", "/v1/indexes/v", tok, `{"dims":2,"label":"toy"}`))
	r := e.ok(e.req("POST", "/v1/indexes/v/vectors", tok, `{"items":[{"id":1,"vector":[1,0]},{"id":2,"vector":[0,1]}]}`))
	if r.num("stored") != 2 {
		t.Fatalf("stored %v", r.body)
	}
	r = e.ok(e.req("POST", "/v1/indexes/v/search", tok, `{"vector":[0.1,1],"k":1}`))
	hits := r.body["hits"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["id"] != float64(2) {
		t.Fatalf("hits %v", hits)
	}
	r = e.ok(e.req("GET", "/v1/indexes", tok, ""))
	idx := r.body["indexes"].([]any)[0].(map[string]any)
	if idx["name"] != "v" || idx["vectors"] != float64(2) || idx["label"] != "toy" {
		t.Fatalf("indexes %v", r.body)
	}
	e.ok(e.req("POST", "/v1/indexes/v/verify", tok, ""))
	e.ok(e.req("DELETE", "/v1/indexes/v", tok, ""))
	e.want(e.req("POST", "/v1/indexes/v/search", tok, `{"vector":[1,0]}`), 404, "no_index")
}

// Data that is not UTF-8 travels as base64, both ways.
func TestBinaryData(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	raw := []byte{0xff, 0x00, 0xfe}
	b64 := base64.StdEncoding.EncodeToString(raw)
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"blob","data_base64":"`+b64+`"}`))
	r := e.ok(e.req("GET", "/v1/items/1", tok, ""))
	if r.body["data_base64"] != b64 || r.body["text"] != nil {
		t.Fatalf("binary item: %v", r.body)
	}
}

func TestPaging(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"src"}`))
	for i := range 5 {
		e.ok(e.req("POST", "/v1/memories", tok, fmt.Sprintf(`{"text":"m%d","legs":[[1]]}`, i)))
	}
	// Memories are ids 2..6. Newest first, two at a time.
	var pages []string
	before := ""
	for range 4 {
		r := e.ok(e.req("GET", "/v1/memories?limit=2"+before, tok, ""))
		pages = append(pages, fmt.Sprint(r.body["ids"]))
		nb, more := r.body["next_before"]
		if !more {
			break
		}
		before = fmt.Sprintf("&before=%d", uint64(nb.(float64)))
	}
	if got := strings.Join(pages, " "); got != "[6 5] [4 3] [2]" {
		t.Fatalf("pages %s", got)
	}
}

// Beyond MaxRequests running at once, the server answers 503 busy at
// once instead of queueing (backpressure). /v1/health is not counted.
func TestBusy(t *testing.T) {
	e := newEnv(t, Options{MaxRequests: 1})
	tok := e.agent("alice")
	pr, pw := io.Pipe()
	r := httptest.NewRequest("POST", "http://"+host+"/v1/lots", pr)
	done := make(chan reply)
	go func() { done <- e.send(r, tok) }()
	waitFor(t, "the first request to start", func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		return e.srv.running == 1
	})

	busy := e.req("GET", "/v1/memories", tok, "")
	e.want(busy, 503, "busy")
	if busy.header.Get("Retry-After") == "" {
		t.Fatal("503 busy without Retry-After")
	}
	e.ok(e.req("GET", "/v1/health", "", ""))

	pw.Write([]byte(`{"source_type":"doc","text":"slow"}`))
	pw.Close()
	e.ok(<-done)
	e.ok(e.req("GET", "/v1/memories", tok, ""))
}

func TestAdmin(t *testing.T) {
	e := newEnv(t, Options{})
	e.want(e.req("POST", "/v1/admin/agents", "wrong", `{"name":"carol"}`), 401, "unauthorized")
	r := e.ok(e.req("POST", "/v1/admin/agents", e.admin, `{"name":"carol"}`))
	carol := r.body["token"].(string)
	e.ok(e.req("POST", "/v1/lots", carol, `{"source_type":"doc","text":"hi"}`))

	r = e.ok(e.req("GET", "/v1/admin/status", e.admin, ""))
	if r.num("open_agents") != 1 || r.num("requests") != 0 {
		t.Fatalf("status %v", r.body)
	}
	// Carol is kept open between requests; delete closes that first.
	e.ok(e.req("POST", "/v1/admin/agents/delete", e.admin, `{"token":"`+carol+`"}`))
	e.want(e.req("GET", "/v1/items/1", carol, ""), 401, "unauthorized")
	e.want(e.req("POST", "/v1/admin/agents/delete", e.admin, `{"token":"`+carol+`"}`), 404, "unknown_agent")
	e.want(e.req("POST", "/v1/admin/agents/delete", e.admin, `{"token":"not hex"}`), 400, "bad_request")
}

func TestAdminOff(t *testing.T) {
	s, err := cheesedb.Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	srv := New(s, Options{})
	defer srv.Close()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://x/v1/admin/agents", strings.NewReader(`{"name":"a"}`))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("ad", 32))
	srv.Handler(nil).ServeHTTP(w, r)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "no_admin") {
		t.Fatalf("admin route without a key: %d %s", w.Code, w.Body.String())
	}
}

// The request log never holds a token, memory text or agent name,
// but tells agents apart by 8 hex characters of the token's hash.
func TestRequestLog(t *testing.T) {
	e := newEnv(t, Options{})
	tok := e.agent("alice-the-agent")
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"very private words"}`))
	e.ok(e.req("GET", "/v1/items/1?secret=query", tok, ""))
	e.ok(e.req("POST", "/v1/admin/agents", e.admin, `{"name":"bob-the-agent"}`))
	log := e.log.String()
	for _, bad := range []string{tok, e.admin, "very private", "alice-the-agent", "bob-the-agent", "secret=query"} {
		if strings.Contains(log, bad) {
			t.Fatalf("request log contains %q:\n%s", bad, log)
		}
	}
	key := AdminKeyOf(tok)
	if !strings.Contains(log, " POST /v1/lots 200 ") || !strings.Contains(log, fmt.Sprintf("%x", key[:4])) {
		t.Fatalf("request log lacks the line or the key prefix:\n%s", log)
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A client that never reads its answer cannot hold a request slot (and
// its agent handle) for longer than writeWait.
func TestStalledReaderIsCutOff(t *testing.T) {
	old := writeWait
	writeWait = 200 * time.Millisecond
	t.Cleanup(func() { writeWait = old })
	e := newEnv(t, Options{})
	tok := e.agent("alice")
	big := strings.Repeat("x", 1<<20-1000) // just under the record limit
	e.ok(e.req("POST", "/v1/lots", tok, `{"source_type":"doc","text":"`+big+`"}`))

	ts := httptest.NewServer(e.h)
	defer ts.Close()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(1024) // fill up quickly; never read
	fmt.Fprintf(conn, "GET /v1/items/1 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", host, tok)
	waitFor(t, "the request to start", func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		return e.srv.running == 1
	})
	waitFor(t, "the stalled answer to be cut off", func() bool {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		return e.srv.running == 0
	})
}
