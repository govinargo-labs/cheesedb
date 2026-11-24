package main

// A minimal client for an OpenAI-compatible chat API (standard library
// only). The LLM lives here, in this command, never in the cheesedb library:
// cheesedb is a database and does not call models.
//
// Settings come from <home>/llm.conf (~/.cheesedb/llm.conf by default), one
// "name = value" per line, # for comments. Environment variables win over
// the file (the embedding settings for search are in embed.go):
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
	"net/url"
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
// The embed_ ones are for search (embed.go).
var llmSettings = map[string]string{
	"base_url":       "LLM_BASE_URL",
	"model":          "LLM_MODEL",
	"api_key":        "LLM_API_KEY",
	"thinking":       "LLM_THINKING",
	"embed_base_url": "EMBED_BASE_URL",
	"embed_model":    "EMBED_MODEL",
	"embed_api_key":  "EMBED_API_KEY",
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
// environment may hold everything. A file that holds a key must be private.
func readConf(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The mode comes from this open file, not from a second stat of the path:
	// what is checked is the file that was read.
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
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
			return nil, fmt.Errorf("%s line %d: want one of base_url, model, api_key, thinking, embed_base_url, embed_model, embed_api_key = value", path, i+1)
		}
		conf[name] = strings.TrimSpace(value)
	}
	if err := confModeError(path, info.Mode(), conf); err != nil {
		return nil, err
	}
	return conf, nil
}

// confKeyNames lists the key settings the file itself holds, in the order they
// are documented. A key in the environment is not the file's business.
func confKeyNames(conf map[string]string) []string {
	var names []string
	for _, name := range []string{"api_key", "embed_api_key"} {
		if conf[name] != "" {
			names = append(names, name)
		}
	}
	return names
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
		return "", fmt.Errorf("LLM request to %s: %w", safeURL(c.base), err)
	}
	defer resp.Body.Close()
	raw, err := readBody(resp.Body, 16<<20)
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

// readBody reads a server's answer, at most max bytes: a broken or hostile
// server could otherwise send without end until memory runs out.
func readBody(r io.Reader, max int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err == nil && int64(len(raw)) > max {
		err = fmt.Errorf("the server's answer is larger than %d MiB", max>>20)
	}
	return raw, err
}

// safeURL is base for a message, without any user name or password in it:
// errors go to stderr and, through remember's note, to the model.
func safeURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return "the configured server"
	}
	u.User = nil
	return u.String()
}
