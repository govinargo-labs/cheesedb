package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/govinargo-labs/cheesedb"
	"github.com/govinargo-labs/cheesedb/internal/server"
)

const serveUsage = `use: cheesedb serve [flags]

Serves the store over HTTP + JSON until Ctrl-C (a second Ctrl-C stops at
once). While it runs, it holds the store's lock: other cheesedb commands on
the same store wait for it to stop.

  --dir <folder>        the store (default: <home>/data, the command's own)
  --listen <host:port>  TCP address (default 127.0.0.1:7700; "" for none).
                        Any address other than this machine's needs TLS.
  --unix <path>         also serve on a Unix socket (mode 0600)
  --tls-cert <file>     TLS certificate and key, both or neither
  --tls-key <file>
  --tls-auto <folder>   use the certificate and key in this folder, making a
                        self-signed pair there first if it has none (for
                        containers); clients must trust its tls.crt
  --tls-host <names>    with --tls-auto: extra names and addresses the new
                        certificate is for, comma-separated (it always
                        covers localhost, 127.0.0.1 and ::1)
  --admin-key <file>    file holding the admin token's hash; turns on the
                        routes that create and delete agents
  --init                with --admin-key: create that file, print the admin
                        token once, and exit
  --quiet               no request log`

type serveConfig struct {
	dir, listen, unix string
	tlsCert, tlsKey   string
	tlsAuto, tlsHosts string
	adminKey          string
	init, quiet       bool
}

func serveCommand(args []string) error {
	fl := flag.NewFlagSet("serve", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	var c serveConfig
	fl.StringVar(&c.dir, "dir", dataDir, "")
	fl.StringVar(&c.listen, "listen", "127.0.0.1:7700", "")
	fl.StringVar(&c.unix, "unix", "", "")
	fl.StringVar(&c.tlsCert, "tls-cert", "", "")
	fl.StringVar(&c.tlsKey, "tls-key", "", "")
	fl.StringVar(&c.tlsAuto, "tls-auto", "", "")
	fl.StringVar(&c.tlsHosts, "tls-host", "", "")
	fl.StringVar(&c.adminKey, "admin-key", "", "")
	fl.BoolVar(&c.init, "init", false, "")
	fl.BoolVar(&c.quiet, "quiet", false, "")
	err := fl.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(serveUsage)
		return nil
	}
	if err != nil || fl.NArg() > 0 {
		return errors.New(serveUsage)
	}
	if c.init {
		return initAdminKey(c.adminKey, os.Stdout)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once the first signal has started the shutdown, stop listening for
	// signals, so a second Ctrl-C ends the process at once (the default)
	// instead of waiting for running requests.
	go func() { <-ctx.Done(); stop() }()
	return runServer(ctx, c, os.Stderr, func(where []string) {
		fmt.Fprintf(os.Stderr, "cheesedb %s serving %s on %s (Ctrl-C to stop)\n",
			version(), c.dir, strings.Join(where, " and "))
	})
}

// initAdminKey creates the admin key file and prints the admin token once.
// The file holds only the token's SHA-256, so reading it gives no way in.
func initAdminKey(path string, out io.Writer) error {
	if path == "" {
		return errors.New("--init needs --admin-key <file>: where to keep the admin token's hash")
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	tok := hex.EncodeToString(b[:])
	key := server.AdminKeyOf(tok)
	// The folder may not exist yet, e.g. a fresh CHEESEDB_DIR.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// O_EXCL: never overwrite a key that a running setup may depend on.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, hex.EncodeToString(key[:])); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(out, "admin key written to %s\nadmin token (shown once; keep it like a password):\n%s\n", path, tok)
	return nil
}

func readAdminKey(path string) (*[32]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no admin key at %s; make one first: cheesedb serve --admin-key %s --init", path, path)
	}
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	var key [32]byte
	if err != nil || len(raw) != len(key) {
		return nil, fmt.Errorf("%s does not hold an admin key (64 hex characters); make one with --init", path)
	}
	copy(key[:], raw)
	return &key, nil
}

// checkListen refuses to listen beyond this machine without TLS: a token
// sent in plain text over a network can be read by anyone on the way. It
// checks the address as written; runServer checks the bound one too.
func checkListen(addr string, tls bool) error {
	if addr == "" || tls {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %v", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--listen %q reaches other machines; that needs --tls-cert and --tls-key (or listen on 127.0.0.1)", addr)
}

// loopbackHosts lists the Host headers a browser-safe local listener
// accepts (against DNS rebinding). A non-loopback listener has TLS, whose certificate check
// already defeats DNS rebinding, so it gets no list.
func loopbackHosts(addr net.Addr) []string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil
	}
	return []string{addr.String(), "localhost:" + port, "127.0.0.1:" + port, "[::1]:" + port}
}

