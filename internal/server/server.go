// Package server serves a cheesedb store over HTTP + JSON, so programs in
// any language (and curl) can use it, and so the store, its agents and their
// indexes stay loaded between calls.
//
// One request is one call to an Agent method. The token in the
// Authorization header chooses the agent; no URL and no body field names an
// agent, so the isolation rule of the library (no method takes an agent)
// holds over HTTP too.
//
// The server never calls a model, never fetches content and never runs
// tools: vectors arrive in the request.
//
// It imports only the public cheesedb package and the standard library, so
// it can move to its own module or repository without changes beyond its
// import path.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/govinargo-labs/cheesedb"
)

// Options tune a Server. The zero value is usable.
type Options struct {
	// AdminKey is the SHA-256 of the admin token, which creates and deletes
	// agents. Nil turns the admin routes off. The server keeps only the
	// hash, like the store does for agent tokens.
	AdminKey *[32]byte

	MaxRequests   int           // requests running at once before 503 busy; default 256
	IdleClose     time.Duration // close an agent unused this long; default 10 minutes
	MaxOpenAgents int           // agents kept open between requests; default 64

	Log     io.Writer // one line per request (never a token or memory text); nil: none
	Version string    // reported by /v1/health
}

// Server answers HTTP requests for one open store. It does not own the
// store: the caller opens it before New and closes it after Close.
type Server struct {
	store *cheesedb.Store
	opts  Options
	slots chan struct{} // one per running request: the cap on requests at once
	warm  *warmSet
	mux   *http.ServeMux

	mu      sync.Mutex
	running int // requests running now, for /v1/admin/status
}

// Limits on every request, so one client cannot starve the others.
const (
	maxBody   = 2 << 20 // a record body is at most 1 MiB; JSON escaping can double it
	maxK      = 1000    // results per search
	maxPage   = 10000   // ids per page of /v1/memories
	idleCheck = time.Minute
)

// writeWait bounds writing one answer. A client that stops reading would
// otherwise hold its request slot and agent handle forever. It starts when
// the answer is ready, so long work (a Compact, a first search that loads
// an index) is not cut short. A variable so a test can shorten it.
var writeWait = 30 * time.Second

// New makes a server for s. Call Close when done, before closing s.
func New(s *cheesedb.Store, o Options) *Server {
	if o.MaxRequests <= 0 {
		o.MaxRequests = 256
	}
	if o.IdleClose <= 0 {
		o.IdleClose = 10 * time.Minute
	}
	if o.MaxOpenAgents <= 0 {
		o.MaxOpenAgents = 64
	}
	srv := &Server{
		store: s,
		opts:  o,
		slots: make(chan struct{}, o.MaxRequests),
		warm:  newWarmSet(s, o.IdleClose, o.MaxOpenAgents),
		mux:   http.NewServeMux(),
	}
	srv.routes()
	srv.warm.start(max(min(idleCheck, o.IdleClose/2), time.Millisecond))
	return srv
}

// Close closes the agents kept open between requests. Call it after the
// HTTP server has stopped (http.Server.Shutdown), so no request is running.
func (s *Server) Close() error { return s.warm.close() }

// Handler returns the server's routes for one listener. hosts lists the
// Host header values allowed, such as "127.0.0.1:7700" and
// "localhost:7700"; a request with any other Host is refused, which blocks
// DNS rebinding (a web page on a hostile domain that resolves to
// 127.0.0.1). Empty hosts turns the check off: for a Unix socket, which a
// browser cannot reach.
func (s *Server) Handler(hosts []string) http.Handler {
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		s.serve(rw, r, allowed)
		s.logRequest(r, rw.status, time.Since(start))
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, allowed map[string]bool) {
	// Browser defences. A web page can send requests to localhost;
	// a request from one always carries Origin (for anything but a plain
	// GET or form post) and a Host naming the page's domain. No browser
	// access is offered, so any Origin is refused.
	if r.Header.Get("Origin") != "" {
		fail(w, http.StatusForbidden, "origin_refused", "requests from web pages are not accepted")
		return
	}
	if len(allowed) > 0 && !allowed[strings.ToLower(r.Host)] {
		fail(w, http.StatusForbidden, "bad_host", "unexpected Host header")
		return
	}
	if r.URL.Path != "/v1/health" {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			fail(w, http.StatusServiceUnavailable, "busy", "too many requests at once; retry shortly")
			return
		}
		s.mu.Lock()
		s.running++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.running--
			s.mu.Unlock()
		}()
	}
	s.mux.ServeHTTP(w, r)
}

// statusWriter remembers the status written, for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the connection (for the write
// deadline in writeJSON).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequest writes one line per request: time, method, path, status,
// duration and the first 8 hex characters of the token's hash. That is
// enough to tell agents apart in the log and useless for logging in.
// Never the token, the query string, a body or an agent name.
func (s *Server) logRequest(r *http.Request, status int, d time.Duration) {
	if s.opts.Log == nil {
		return
	}
	who := "-"
	if t, ok := bearer(r); ok {
		sum := sha256.Sum256([]byte(t))
		who = hex.EncodeToString(sum[:4])
	}
	// The escaped path: the decoded one could hold a line break (%0A) and
	// forge a second log line. It also carries index names, which callers
	// choose.
	fmt.Fprintf(s.opts.Log, "%s %s %s %d %s %s\n",
		time.Now().UTC().Format(time.RFC3339), r.Method, r.URL.EscapedPath(), status,
		d.Round(time.Microsecond), who)
}

