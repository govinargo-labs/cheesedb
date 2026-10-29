package main

// A minimal client for an OpenAI-compatible chat API (standard library
// only). The LLM lives here, in this command, never in the cheesedb library:
// cheesedb is a database and does not call models.
//
// Settings come from <home>/llm.conf (~/.cheesedb/llm.conf by default), one
// "name = value" per line, # for comments. Environment variables win over
// the file:
//
//	base_url   LLM_BASE_URL   the server, e.g. http://localhost:8000/v1 (required)
//	model      LLM_MODEL      the model name (required)
//	api_key    LLM_API_KEY    sent as a Bearer token when set; never printed
//	thinking   LLM_THINKING   on or off, for models with a thinking switch
//	                          (sent as chat_template_kwargs.enable_thinking);
//	                          leave unset for servers that do not know it
//
// The file stays on each machine, outside the repository, so every user
// keeps their own server, model and key.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type llm struct {
	base, key, model string
	think            *bool // nil: do not send the thinking switch
	client           *http.Client
}

// llmSettings names each setting in llm.conf and its environment variable.
var llmSettings = map[string]string{
	"base_url": "LLM_BASE_URL",
	"model":    "LLM_MODEL",
	"api_key":  "LLM_API_KEY",
	"thinking": "LLM_THINKING",
}

func newLLM() (*llm, error) {
	path := filepath.Join(homeDir, "llm.conf")
	conf, err := readConf(path)
	if err != nil {
		return nil, err
	}
	get := func(name string) string {
		if v := os.Getenv(llmSettings[name]); v != "" {
			return v
		}
		return conf[name]
	}
	c := &llm{
		base:   get("base_url"),
		model:  get("model"),
		key:    get("api_key"),
		client: &http.Client{Timeout: 10 * time.Minute},
	}
	if c.base == "" || c.model == "" {
		return nil, fmt.Errorf("the LLM commands need a server and a model. Create %s with, for example:\n"+
			"  base_url = http://localhost:8000/v1\n  model    = your-model-name\n"+
			"(or set LLM_BASE_URL and LLM_MODEL)", path)
	}
	switch t := get("thinking"); t {
	case "":
	case "on", "off":
		on := t == "on"
		c.think = &on
	default:
		return nil, fmt.Errorf("thinking must be on or off, not %q", t)
	}
	return c, nil
}

// readConf reads "name = value" lines. A missing file is not an error: the
// environment may hold everything.
func readConf(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	conf := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if _, known := llmSettings[name]; !ok || !known {
			return nil, fmt.Errorf("%s line %d: want one of base_url, model, api_key, thinking = value", path, i+1)
		}
		conf[name] = strings.TrimSpace(value)
	}
	return conf, nil
}

var thinkTags = regexp.MustCompile(`(?s)<think>.*?</think>`)

// chat sends one system + user message and returns the reply text.
func (c *llm) chat(system, user string) (string, error) {
	msg := map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0,
		"max_tokens":  4096,
	}
	if c.think != nil {
		msg["chat_template_kwargs"] = map[string]bool{"enable_thinking": *c.think}
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequest("POST", strings.TrimSuffix(c.base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("LLM request to %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM returned %s: %.300s", resp.Status, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("LLM reply not understood: %.300s", raw)
	}
	return strings.TrimSpace(thinkTags.ReplaceAllString(out.Choices[0].Message.Content, "")), nil
}

// chatJSON asks for JSON and decodes it into v. Models sometimes wrap JSON
// in ``` fences or add a sentence around it, so this takes the outermost
// {...} from the reply.
func (c *llm) chatJSON(system, user string, v any) error {
	reply, err := c.chat(system, user)
	if err != nil {
		return err
	}
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return fmt.Errorf("LLM did not return JSON: %.300s", reply)
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), v); err != nil {
		return fmt.Errorf("LLM returned invalid JSON (%v): %.300s", err, reply)
	}
	return nil
}
