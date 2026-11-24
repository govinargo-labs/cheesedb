package server

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/govinargo-labs/cheesedb"
)

// errBadRequest is a request the server itself refuses (bad JSON, a
// missing field), before it reaches the engine.
var errBadRequest = errors.New("bad request")

func badRequest(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, a...))
}

// routes lists every route. {name} is always an index name and {id} a
// record id; no route takes an agent. A known path with the wrong method
// gets the same 404 as an unknown path.
func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /v1/health", s.health)

	m.HandleFunc("POST /v1/lots", s.withAgent(s.addLot))
	m.HandleFunc("POST /v1/memories", s.withAgent(s.remember))
	m.HandleFunc("GET /v1/memories", s.withAgent(s.active))
	m.HandleFunc("GET /v1/items/{id}", s.withAgent(s.get))
	m.HandleFunc("POST /v1/recall", s.withAgent(s.recall))
	m.HandleFunc("POST /v1/recall/preview", s.withAgent(s.wouldRecall))
	m.HandleFunc("POST /v1/compact", s.withAgent(s.compact))
	m.HandleFunc("POST /v1/verify", s.withAgent(s.verify))
	m.HandleFunc("GET /v1/report", s.withAgent(s.report))

	m.HandleFunc("GET /v1/indexes", s.withAgent(s.indexes))
	m.HandleFunc("PUT /v1/indexes/{name}", s.withAgent(s.createIndex))
	m.HandleFunc("DELETE /v1/indexes/{name}", s.withAgent(s.dropIndex))
	m.HandleFunc("POST /v1/indexes/{name}/vectors", s.withAgent(s.embed))
	m.HandleFunc("POST /v1/indexes/{name}/search", s.withAgent(s.search))
	m.HandleFunc("POST /v1/indexes/{name}/verify", s.withAgent(s.verifyIndex))

	m.HandleFunc("POST /v1/admin/agents", s.withAdmin(s.createAgent))
	m.HandleFunc("POST /v1/admin/agents/delete", s.withAdmin(s.deleteAgent))
	m.HandleFunc("GET /v1/admin/status", s.withAdmin(s.status))

	// Anything else, including a known path with the wrong method, gets the
	// same JSON error shape as every other refusal.
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "not_found", "no such route")
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.opts.Version})
}

func (s *Server) addLot(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	var in struct {
		SourceType string `json:"source_type"`
		content
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	data, err := in.bytes()
	if err == nil {
		var id cheesedb.ID
		id, err = a.AddLot(in.SourceType, data)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"id": id})
			return
		}
	}
	s.writeErr(w, err)
}

