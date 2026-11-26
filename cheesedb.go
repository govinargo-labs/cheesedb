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
//	    state.snap         a snapshot of the agent's state, so opening replays
//	                       only newer records; only once the log has 1,000
//	                       records (internal/ledger/snapshot.go)
//	    index-64656661756c74/   a vector index ("default"), only if created
//	      index.meta       dims and label
//	      000001.seg ...   its own log of vectors
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
	ID        = ledger.ID
	Leg       = ledger.Leg
	Mode      = ledger.Mode
	Reason    = ledger.Reason
	Item      = ledger.Item
	Kind      = record.Type
	Report    = logfile.Report
	Hit       = ledger.Hit
	Embedding = ledger.Embedding
	IndexInfo = ledger.IndexInfo
)

const (
	Fair       = ledger.Fair
	Strict     = ledger.Strict
	Untrusted  = ledger.Untrusted
	Erase      = ledger.Erase
	KindLot    = record.TypeLot
	KindMemory = record.TypeMemory

	// MaxDims caps the values per vector in an index.
	MaxDims = ledger.MaxDims
)

var (
	ErrUnknownID   = ledger.ErrUnknownID
	ErrDeadParent  = ledger.ErrDeadParent
	ErrBadLegs     = ledger.ErrBadLegs
	ErrBadArgument = ledger.ErrBadArgument
	ErrCorrupt     = logfile.ErrCorrupt
	ErrNoIndex     = ledger.ErrNoIndex
	ErrIndexExists = ledger.ErrIndexExists
	ErrBadVector   = ledger.ErrBadVector
	ErrRecalled    = ledger.ErrRecalled

	// ErrBroken means a write to this agent's log failed (a full disk, an
	// I/O error). The agent refuses further writes until it is opened
	// again: retrying an fsync that failed cannot be trusted.
	ErrBroken = logfile.ErrBroken
	// ErrTooLarge means a lot or memory is over the 1 MiB record limit.
	ErrTooLarge = record.ErrTooLarge

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

	// mu guards the maps: which agents exist, and which are open. It is
	// held to create, open, close or delete an agent, never during an
	// agent's reads and writes or its replay, so agents never wait for
	// each other's work.
	mu      sync.Mutex
	agents  map[[32]byte]string  // token key -> agent folder name
	opening map[[32]byte]*shared // agents whose log is being replayed (see OpenAgent)
	open    map[[32]byte]*shared // agents open right now
	closing map[[32]byte]*shared // agents whose ledger is closing (see Agent.Close)
	closed  bool

	closeDone chan struct{} // closed when the first Close has finished

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
	ready   chan struct{} // closed once the replay has finished, well or not
	err     error         // the replay's error, set before ready is closed
	done    chan struct{} // closed once led is closed
}

