// Package cheesedb is agent memory with a recall button: an append-only,
// hash-chained ledger where every memory records the sources it was learned
// from, and recalling a source removes what depended on it.
//
// One folder holds many agents; each agent is reachable only through its
// own token.
//
//	s, _ := cheesedb.Open("/data/cheesedb")  // once, at startup
//	tok, _ := s.CreateAgent("alice")         // once; keep the token
//	alice, _ := s.OpenAgent(tok)             // whenever Alice needs her memory
//	defer alice.Close()
//
// Isolation is by token, not by name. No method on an Agent takes an agent
// name or id, so code holding Alice's handle (a tool, a prompt-injected
// agent) has no parameter it could change to reach Bob. The only route to
// Bob is Bob's token.
//
// On disk:
//
//	<dir>/
//	  LOCK                 held while a Store is open: one Store per folder
//	  616c696365/          hex of the agent's name ("alice"), 0700
//	    agent.key          SHA-256 of the token, never the token itself
//	    000001.seg ...     the agent's log (internal/logfile)
//	    000001.seal ...
//
// See ARCHITECTURE.md section 3 and THREAT_MODEL.md.
package cheesedb

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/govinargo-labs/cheesedb/internal/ledger"
	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

// The types callers use, named here so they never need an internal package.
type (
	ID     = ledger.ID
	Leg    = ledger.Leg
	Mode   = ledger.Mode
	Reason = ledger.Reason
	Item   = ledger.Item
	Kind   = record.Type
	Report = logfile.Report
)

const (
	Fair       = ledger.Fair
	Strict     = ledger.Strict
	Untrusted  = ledger.Untrusted
	Erase      = ledger.Erase
	KindLot    = record.TypeLot
	KindMemory = record.TypeMemory
)

var (
	ErrUnknownID   = ledger.ErrUnknownID
	ErrDeadParent  = ledger.ErrDeadParent
	ErrBadLegs     = ledger.ErrBadLegs
	ErrBadArgument = ledger.ErrBadArgument
	ErrCorrupt     = logfile.ErrCorrupt

	ErrBadName     = errors.New("cheesedb: agent name must be 1-127 bytes")
	ErrAgentExists = errors.New("cheesedb: an agent with this name already exists")
	ErrBadToken    = errors.New("cheesedb: no agent has this token")
	ErrAgentInUse  = errors.New("cheesedb: agent is open; close every handle first")
	ErrClosed      = errors.New("cheesedb: closed")
	ErrLocked      = errors.New("cheesedb: already open, in another process or by another Open in this one")
)

// MaxNameBytes caps an agent name. Its folder name is the hex of the name,
// twice as long, and most file systems allow 255 bytes per name.
const MaxNameBytes = 127

// Token is an agent's secret: 32 random bytes, returned once by
// CreateAgent. The store keeps only its SHA-256, so the files alone cannot
// open an agent.
type Token [32]byte

// String returns the token as 64 hex characters, for keeping it in a
// config file or secret store.
func (t Token) String() string { return hex.EncodeToString(t[:]) }

// ParseToken reads a token written by String.
func ParseToken(s string) (Token, error) {
	var t Token
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(t) {
		return Token{}, ErrBadToken
	}
	copy(t[:], b)
	return t, nil
}

// keyOf is what the store keeps instead of the token.
func keyOf(t Token) [32]byte { return sha256.Sum256(t[:]) }

// Store is one folder of agents.
type Store struct {
	dir  string
	lock *os.File // the open LOCK file; closing it releases the lock

	// mu guards the two maps: which agents exist, and which are open. It is
	// held to create, open, close or delete an agent, never during an
	// agent's reads and writes, so agents never wait for each other's work.
	mu     sync.Mutex
	agents map[[32]byte]string  // token key -> agent folder name
	open   map[[32]byte]*shared // agents open right now
	closed bool

	// shut mirrors closed for handles, which check it on every call
	// without taking mu (that would make agents wait for each other).
	shut atomic.Bool
}

// shared is one open ledger, used by every handle opened with its token.
// Opening an agent twice gives two handles on ONE ledger, so there is
// never a second writer on the same files.
type shared struct {
	led     *ledger.Ledger
	report  Report
	handles int
}

