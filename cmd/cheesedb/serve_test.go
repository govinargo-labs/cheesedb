package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/govinargo-labs/cheesedb"
	"github.com/govinargo-labs/cheesedb/internal/server"
)

func TestCheckListen(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:7700": true,
		"127.0.0.2:7700": true, // all of 127/8 is this machine
		"[::1]:7700":     true,
		"localhost:7700": true,
		"0.0.0.0:7700":   false, // every interface
		":7700":          false, // the same
		"192.168.1.5:80": false,
		"example.com:80": false,
		"nonsense":       false,
	} {
		if err := checkListen(addr, false); (err == nil) != ok {
			t.Errorf("checkListen(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
	if err := checkListen("0.0.0.0:7700", true); err != nil {
		t.Errorf("with TLS: %v", err)
	}
}

// A server started on a free port, with Alice created beforehand by the
// command. stop shuts it down and returns runServer's error.
type running struct {
	url   string
	token string
	stop  func() error
}

func startServer(t *testing.T, c serveConfig) running {
	t.Helper()
	useHome(t, "")
	if err := create(dataDir, "alice"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tokenPath("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if c.dir == "" {
		c.dir = dataDir
	}
	if c.listen == "" && c.unix == "" {
		c.listen = "127.0.0.1:0"
	}
	ctx, cancel := context.WithCancel(context.Background())
	where := make(chan []string, 1)
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, c, io.Discard, func(w []string) { where <- w }) }()
	var url string
	select {
	case w := <-where:
		url = w[0]
	case err := <-done:
		t.Fatalf("runServer: %v", err)
	}
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		return <-done
	}
	t.Cleanup(func() { stop() })
	return running{url: url, token: strings.TrimSpace(string(b)), stop: stop}
}