// bearer returns the token of an "Authorization: Bearer <token>" header.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return strings.TrimSpace(h[len(p):]), true
}

// errUnauthorized covers a missing, malformed or unknown token alike, so a
// caller learns nothing about which agents exist.
var errUnauthorized = errors.New("missing or wrong token")

// agentFunc is a route that works on the caller's own agent.
type agentFunc func(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent)

// withAgent opens the agent named by the request's token for the length of
// the request. Each request takes its own handle (cheap: an open agent
// shares its ledger), and the warm set keeps one more so the agent stays
// loaded between requests.
func (s *Server) withAgent(f agentFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearer(r)
		if !ok {
			s.writeErr(w, errUnauthorized)
			return
		}
		tok, err := cheesedb.ParseToken(raw)
		if err != nil {
			s.writeErr(w, errUnauthorized)
			return
		}
		a, err := s.store.OpenAgent(tok)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		defer a.Close()
		s.warm.keep(tok)
		f(w, r, a)
	}
}

// withAdmin lets the request through only with the admin token.
func (s *Server) withAdmin(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.AdminKey == nil {
			fail(w, http.StatusNotFound, "no_admin", "admin routes are off: start the server with an admin key")
			return
		}
		raw, ok := bearer(r)
		sum := sha256.Sum256([]byte(raw))
		// Constant time, so the time taken says nothing about how much of
		// a guess was right.
		if !ok || subtle.ConstantTimeCompare(sum[:], s.opts.AdminKey[:]) != 1 {
			s.writeErr(w, errUnauthorized)
			return
		}
		f(w, r)
	}
}

// AdminKeyOf is what an admin key file holds for token t: its SHA-256.
func AdminKeyOf(t string) [32]byte { return sha256.Sum256([]byte(t)) }

// errStatus maps an engine error to an HTTP status and a stable code.
// Unknown errors are 500 and their text is not sent: it may name files.
func errStatus(err error) (int, string) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig), errors.Is(err, cheesedb.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, errBadRequest), errors.Is(err, cheesedb.ErrBadArgument):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, cheesedb.ErrBadLegs):
		return http.StatusBadRequest, "bad_legs"
	case errors.Is(err, cheesedb.ErrBadVector):
		return http.StatusBadRequest, "bad_vector"
	case errors.Is(err, cheesedb.ErrBadName):
		return http.StatusBadRequest, "bad_name"
	case errors.Is(err, errUnauthorized), errors.Is(err, cheesedb.ErrBadToken):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, cheesedb.ErrUnknownID):
		return http.StatusNotFound, "unknown_id"
	case errors.Is(err, cheesedb.ErrNoIndex):
		return http.StatusNotFound, "no_index"
	case errors.Is(err, cheesedb.ErrDeadParent):
		return http.StatusConflict, "dead_parent"
	case errors.Is(err, cheesedb.ErrRecalled):
		return http.StatusConflict, "recalled"
	case errors.Is(err, cheesedb.ErrIndexExists):
		return http.StatusConflict, "index_exists"
	case errors.Is(err, cheesedb.ErrAgentExists):
		return http.StatusConflict, "agent_exists"
	case errors.Is(err, cheesedb.ErrAgentInUse):
		return http.StatusConflict, "agent_in_use"
	case errors.Is(err, cheesedb.ErrBroken):
		return http.StatusServiceUnavailable, "broken"
	case errors.Is(err, cheesedb.ErrClosed):
		return http.StatusServiceUnavailable, "shutting_down"
	case errors.Is(err, cheesedb.ErrCorrupt):
		return http.StatusInternalServerError, "corrupt"
	}
	return http.StatusInternalServerError, "internal"
}

// writeErr answers with err's status and code. The message is the
// engine's for a mistake in the request (it names ids and fields, never
// content), and a fixed one for a failure on the server's side: those carry
// file paths, which name the store folder and, in hex, the agent.
func (s *Server) writeErr(w http.ResponseWriter, err error) {
	status, code := errStatus(err)
	msg := err.Error()
	switch {
	case status == http.StatusUnauthorized:
		msg = errUnauthorized.Error() // the same answer for every token problem
	case code == "broken":
		msg = "a write to this agent's log failed; it refuses writes until the server reopens it"
	case status >= 500:
		msg = "internal error; see the server's log"
	}
	if status >= 500 && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "error: %s: %s\n", code, withoutPaths(err.Error()))
	}
	fail(w, status, code, msg)
}

// A file path in an error message: the part from the first slash to the
// next colon, space or quote.
var pathRE = regexp.MustCompile(`(?:[A-Za-z]:)?[/\\][^\s:"]*`)

// withoutPaths replaces file paths in an error message with <path>, so the
// log keeps what went wrong but not which agent's folder it was.
func withoutPaths(msg string) string { return pathRE.ReplaceAllString(msg, "<path>") }

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}