const (
	keyFile    = "agent.key"
	keyVersion = 1
	keySize    = 1 + 32 + 4 // version, SHA-256 of the token, CRC-32C
	tmpPrefix  = ".tmp-"    // folders being created or deleted
	lockName   = "LOCK"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Open opens (or creates) the store in dir. dir's parent must exist.
// The folder is created readable only by this OS user (0700).
//
// Only one Store may have a folder open at a time: a second Open, from
// another process or this one, fails with ErrLocked until Close. Two
// writers on the same files would corrupt them.
func Open(dir string) (*Store, error) {
	dir = filepath.Clean(dir)
	if err := os.Mkdir(dir, 0o700); err == nil {
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}

	// Lock before anything else: load deletes leftover .tmp- folders,
	// which, without the lock, could be a create still running in another
	// process.
	lock, err := lockFile(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: lock, agents: map[[32]byte]string{}, open: map[[32]byte]*shared{}}
	if err := s.load(); err != nil {
		lock.Close()
		return nil, err
	}
	return s, nil
}

// load finds the agents in the folder and cleans up interrupted creates
// and deletes.
func (s *Store) load() error {
	dir := s.dir
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		// A leftover temporary folder is a create or delete that a crash
		// interrupted. A create returns its token only after the rename,
		// so nobody holds a token for it; a delete had already been asked
		// for. Either way it can go.
		if strings.HasPrefix(name, tmpPrefix) {
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
				return err
			}
			continue
		}
		if !e.IsDir() || !isAgentFolder(name) {
			continue // not ours (LOCK, .DS_Store)
		}
		key, err := readKey(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if other, dup := s.agents[key]; dup {
			return fmt.Errorf("%w: agents %s and %s have the same key", ErrCorrupt, other, name)
		}
		s.agents[key] = name
	}
	return nil
}

// isAgentFolder reports whether name could be the hex of a valid agent name.
func isAgentFolder(name string) bool {
	b, err := hex.DecodeString(name)
	return err == nil && len(b) >= 1 && len(b) <= MaxNameBytes && hex.EncodeToString(b) == name
}

// CreateAgent creates an agent and returns its token. The token is shown
// only this once: the store keeps just its hash, so a lost token cannot be
// recovered (delete the agent instead).
//
// Names are compared byte by byte (1-127 bytes, any bytes). "A" and "a" are
// different agents, and so are two names that look identical but are
// encoded differently in Unicode.
func (s *Store) CreateAgent(name string) (Token, error) {
	if len(name) == 0 || len(name) > MaxNameBytes {
		return Token{}, ErrBadName
	}
	// Hex uses only 0-9 and a-f, so no name can contain "/" or ".." on
	// disk, and distinct names stay distinct even on case-insensitive file
	// systems (macOS): "A" is "41", "a" is "61".
	folder := hex.EncodeToString([]byte(name))

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Token{}, ErrClosed
	}
	final := filepath.Join(s.dir, folder)
	if _, err := os.Lstat(final); err == nil {
		return Token{}, ErrAgentExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Token{}, err
	}

	var tok Token
	if _, err := rand.Read(tok[:]); err != nil {
		return Token{}, err
	}
	key := keyOf(tok)

	// Build the agent under a temporary name, then rename it into place.
	// A rename is atomic, so a crash leaves either no agent or a complete
	// one, never a folder without its key.
	tmp, err := s.tmpPath()
	if err != nil {
		return Token{}, err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return Token{}, err
	}
	if err := writeKey(tmp, key); err != nil {
		os.RemoveAll(tmp)
		return Token{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return Token{}, err
	}
	if err := syncDir(s.dir); err != nil {
		return Token{}, err
	}
	s.agents[key] = folder
	return tok, nil
}

