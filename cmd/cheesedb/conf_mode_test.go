//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const confWithKey = "base_url = http://localhost:8000/v1\nmodel = chat\napi_key = sk-not-in-the-message\n"

// readableConf writes llm.conf with body and sets its mode, then reads it
// through readConf. The mode is set with chmod because the umask decides what
// WriteFile's mode argument actually leaves behind.
func readableConf(t *testing.T, body string, mode os.FileMode) (map[string]string, string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llm.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	conf, err := readConf(path)
	return conf, path, err
}

func TestConfWithAKeyMustBePrivate(t *testing.T) {
	if _, _, err := readableConf(t, confWithKey, 0o600); err != nil {
		t.Fatalf("0600 with a key: %v", err)
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		_, path, err := readableConf(t, confWithKey, mode)
		if err == nil {
			t.Fatalf("mode %04o with a key was read; want a refusal", mode)
		}
		for _, want := range []string{path, "chmod 600"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("mode %04o: error does not mention %q: %v", mode, want, err)
			}
		}
		if strings.Contains(err.Error(), "sk-not-in-the-message") {
			t.Errorf("mode %04o: the error prints the key: %v", mode, err)
		}
	}
}

func TestConfWithoutAKeyMayBeReadable(t *testing.T) {
	body := "base_url = http://localhost:8000/v1\nmodel = chat\n"
	if _, _, err := readableConf(t, body, 0o644); err != nil {
		t.Fatalf("0644 without a key: %v", err)
	}
}

// An embedding key is a key: the same file, the same refusal, and the error
// names the setting that holds it.
func TestConfWithAnEmbeddingKeyMustBePrivate(t *testing.T) {
	body := "embed_base_url = http://localhost:11434/v1\nembed_model = nomic\nembed_api_key = sk-not-in-the-message\n"
	_, _, err := readableConf(t, body, 0o644)
	if err == nil {
		t.Fatal("0644 with an embed_api_key was read; want a refusal")
	}
	if !strings.Contains(err.Error(), "holds embed_api_key") || strings.Contains(err.Error(), "holds api_key") {
		t.Errorf("the error should name embed_api_key and not plain api_key: %v", err)
	}
	// The key itself may sit in a file others can read when no key is used.
	if _, _, err := readableConf(t, body, 0o600); err != nil {
		t.Fatalf("0600 with an embed_api_key: %v", err)
	}
}

// The check reaches the commands: a readable llm.conf with a key fails newLLM
// before any request is made.
func TestNewLLMRefusesAReadableKey(t *testing.T) {
	old := homeDir
	homeDir = t.TempDir()
	t.Cleanup(func() { homeDir = old })

	path := filepath.Join(homeDir, "llm.conf")
	if err := os.WriteFile(path, []byte(confWithKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := newLLM()
	if err == nil {
		t.Fatal("newLLM accepted a readable llm.conf that holds a key")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("newLLM's error does not say how to fix it: %v", err)
	}
}

// A key in the environment needs no private file: the file may stay readable.
func TestAnEnvironmentKeyWorksWithAReadableConf(t *testing.T) {
	old := homeDir
	homeDir = t.TempDir()
	t.Cleanup(func() { homeDir = old })

	path := filepath.Join(homeDir, "llm.conf")
	if err := os.WriteFile(path, []byte("base_url = http://localhost:8000/v1\nmodel = chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_API_KEY", "sk-from-the-environment")
	c, err := newLLM()
	if err != nil {
		t.Fatalf("a key from the environment was refused: %v", err)
	}
	if c.key != "sk-from-the-environment" {
		t.Errorf("key = %q, want the environment's", c.key)
	}
}

// The chmod line is meant to be copied: a path the shell would split comes
// quoted.
func TestTheRepairLineQuotesASpacedPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "llm.conf")
	if err := os.WriteFile(path, []byte(confWithKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readConf(path)
	if err == nil {
		t.Fatal("0644 with a key in a folder with a space was read")
	}
	if want := "chmod 600 '" + path + "'"; !strings.Contains(err.Error(), want) {
		t.Errorf("the chmod line is not quoted for the shell; want %q in: %v", want, err)
	}
}
