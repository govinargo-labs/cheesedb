package main

// A minimal client for an OpenAI-compatible embeddings API (POST
// /embeddings), for search. Like the chat client in llm.go, it lives in
// this command, never in the cheesedb library.
//
// Settings, in <home>/llm.conf or the environment (which wins):
//
//	embed_model     EMBED_MODEL      the embedding model, e.g. nomic-embed-text;
//	                                 search is off without it
//	embed_base_url  EMBED_BASE_URL   the server; default: base_url
//	embed_api_key   EMBED_API_KEY    sent as a Bearer token; default: api_key,
//	                                 but only when embed_base_url is not set,
//	                                 so a key is never sent to a server it
//	                                 was not given for
//
// Every text sent here leaves this program: a local server (Ollama, LM
// Studio) keeps memories on the machine, a hosted one does not.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type embedder struct {
	base, key, model string
	client           *http.Client
}

// newEmbedder reads the settings. It returns nil, and no error, when no
// embedding model is set: search is simply off.
func newEmbedder() (*embedder, error) {
	conf, err := readConf(filepath.Join(homeDir, "llm.conf"))
	if err != nil {
		return nil, err
	}
	get := func(name string) string {
		if v := os.Getenv(llmSettings[name]); v != "" {
			return v
		}
		return conf[name]
	}
	e := &embedder{model: get("embed_model"), client: &http.Client{Timeout: 2 * time.Minute}}
	if e.model == "" {
		return nil, nil
	}
	if e.base = get("embed_base_url"); e.base != "" {
		e.key = get("embed_api_key")
	} else {
		e.base, e.key = get("base_url"), get("api_key")
		if k := get("embed_api_key"); k != "" {
			e.key = k
		}
	}
	if e.base == "" {
		return nil, fmt.Errorf("embed_model is set, but no server: set embed_base_url (or base_url) in %s, or EMBED_BASE_URL",
			filepath.Join(homeDir, "llm.conf"))
	}
	return e, nil
}

func (e *embedder) Model() string { return e.model }

// Embed returns one vector per text, in the order of texts.
func (e *embedder) Embed(texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": e.model, "input": texts})
	req, err := http.NewRequest("POST", strings.TrimSuffix(e.base, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request to %s: %w", safeURL(e.base), err)
	}
	defer resp.Body.Close()
	raw, err := readBody(resp.Body, 64<<20) // a batch of 64 texts at 8192 values is about 10 MB
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding server returned %s: %.300s", resp.Status, raw)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedding reply not understood: %.300s", raw)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding server returned %d vectors for %d texts", len(out.Data), len(texts))
	}
	// Servers may answer out of order; index says which text each is for.
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) || vecs[d.Index] != nil {
			return nil, fmt.Errorf("embedding server returned a bad index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