// runServer serves until ctx is done, then shuts down gracefully:
// stop accepting, let running requests finish (up to 30 s), close the
// agents and the store, which releases its lock. ready is told where it
// listens once it does.
func runServer(ctx context.Context, c serveConfig, logw io.Writer, ready func([]string)) error {
	if c.tlsAuto != "" && (c.tlsCert != "" || c.tlsKey != "") {
		return errors.New("give --tls-auto or --tls-cert and --tls-key, not both")
	}
	if c.tlsHosts != "" && c.tlsAuto == "" {
		return errors.New("--tls-host goes with --tls-auto")
	}
	if c.tlsAuto != "" {
		var err error
		if c.tlsCert, c.tlsKey, err = selfSigned(c.tlsAuto, splitList(c.tlsHosts), logw); err != nil {
			return err
		}
	}
	useTLS := c.tlsCert != "" || c.tlsKey != ""
	if useTLS && (c.tlsCert == "" || c.tlsKey == "") {
		return errors.New("give both --tls-cert and --tls-key, or neither")
	}
	// Load the certificate now, so a wrong file fails before "serving".
	var tlsConf *tls.Config
	if useTLS {
		pair, err := tls.LoadX509KeyPair(c.tlsCert, c.tlsKey)
		if err != nil {
			return fmt.Errorf("TLS certificate and key: %w", err)
		}
		tlsConf = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
	if c.listen == "" && c.unix == "" {
		return errors.New("nothing to listen on: give --listen or --unix")
	}
	if err := checkListen(c.listen, useTLS); err != nil {
		return err
	}
	opts := server.Options{Version: version()}
	if c.adminKey != "" {
		k, err := readAdminKey(c.adminKey)
		if err != nil {
			return err
		}
		opts.AdminKey = k
	}
	if !c.quiet {
		opts.Log = logw
	}

	if c.dir == dataDir {
		if err := makeHome(); err != nil {
			return err
		}
	}
	store, err := cheesedb.Open(c.dir)
	if errors.Is(err, cheesedb.ErrLocked) {
		return fmt.Errorf("%s is already open, by another cheesedb serve or a command such as cheesedb mcp: %w", c.dir, err)
	}
	if err != nil {
		return err
	}
	srv := server.New(store, opts)

	var (
		servers []*http.Server
		where   []string
		fails   = make(chan error, 2)
	)
	start := func(ln net.Listener, hosts []string, name string) {
		hs := &http.Server{
			Handler:           srv.Handler(hosts),
			ReadHeaderTimeout: 10 * time.Second, // against slow clients (slowloris)
			ReadTimeout:       time.Minute,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
			TLSConfig:         tlsConf,
		}
		servers = append(servers, hs)
		where = append(where, name)
		go func() {
			var err error
			if useTLS && ln.Addr().Network() == "tcp" {
				err = hs.ServeTLS(ln, "", "") // the pair is in TLSConfig
			} else {
				err = hs.Serve(ln)
			}
			if !errors.Is(err, http.ErrServerClosed) {
				fails <- err
			}
		}()
	}

	if c.listen != "" {
		ln, err := net.Listen("tcp", c.listen)
		if err != nil {
			return errors.Join(err, srv.Close(), store.Close())
		}
		// A name such as "localhost" passed checkListen, but could resolve
		// to an address other machines reach: check what was bound.
		if ip := ln.Addr().(*net.TCPAddr).IP; !useTLS && !ip.IsLoopback() {
			ln.Close()
			return errors.Join(fmt.Errorf("--listen %q bound to %s, which other machines reach; that needs TLS", c.listen, ip), srv.Close(), store.Close())
		}
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		start(ln, loopbackHosts(ln.Addr()), scheme+"://"+ln.Addr().String())
	}
	if c.unix != "" {
		ln, err := listenUnix(c.unix)
		if err != nil {
			for _, hs := range servers {
				hs.Close()
			}
			return errors.Join(err, srv.Close(), store.Close())
		}
		start(ln, nil, "unix:"+c.unix)
	}
	if ready != nil {
		ready(where)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-fails:
	}
	// Running requests finish; a write is never cut off halfway. After 30
	// s the connections are closed anyway; requests still running then
	// get 503 shutting_down once the store closes. (The Unix socket file
	// is removed by its listener's Close.)
	shut, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var errs []error
	for _, hs := range servers {
		if err := hs.Shutdown(shut); err != nil {
			errs = append(errs, err, hs.Close())
		}
	}
	errs = append(errs, srv.Close(), store.Close())
	return errors.Join(append([]error{serveErr}, errs...)...)
}

// listenUnix listens on a Unix socket, readable and writable by this user
// only. A socket file left by a crashed server is removed; anything else at
// the path, or a socket that answers, is refused.
func listenUnix(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("--unix %s: a file that is not a socket is in the way", path)
		}
		if conn, err := net.Dial("unix", path); err == nil {
			conn.Close()
			return nil, fmt.Errorf("--unix %s: another server is answering on it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	if runtime.GOOS == "windows" {
		// File modes do not guard sockets on Windows.
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			ln.Close()
			return nil, err
		}
		return ln, nil
	}

	// Made at path directly, the socket would have the default mode until
	// Chmod, and another user could connect in that moment. So make it in a
	// new folder only this user can enter (0700), set 0600, then link it
	// into place: at path it is 0600 from the start.
	tmp, err := os.MkdirTemp(filepath.Dir(path), ".sock")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp) // removes only the temp name; path stays
	tmpSock := filepath.Join(tmp, "s")
	ln, err := net.Listen("unix", tmpSock)
	if err != nil {
		return nil, err
	}
	// Go would remove tmpSock on Close; unixListener removes path instead.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(tmpSock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	// Link, unlike Rename, fails if another server made path meanwhile.
	if err := os.Link(tmpSock, path); err != nil {
		ln.Close()
		return nil, fmt.Errorf("--unix %s: %w", path, err)
	}
	return &unixListener{Listener: ln, path: path}, nil
}

// unixListener removes its socket file on Close.
type unixListener struct {
	net.Listener
	path string
	once sync.Once
}

func (l *unixListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { os.Remove(l.path) })
	return err
}