func (s *Server) remember(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	var in struct {
		content
		Legs []cheesedb.Leg `json:"legs"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	data, err := in.bytes()
	if err == nil {
		var id cheesedb.ID
		id, err = a.Remember(data, in.Legs...)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"id": id})
			return
		}
	}
	s.writeErr(w, err)
}

// active pages through the live memories, newest first: ?limit=n (default
// 1000) and ?before=id (only ids below it), with next_before set when more
// remain.
func (s *Server) active(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	limit, err := queryInt(r, "limit", 1000, 1, maxPage)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	before, err := queryInt(r, "before", 0, 1, -1)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	ids, err := a.Active() // ascending
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if before > 0 {
		n, _ := slices.BinarySearch(ids, cheesedb.ID(before))
		ids = ids[:n]
	}
	out := map[string]any{}
	if len(ids) > limit {
		ids = ids[len(ids)-limit:]
		out["next_before"] = ids[0]
	}
	slices.Reverse(ids)
	out["ids"] = nonNil(ids)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	id, err := pathID(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	it, err := a.Get(id)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := map[string]any{"id": it.ID, "kind": kindName(it.Kind), "alive": it.Alive, "redacted": it.Redacted}
	if it.Data != nil {
		if utf8.Valid(it.Data) {
			out["text"] = string(it.Data)
		} else {
			out["data_base64"] = base64.StdEncoding.EncodeToString(it.Data)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func kindName(k cheesedb.Kind) string {
	switch k {
	case cheesedb.KindLot:
		return "lot"
	case cheesedb.KindMemory:
		return "memory"
	}
	return "other"
}

type recallIn struct {
	ID     cheesedb.ID `json:"id"`
	Mode   string      `json:"mode"`
	Reason string      `json:"reason"`
}

// mode reads "fair" or "strict". There is no default: a recall cannot be
// undone, so the caller says which one it means.
func (in recallIn) mode() (cheesedb.Mode, error) {
	switch in.Mode {
	case "fair":
		return cheesedb.Fair, nil
	case "strict":
		return cheesedb.Strict, nil
	}
	return 0, badRequest(`mode must be "fair" or "strict"`)
}

func (s *Server) recall(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	var in recallIn
	if !s.readJSON(w, r, &in) {
		return
	}
	mode, err := in.mode()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	var reason cheesedb.Reason
	switch in.Reason {
	case "untrusted":
		reason = cheesedb.Untrusted
	case "erase":
		reason = cheesedb.Erase
	default:
		s.writeErr(w, badRequest(`reason must be "untrusted" or "erase"`))
		return
	}
	removed, err := a.Recall(in.ID, mode, reason)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": nonNil(removed)})
}

func (s *Server) wouldRecall(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	var in recallIn
	if !s.readJSON(w, r, &in) {
		return
	}
	if in.Reason != "" {
		s.writeErr(w, badRequest("a preview takes no reason"))
		return
	}
	mode, err := in.mode()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	removed, err := a.WouldRecall(in.ID, mode)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": nonNil(removed)})
}

func (s *Server) compact(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	if !s.readJSON(w, r, nil) {
		return
	}
	n, err := a.Compact()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"erased": n})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	if !s.readJSON(w, r, nil) {
		return
	}
	head, err := a.Verify()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"head": hex.EncodeToString(head[:])})
}

func (s *Server) report(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	writeJSON(w, http.StatusOK, reportJSON(a.Report()))
}

func reportJSON(rep cheesedb.Report) map[string]any {
	out := map[string]any{
		"records":          rep.Records,
		"dropped_bytes":    rep.DroppedBytes,
		"resumed_rollover": rep.ResumedRollover,
		"finished_erase":   rep.FinishedErase,
		"unfinished_erase": rep.UnfinishedErase,
	}
	if rep.EraseErr != nil {
		out["erase_error"] = withoutPaths(rep.EraseErr.Error())
	}
	return out
}

func (s *Server) indexes(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	infos, err := a.Indexes()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := []map[string]any{}
	for _, in := range infos {
		m := map[string]any{"name": in.Name, "dims": in.Dims, "label": in.Label, "vectors": in.Vectors}
		if in.Err != nil {
			m["error"] = withoutPaths(in.Err.Error())
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexes": out})
}

func (s *Server) createIndex(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	var in struct {
		Dims  int    `json:"dims"`
		Label string `json:"label"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	if err := a.CreateIndex(r.PathValue("name"), in.Dims, in.Label); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) dropIndex(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	if err := a.DropIndex(r.PathValue("name")); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) embed(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	var in struct {
		Items []struct {
			ID     cheesedb.ID `json:"id"`
			Vector []float32   `json:"vector"`
		} `json:"items"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	items := make([]cheesedb.Embedding, len(in.Items))
	for i, it := range in.Items {
		items[i] = cheesedb.Embedding{ID: it.ID, Vector: it.Vector}
	}
	if err := a.EmbedBatch(r.PathValue("name"), items); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": len(items)})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	var in struct {
		Vector []float32 `json:"vector"`
		K      *int      `json:"k"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	k := 10
	if in.K != nil {
		k = *in.K
	}
	if k < 1 || k > maxK {
		s.writeErr(w, badRequest("k must be 1 to %d", maxK))
		return
	}
	hits, err := a.Search(r.PathValue("name"), in.Vector, k)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	out := []map[string]any{}
	for _, h := range hits {
		out = append(out, map[string]any{"id": h.ID, "score": h.Score})
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": out})
}

func (s *Server) verifyIndex(w http.ResponseWriter, r *http.Request, a *cheesedb.Agent) {
	if !s.readJSON(w, r, nil) {
		return
	}
	head, err := a.VerifyIndex(r.PathValue("name"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"head": hex.EncodeToString(head[:])})
}

// The admin routes, which need the admin token: creating and deleting
// agents, and a status page.

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	tok, err := s.store.CreateAgent(in.Name)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	// The only time the token is ever sent: the store keeps its hash only.
	writeJSON(w, http.StatusOK, map[string]any{"token": tok.String()})
}

// deleteAgent takes the agent's own token in the body, never in the URL,
// where it would end up in logs.
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !s.readJSON(w, r, &in) {
		return
	}
	// The admin token was right, so a problem with this one is the
	// request's, not a reason for 401.
	tok, err := cheesedb.ParseToken(in.Token)
	if err != nil {
		s.writeErr(w, badRequest("token must be 64 hex characters"))
		return
	}
	// The warm handle would keep the agent open, and DeleteAgent refuses
	// an open agent. A request running now still gets agent_in_use.
	if err := s.warm.drop(tok); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := s.store.DeleteAgent(tok); errors.Is(err, cheesedb.ErrBadToken) {
		fail(w, http.StatusNotFound, "unknown_agent", "no agent has this token")
		return
	} else if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"open_agents": s.warm.len(),
		"requests":    running - 1, // not counting this one
	})
}