func call(t *testing.T, c *http.Client, method, url, tok, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

// Over a real socket: write, read back, stop; the lock is released and the
// data is in the store for the embedded command to read.
func TestServeOverTCP(t *testing.T) {
	r := startServer(t, serveConfig{})
	if !strings.HasPrefix(r.url, "http://127.0.0.1:") {
		t.Fatalf("serving on %s", r.url)
	}
	st, out := call(t, http.DefaultClient, "POST", r.url+"/v1/lots", r.token, `{"source_type":"note","text":"over the wire"}`)
	if st != 200 {
		t.Fatalf("add lot: %d %v", st, out)
	}
	st, out = call(t, http.DefaultClient, "GET", r.url+"/v1/items/1", r.token, "")
	if st != 200 || out["text"] != "over the wire" {
		t.Fatalf("get: %d %v", st, out)
	}

	// While serving, the store is locked, and the command says why.
	if _, _, err := openAgent(dataDir, "alice"); !errors.Is(err, cheesedb.ErrLocked) || !strings.Contains(err.Error(), "cheesedb serve") {
		t.Fatalf("command while serving: %v", err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	s, a, err := openAgent(dataDir, "alice")
	if err != nil {
		t.Fatalf("after stop the store is still locked: %v", err)
	}
	defer s.Close()
	defer a.Close()
	if it, err := a.Get(1); err != nil || string(it.Data) != "over the wire" {
		t.Fatalf("embedded read after the server: %v %q", err, it.Data)
	}
}

// Shutdown lets a running request finish before the store closes (D12).
func TestShutdownFinishesRequests(t *testing.T) {
	r := startServer(t, serveConfig{})
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", r.url+"/v1/lots", pr)
	req.Header.Set("Authorization", "Bearer "+r.token)
	got := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- -1
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	pw.Write([]byte(`{"source_type":"note",`)) // the request is now running
	time.Sleep(50 * time.Millisecond)

	stopped := make(chan error, 1)
	go func() { stopped <- r.stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("stopped (%v) while a request was running", err)
	case <-time.After(100 * time.Millisecond):
	}
	pw.Write([]byte(`"text":"finished during shutdown"}`))
	pw.Close()
	if st := <-got; st != 200 {
		t.Fatalf("request during shutdown: %d", st)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	s, a, err := openAgent(dataDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer a.Close()
	if it, err := a.Get(1); err != nil || string(it.Data) != "finished during shutdown" {
		t.Fatalf("after shutdown: %v %q", err, it.Data)
	}
}

func TestServeRefuses(t *testing.T) {
	useHome(t, "")
	ctx := context.Background()
	for _, c := range []serveConfig{
		{dir: dataDir, listen: "0.0.0.0:0"},                        // network without TLS
		{dir: dataDir, listen: "127.0.0.1:0", tlsCert: "cert.pem"}, // half of TLS
		{dir: dataDir}, // nothing to listen on
		{dir: dataDir, listen: "127.0.0.1:0", adminKey: "missing"}, // no such key file
	} {
		if err := runServer(ctx, c, io.Discard, nil); err == nil {
			t.Errorf("%+v: started", c)
		}
	}
	// A store already open elsewhere.
	if err := makeHome(); err != nil {
		t.Fatal(err)
	}
	s, err := cheesedb.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = runServer(ctx, serveConfig{dir: dataDir, listen: "127.0.0.1:0"}, io.Discard, nil)
	if !errors.Is(err, cheesedb.ErrLocked) {
		t.Fatalf("on a locked store: %v", err)
	}
}

func TestServeUnix(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	r := startServer(t, serveConfig{listen: "", unix: sock})
	if r.url != "unix:"+sock {
		t.Fatalf("serving on %s", r.url)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", fi.Mode().Perm())
	}
	// The temp folder the socket was made in is gone.
	if ents, _ := os.ReadDir(filepath.Dir(sock)); len(ents) != 1 {
		t.Fatalf("socket folder holds %d entries, want only the socket", len(ents))
	}
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	st, out := call(t, c, "POST", "http://local/v1/lots", r.token, `{"source_type":"note","text":"by socket"}`)
	if st != 200 {
		t.Fatalf("over the socket: %d %v", st, out)
	}
	// A second server cannot take a socket that answers.
	if _, err := listenUnix(sock); err == nil {
		t.Fatal("listened on a live socket")
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

func TestInitAdminKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-folder", "admin.key") // --init makes the folder
	var out bytes.Buffer
	if err := initAdminKey(path, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	tok := lines[len(lines)-1]
	key, err := readAdminKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if *key != server.AdminKeyOf(tok) {
		t.Fatal("the file does not hold the printed token's hash")
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), tok) {
		t.Fatal("the key file holds the token itself")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	if err := initAdminKey(path, io.Discard); err == nil {
		t.Fatal("--init overwrote an existing key")
	}
	if _, err := hex.DecodeString(tok); err != nil || len(tok) != 64 {
		t.Fatalf("token %q", tok)
	}
}

// --tls-auto makes a self-signed certificate on first start, serves HTTPS
// with it on an address other machines could reach (as in a container),
// and reuses it after a restart, so clients that trust it keep working.
func TestServeTLSAuto(t *testing.T) {
	certDir := filepath.Join(t.TempDir(), "tls")
	r := startServer(t, serveConfig{listen: "0.0.0.0:0", tlsAuto: certDir, tlsHosts: "cheesedb.example, 10.0.0.7"})
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(r.url, "https://"))
	pem1, err := os.ReadFile(filepath.Join(certDir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(certDir, "tls.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem1)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	st, out := call(t, c, "POST", "https://127.0.0.1:"+port+"/v1/lots", r.token, `{"source_type":"note","text":"over TLS"}`)
	if st != 200 {
		t.Fatalf("over TLS: %d %v", st, out)
	}
	// Plain HTTP to the TLS port is no way in.
	if resp, err := http.Get("http://127.0.0.1:" + port + "/v1/health"); err == nil {
		if resp.StatusCode == 200 {
			t.Fatal("plain HTTP answered on the TLS port")
		}
		resp.Body.Close()
	}
	leaf, _ := pem.Decode(pem1)
	cert, err := x509.ParseCertificate(leaf.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("cheesedb.example"); err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("10.0.0.7"); err != nil {
		t.Fatal(err)
	}
	if err := r.stop(); err != nil {
		t.Fatal(err)
	}

	// Started again: the same certificate.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runServer(ctx, serveConfig{dir: dataDir, listen: "127.0.0.1:0", tlsAuto: certDir}, io.Discard, func([]string) { cancel() })
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if pem2, _ := os.ReadFile(filepath.Join(certDir, "tls.crt")); !bytes.Equal(pem1, pem2) {
		t.Fatal("a restart made a new certificate")
	}
}

func TestServeTLSRefuses(t *testing.T) {
	useHome(t, "")
	bad := filepath.Join(t.TempDir(), "not-a-cert")
	os.WriteFile(bad, []byte("nonsense"), 0o600)
	for _, c := range []serveConfig{
		{dir: dataDir, listen: "127.0.0.1:0", tlsCert: bad, tlsKey: bad},          // broken files fail before serving
		{dir: dataDir, listen: "127.0.0.1:0", tlsAuto: t.TempDir(), tlsCert: bad}, // both ways at once
		{dir: dataDir, listen: "127.0.0.1:0", tlsHosts: "x"},                      // --tls-host alone
	} {
		served := false
		if err := runServer(context.Background(), c, io.Discard, func([]string) { served = true }); err == nil || served {
			t.Errorf("%+v: started (err %v)", c, err)
		}
	}
}
