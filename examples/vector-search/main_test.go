package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRunToy runs the whole demo with the toy embedder.
func TestRunToy(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("EMBED_BASE_URL", "")
	if err := run(false); err != nil {
		t.Fatal(err)
	}
}

// TestRunServer runs the demo against a fake OpenAI-compatible embeddings
// server, which answers with the toy vectors in reverse order (the API
// allows any order; "index" says which text each vector belongs to).
func TestRunServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "m" {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := len(req.Input) - 1; i >= 0; i-- {
			data = append(data, item{i, toyEmbed(req.Input[i])})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("EMBED_BASE_URL", srv.URL+"/v1")
	t.Setenv("EMBED_MODEL", "m")
	t.Setenv("EMBED_API_KEY", "k")
	if err := run(false); err != nil {
		t.Fatal(err)
	}
}