// content is the text of a lot or memory in a request: "text" for UTF-8,
// "data_base64" for anything else. Exactly one of them.
type content struct {
	Text       *string `json:"text"`
	DataBase64 *string `json:"data_base64"`
}

func (c content) bytes() ([]byte, error) {
	switch {
	case c.Text != nil && c.DataBase64 == nil:
		return []byte(*c.Text), nil
	case c.DataBase64 != nil && c.Text == nil:
		b, err := base64.StdEncoding.DecodeString(*c.DataBase64)
		if err != nil {
			return nil, badRequest("data_base64 is not valid base64")
		}
		return b, nil
	}
	return nil, badRequest(`give exactly one of "text" and "data_base64"`)
}

// readJSON decodes the body into v, refusing unknown fields, anything
// after the value and bodies over maxBody. Field names match regardless of
// case (encoding/json does that); harmless, since no field names an agent.
// With v nil the body must be empty or {}. On failure it has answered and
// returns false.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.writeErr(w, err)
		return false
	}
	if err != nil { // the client went away or was too slow
		s.writeErr(w, badRequest("reading the body: %v", err))
		return false
	}
	if v == nil {
		if len(b) == 0 {
			return true
		}
		v = &struct{}{}
	}
	return s.decodeAll(w, b, v)
}

func (s *Server) decodeAll(w http.ResponseWriter, b []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.writeErr(w, badRequest("body: %v", err))
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		s.writeErr(w, badRequest("body: something after the JSON value"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// Errors only where the writer has no connection (tests): then there
	// is nothing to bound.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeWait))
	w.Header().Set("Content-Type", "application/json")
	// Never cached, never sniffed as HTML by a browser.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func pathID(r *http.Request) (cheesedb.ID, error) {
	n, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil || n == 0 {
		return 0, badRequest("id must be a positive whole number")
	}
	return cheesedb.ID(n), nil
}

// queryInt reads a whole-number query parameter, def when absent, refused
// outside lo..hi (hi < 0: no upper bound).
func queryInt(r *http.Request, name string, def, lo, hi int) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || (hi >= 0 && n > hi) {
		return 0, badRequest("%s is out of range", name)
	}
	return n, nil
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil(ids []cheesedb.ID) []cheesedb.ID {
	if ids == nil {
		return []cheesedb.ID{}
	}
	return ids
}