// OpenAgent returns a handle on the agent this token belongs to. Opening the
// same agent again returns another handle on the same open ledger. The
// first open replays the agent's log, holding the store's lock meanwhile:
// other agents' opens wait, their reads and writes do not.
func (s *Store) OpenAgent(tok Token) (*Agent, error) {
	key := keyOf(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	folder, ok := s.agents[key]
	if !ok {
		return nil, ErrBadToken
	}
	sh := s.open[key]
	if sh == nil {
		led, rep, err := ledger.Open(filepath.Join(s.dir, folder), logfile.Options{})
		if err != nil {
			return nil, err
		}
		sh = &shared{led: led, report: rep}
		s.open[key] = sh
	}
	sh.handles++
	return &Agent{s: s, key: key, sh: sh}, nil
}

// DeleteAgent removes the agent this token belongs to, and all its files.
// It refuses while any handle on the agent is open.
//
// Deleting removes the files; it does not wipe the disk. Copies in backups
// and snapshots remain, and the SSD may keep old blocks (see README Limits).
func (s *Store) DeleteAgent(tok Token) error {
	key := keyOf(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	folder, ok := s.agents[key]
	if !ok {
		return ErrBadToken
	}
	if s.open[key] != nil {
		return ErrAgentInUse
	}
	// Rename first, so the agent disappears in one atomic step; a crash
	// during the slower RemoveAll leaves a temporary folder that Open
	// finishes removing.
	tmp, err := s.tmpPath()
	if err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(s.dir, folder), tmp); err != nil {
		return err
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	delete(s.agents, key)
	return os.RemoveAll(tmp)
}

// Close closes every open agent, then releases the folder for the next
// Open. Handles still held fail with ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.shut.Store(true)
	var errs []error
	for key, sh := range s.open {
		errs = append(errs, sh.led.Close())
		delete(s.open, key)
	}
	// Last: another process may open the folder as soon as this returns.
	errs = append(errs, s.lock.Close())
	return errors.Join(errs...)
}

// Agent is a handle on one agent's memory. Every method acts on this agent
// only; none takes an agent name or id.
type Agent struct {
	s      *Store
	key    [32]byte
	sh     *shared
	closed atomic.Bool
}

func (a *Agent) ledger() (*ledger.Ledger, error) {
	if a.closed.Load() || a.s.shut.Load() {
		return nil, ErrClosed
	}
	return a.sh.led, nil
}

// Report says what opening this agent's log found (records replayed, any
// torn tail cut off), so a recovery is never silent.
func (a *Agent) Report() Report { return a.sh.report }

// AddLot registers a source. See ledger.Ledger.AddLot.
func (a *Agent) AddLot(sourceType string, data []byte) (ID, error) {
	l, err := a.ledger()
	if err != nil {
		return 0, err
	}
	return l.AddLot(sourceType, data)
}

// Remember stores a memory with the legs it was learned from.
func (a *Agent) Remember(data []byte, legs ...Leg) (ID, error) {
	l, err := a.ledger()
	if err != nil {
		return 0, err
	}
	return l.Remember(data, legs...)
}

// Recall recalls a lot or memory and returns the memories that died.
func (a *Agent) Recall(id ID, m Mode, r Reason) ([]ID, error) {
	l, err := a.ledger()
	if err != nil {
		return nil, err
	}
	return l.Recall(id, m, r)
}

// Active returns the ids of this agent's live memories.
func (a *Agent) Active() ([]ID, error) {
	l, err := a.ledger()
	if err != nil {
		return nil, err
	}
	return l.Active(), nil
}

// Get returns one of this agent's lots or memories.
func (a *Agent) Get(id ID) (Item, error) {
	l, err := a.ledger()
	if err != nil {
		return Item{}, err
	}
	return l.Get(id)
}

// Compact erases the content of every dead lot or memory that still has it
// (for example those recalled as Untrusted) and returns how many. It runs
// only when called.
func (a *Agent) Compact() (int, error) {
	l, err := a.ledger()
	if err != nil {
		return 0, err
	}
	return l.Compact()
}

// Verify checks this agent's whole log on disk and returns its head
// fingerprint. Keep the head somewhere else to detect a rebuilt or
// shortened log later.
func (a *Agent) Verify() ([32]byte, error) {
	l, err := a.ledger()
	if err != nil {
		return [32]byte{}, err
	}
	return l.Verify()
}

// Close releases this handle. The agent's ledger closes when its last
// handle does.
func (a *Agent) Close() error {
	if a.closed.Swap(true) {
		return nil
	}
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[a.key] != a.sh {
		return nil // the store was closed, and the ledger with it
	}
	a.sh.handles--
	if a.sh.handles > 0 {
		return nil
	}
	delete(s.open, a.key)
	return a.sh.led.Close()
}

// tmpPath returns a fresh temporary folder path inside the store. The name
// is random rather than built from the agent's folder name: that name can
// already be 254 characters, and a prefix would push it past the 255-byte
// limit on file names.
func (s *Store) tmpPath() (string, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, tmpPrefix+hex.EncodeToString(r[:])), nil
}

// writeKey writes agent.key into a new agent folder and fsyncs it. The
// folder is renamed into place afterwards, which is what makes it visible.
func writeKey(dir string, key [32]byte) error {
	b := make([]byte, keySize)
	b[0] = keyVersion
	copy(b[1:], key[:])
	binary.LittleEndian.PutUint32(b[33:], crc32.Checksum(b[:33], castagnoli))
	f, err := os.OpenFile(filepath.Join(dir, keyFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = syncDir(dir)
	}
	return err
}

func readKey(dir string) ([32]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, keyFile))
	if errors.Is(err, fs.ErrNotExist) {
		return [32]byte{}, fmt.Errorf("%w: agent folder %s has no %s", ErrCorrupt, filepath.Base(dir), keyFile)
	}
	if err != nil {
		return [32]byte{}, err
	}
	if len(b) != keySize || b[0] != keyVersion ||
		crc32.Checksum(b[:33], castagnoli) != binary.LittleEndian.Uint32(b[33:]) {
		return [32]byte{}, fmt.Errorf("%w: %s of agent folder %s is damaged", ErrCorrupt, keyFile, filepath.Base(dir))
	}
	var key [32]byte
	copy(key[:], b[1:33])
	return key, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