const (
	keyFile    = "agent.key"
	keyVersion = 1
	keySize    = 1 + 32 + 4 // version, SHA-256 of the token, CRC-32C
	tmpPrefix  = ".tmp-"    // folders being created or deleted
	lockName   = "LOCK"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// openLedger replays an agent's log. A variable so tests can play a slow
// replay.
var openLedger = ledger.Open

// replayWait is called when an OpenAgent starts waiting for another call's
// replay of the same agent. Tests replace it to know a waiter is there.
var replayWait = func() {}

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
	s := &Store{dir: dir, lock: lock, agents: map[[32]byte]string{}, opening: map[[32]byte]*shared{}, open: map[[32]byte]*shared{}, closing: map[[32]byte]*shared{}, closeDone: make(chan struct{})}
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
// same agent again returns another handle on the same open ledger.
//
// The first open replays the agent's log, which can take seconds for a
// large agent. It does so without the store's lock, so other agents open,
// close and work as usual meanwhile; only opens of the same agent wait for
// it, and then share its ledger (or its error).
func (s *Store) OpenAgent(tok Token) (*Agent, error) {
	key := keyOf(tok)
	s.mu.Lock()
	for {
		// Its last handle is closing right now: wait, since a second
		// ledger on the same files would be a second writer.
		if c := s.closing[key]; c != nil {
			s.mu.Unlock()
			<-c.done
			s.mu.Lock()
			if s.closing[key] == c {
				delete(s.closing, key)
			}
			continue
		}
		// Another call is replaying it: wait and share the outcome.
		if o := s.opening[key]; o != nil {
			s.mu.Unlock()
			replayWait()
			<-o.ready
			if o.err != nil {
				return nil, o.err
			}
			s.mu.Lock()
			continue // it is in open now, or already closing again
		}
		break
	}
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	folder, ok := s.agents[key]
	if !ok {
		s.mu.Unlock()
		return nil, ErrBadToken
	}
	if sh := s.open[key]; sh != nil {
		sh.handles++
		s.mu.Unlock()
		return &Agent{s: s, key: key, sh: sh}, nil
	}

	// First open: list it as opening, then replay without the lock.
	sh := &shared{ready: make(chan struct{}), done: make(chan struct{})}
	s.opening[key] = sh
	s.mu.Unlock()
	led, rep, err := openLedger(filepath.Join(s.dir, folder), logfile.Options{})

	s.mu.Lock()
	delete(s.opening, key)
	if err == nil && s.closed {
		// The store closed during the replay and waits for this ledger to
		// close before it releases the folder (see Close).
		s.mu.Unlock()
		err = errors.Join(ErrClosed, led.Close())
	} else if err == nil {
		sh.led, sh.report, sh.handles = led, rep, 1
		s.open[key] = sh
		s.mu.Unlock()
	} else {
		s.mu.Unlock()
	}
	sh.err = err
	close(sh.ready) // after err is set: waiters read it once ready is closed
	if err != nil {
		return nil, err
	}
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
	if s.opening[key] != nil || s.open[key] != nil || s.closing[key] != nil {
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
	if s.closed {
		// Another Close is running or done. It lets go of mu while it
		// waits for replays, so wait for it to finish: returning sooner
		// would let the caller reopen the folder while files are open.
		s.mu.Unlock()
		<-s.closeDone
		return nil
	}
	defer s.mu.Unlock()
	defer close(s.closeDone) // runs first: after the folder is released
	s.closed = true
	s.shut.Store(true)
	// Agents being replayed right now: wait for each, without mu, which
	// its replay needs to finish. It sees closed and closes its own
	// ledger, so no file stays open once the folder is released.
	for len(s.opening) > 0 {
		var o *shared
		for _, o = range s.opening {
			break
		}
		s.mu.Unlock()
		<-o.ready
		s.mu.Lock()
	}
	var errs []error
	for key, sh := range s.open {
		errs = append(errs, sh.led.Close())
		delete(s.open, key)
	}
	// Agents whose last handle is closing right now: their files must be
	// closed before the folder is released.
	for key, sh := range s.closing {
		<-sh.done
		delete(s.closing, key)
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

// Report says what opening this agent's log found (records, how many came
// from the snapshot, any torn tail cut off), so a recovery is never silent.
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

// WouldRecall returns the memories Recall(id, m, ...) would remove now,
// without changing anything, so a program can ask a person before a
// permanent recall. A write in between can change the answer; Recall
// returns what it actually removed.
func (a *Agent) WouldRecall(id ID, m Mode) ([]ID, error) {
	l, err := a.ledger()
	if err != nil {
		return nil, err
	}
	return l.WouldRecall(id, m)
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
// only when called. It also erases the vectors of dead items, and vectors
// replaced by a newer one, in every index (not counted).
func (a *Agent) Compact() (int, error) {
	l, err := a.ledger()
	if err != nil {
		return 0, err
	}
	return l.Compact()
}

// Verify checks this agent's whole log on disk and returns its head
// fingerprint. Keep the head somewhere else to detect a rebuilt or
// shortened log later. Opening from a snapshot does not re-read older
// records, so Verify is the full check (ARCHITECTURE.md section 6).
func (a *Agent) Verify() ([32]byte, error) {
	l, err := a.ledger()
	if err != nil {
		return [32]byte{}, err
	}
	return l.Verify()
}

// CreateIndex creates an empty vector index for vectors of dims values.
// label is free text kept with it, such as the embedding model's name;
// cheesedb never reads it. Vectors from different models cannot be
// compared, so use one index per model. See ARCHITECTURE.md section 9.
//
// cheesedb never calls a model: the caller computes every vector.
func (a *Agent) CreateIndex(name string, dims int, label string) error {
	l, err := a.ledger()
	if err != nil {
		return err
	}
	return l.CreateIndex(name, dims, label)
}

// DropIndex deletes an index and every vector in it. It is also how to
// recover from a damaged index: drop it, create it, embed again.
func (a *Agent) DropIndex(name string) error {
	l, err := a.ledger()
	if err != nil {
		return err
	}
	return l.DropIndex(name)
}

// Indexes describes this agent's indexes, in name order. It reads each
// one from disk the first time, to count its vectors.
func (a *Agent) Indexes() ([]IndexInfo, error) {
	l, err := a.ledger()
	if err != nil {
		return nil, err
	}
	return l.Indexes(), nil
}

// Embed stores vec as the vector of lot or memory id in the named index,
// replacing any earlier one. id must be alive. The vector is scaled to
// length 1 before it is stored; NaN, infinity, an all-zero vector or the
// wrong number of values are refused with ErrBadVector.
//
// A Recall removes the vectors of everything it removes at once; with
// Erase, they are erased from disk too.
func (a *Agent) Embed(index string, id ID, vec []float32) error {
	l, err := a.ledger()
	if err != nil {
		return err
	}
	return l.Embed(index, id, vec)
}

// EmbedBatch is Embed for many vectors, sharing one disk sync per batch of
// up to 1 MiB (about 320 vectors of 768 values). Every item is checked
// before anything is written. If a write fails, the batches before it are
// stored; embedding them again is harmless.
func (a *Agent) EmbedBatch(index string, items []Embedding) error {
	l, err := a.ledger()
	if err != nil {
		return err
	}
	return l.EmbedBatch(index, items)
}

// Search returns up to k lots or memories whose vectors are closest to
// query (cosine similarity), best first. Every result is alive. Read the
// text with Get. The search is exact: it compares query with every vector.
// The first search of an index reads it from disk.
func (a *Agent) Search(index string, query []float32, k int) ([]Hit, error) {
	l, err := a.ledger()
	if err != nil {
		return nil, err
	}
	return l.Search(index, query, k)
}

// VerifyIndex checks one index's log on disk like Verify checks the main
// log, and returns the index's own head fingerprint. Indexes never change
// the head that Verify returns.
func (a *Agent) VerifyIndex(index string) ([32]byte, error) {
	l, err := a.ledger()
	if err != nil {
		return [32]byte{}, err
	}
	return l.VerifyIndex(index)
}

// Close releases this handle. The agent's ledger closes when its last
// handle does.
func (a *Agent) Close() error {
	if a.closed.Swap(true) {
		return nil
	}
	s := a.s
	s.mu.Lock()
	if s.open[a.key] != a.sh {
		s.mu.Unlock()
		return nil // the store was closed, and the ledger with it
	}
	a.sh.handles--
	if a.sh.handles > 0 {
		s.mu.Unlock()
		return nil
	}
	// Closing the ledger waits for its write in progress, which can take
	// a while (a large Embed, a Compact), so it runs without mu: other
	// agents must not wait for it. Meanwhile the agent is listed as
	// closing, so OpenAgent waits and DeleteAgent refuses.
	delete(s.open, a.key)
	s.closing[a.key] = a.sh
	s.mu.Unlock()
	err := a.sh.led.Close()
	close(a.sh.done) // before taking mu: Store.Close may hold it, waiting for this
	s.mu.Lock()
	if s.closing[a.key] == a.sh {
		delete(s.closing, a.key)
	}
	s.mu.Unlock()
	return err
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
