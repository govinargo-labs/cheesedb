package ledger

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
	"github.com/govinargo-labs/cheesedb/internal/vecindex"
)

// Vector indexes: search by meaning. See ARCHITECTURE.md section 9.
//
// The caller embeds a lot or memory with its own model and hands the vector
// to Embed; Search returns the ids closest to a query vector. Each index
// lives in its own folder inside the agent's folder, with its own log
// written by the same logfile code as the main log:
//
//	<agent>/index-<hex of name>/
//	  index.meta           dims and label, written once at creation
//	  000001.seg ...       one VECTOR record per embedding
//
// The main log is the truth and the index follows it:
//   - nothing here ever writes to the main log, so an agent without an
//     index has exactly the files and head it had before indexes existed;
//   - a vector whose lot or memory is dead is never searchable, and one
//     whose content was erased is erased too;
//   - an index is read only when first used, and an index that cannot be
//     read never stops the agent opening.

const (
	indexPrefix    = "index-"
	indexTmpPrefix = ".tmp-index-" // folders being created or dropped
	metaFile       = "index.meta"
	metaVersion    = 1
	encFloat32     = 1 // float32, little-endian
	metricCosine   = 1 // cosine similarity on vectors scaled to length 1

	// MaxIndexName caps an index name. Its folder name is "index-" plus
	// the hex of the name (twice as long), within the 255-byte limit.
	MaxIndexName = 100
	MaxLabel     = 255
	MaxDims      = vecindex.MaxDims
)

var (
	ErrNoIndex     = errors.New("ledger: no index with this name")
	ErrIndexExists = errors.New("ledger: an index with this name already exists")
	ErrBadVector   = vecindex.ErrBadVector
	ErrRecalled    = errors.New("ledger: lot or memory is recalled")
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Hit is one search result, best first.
type Hit struct {
	ID    ID
	Score float32 // cosine similarity, from -1 to 1
}

// Embedding is one vector for EmbedBatch.
type Embedding struct {
	ID     ID
	Vector []float32
}

// IndexInfo describes one index. Err is set when the index cannot be
// loaded; such an index can only be dropped.
type IndexInfo struct {
	Name    string
	Dims    int
	Label   string
	Vectors int            // live vectors: one per alive lot or memory embedded
	Report  logfile.Report // what loading its log found
	Err     error
}

// vindex is one index of this agent. It is found on disk when the ledger
// opens, and loaded (log opened, vectors in memory) on first use.
type vindex struct {
	name string
	dir  string

	// mu guards meta, log, report and idx for searches. Everything that
	// changes them also holds the ledger's write lock.
	mu     sync.RWMutex
	dims   int
	label  string
	log    *logfile.Log // nil until loaded
	report logfile.Report
	idx    *vecindex.Index

	// stale lists VECTOR records whose content is still on disk but that
	// are no longer searchable: their target died, or a newer vector
	// replaced them. Compact and Erase redact them. Holders of the
	// ledger's write lock only.
	stale []staleRec
}

type staleRec struct {
	rec    uint64 // VECTOR record id in the index log
	target ID
}

func indexFolder(name string) string { return indexPrefix + hex.EncodeToString([]byte(name)) }

func checkIndexName(name string) error {
	if len(name) == 0 || len(name) > MaxIndexName {
		return fmt.Errorf("%w: index name must be 1-%d bytes", ErrBadArgument, MaxIndexName)
	}
	return nil
}

// findIndexes lists the agent's index folders, without reading them, and
// tries to remove folders that a crash left half created or half dropped.
// It runs in Open, which is the only cost an agent without indexes pays:
// one directory listing.
func (l *Ledger) findIndexes() error {
	l.indexes = map[string]*vindex{}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, indexTmpPrefix) {
			// Best effort: a leftover must not stop the agent opening.
			// Erase and Compact try again, and refuse while it stays
			// (see removeLeftovers).
			os.RemoveAll(filepath.Join(l.dir, name))
			continue
		}
		hexName, ok := strings.CutPrefix(name, indexPrefix)
		if !ok || !e.IsDir() {
			continue
		}
		b, err := hex.DecodeString(hexName)
		if err != nil || checkIndexName(string(b)) != nil || indexFolder(string(b)) != name {
			continue // not ours
		}
		l.indexes[string(b)] = &vindex{name: string(b), dir: filepath.Join(l.dir, name)}
	}
	return nil
}

// sortedIndexes returns the indexes in name order, so work over all of
// them runs in the same order every time. Holders of l.write only.
func (l *Ledger) sortedIndexes() []*vindex {
	out := make([]*vindex, 0, len(l.indexes))
	for _, ix := range l.indexes {
		out = append(out, ix)
	}
	slices.SortFunc(out, func(a, b *vindex) int { return strings.Compare(a.name, b.name) })
	return out
}

// CreateIndex creates an empty index for vectors of dims values. label is
// free text kept with it, such as the embedding model's name; the engine
// never reads it. Every vector in one index must come from the same model.
func (l *Ledger) CreateIndex(name string, dims int, label string) error {
	if err := checkIndexName(name); err != nil {
		return err
	}
	if dims < 1 || dims > MaxDims {
		return fmt.Errorf("%w: dims must be 1-%d, got %d", ErrBadArgument, MaxDims, dims)
	}
	if len(label) > MaxLabel {
		return fmt.Errorf("%w: label is %d bytes, max %d", ErrBadArgument, len(label), MaxLabel)
	}
	l.write.Lock()
	defer l.write.Unlock()
	if l.closed {
		return logfile.ErrClosed
	}
	if l.indexes[name] != nil {
		return ErrIndexExists
	}
	final := filepath.Join(l.dir, indexFolder(name))
	if _, err := os.Lstat(final); err == nil {
		return ErrIndexExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// Build the folder under a temporary name, then rename it into place:
	// a crash leaves either no index or a complete one, never a folder
	// without its meta file. Leftovers are removed by the next Open.
	tmp, err := l.tmpIndexPath()
	if err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	if err := writeMeta(tmp, dims, label); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	// The folder is in place: list it now, whatever happens next, so the
	// list always matches the disk.
	l.mu.Lock()
	l.indexes[name] = &vindex{name: name, dir: final}
	l.mu.Unlock()
	if err := syncDir(l.dir); err != nil {
		return fmt.Errorf("index %q is created, but a crash may still undo it: %w", name, err)
	}
	return nil
}

// DropIndex deletes an index and every vector in it. It is also the way
// out of a damaged index: drop it, create it again, embed again.
func (l *Ledger) DropIndex(name string) error {
	l.write.Lock()
	defer l.write.Unlock()
	if l.closed {
		return logfile.ErrClosed
	}
	ix := l.indexes[name]
	if ix == nil {
		return ErrNoIndex
	}
	// Close first: an open segment cannot be renamed on Windows, and no
	// search may use the index once it starts disappearing.
	closeErr := ix.unload()

	// Rename first, like DeleteAgent: the index disappears in one atomic
	// step. A crash, or a failure, during the slower RemoveAll leaves a
	// temporary folder, which the next Open, Erase or Compact removes.
	tmp, err := l.tmpIndexPath()
	if err != nil {
		return errors.Join(closeErr, err)
	}
	if err := os.Rename(ix.dir, tmp); err != nil {
		return errors.Join(closeErr, err)
	}
	// Gone under its name: forget it now, whatever happens next, so the
	// list always matches the disk. If the fsync below fails, a crash may
	// bring the index back, which is harmless.
	l.mu.Lock()
	delete(l.indexes, name)
	l.mu.Unlock()
	return errors.Join(closeErr, syncDir(l.dir), os.RemoveAll(tmp))
}

// removeLeftovers removes index folders that a crash or a failed DropIndex
// left behind under a temporary name. They may hold vectors of items an
// Erase is about to erase, so Erase and Compact call this first and refuse
// while it fails. Holders of l.write only (or Open).
func (l *Ledger) removeLeftovers() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), indexTmpPrefix) {
			errs = append(errs, os.RemoveAll(filepath.Join(l.dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// tmpIndexPath returns a fresh temporary folder path in the agent folder.
// It starts with ".tmp-index-" and never ends in ".tmp": the log removes
// files ending in ".tmp" with os.Remove, which fails on a folder.
func (l *Ledger) tmpIndexPath() (string, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	return filepath.Join(l.dir, indexTmpPrefix+hex.EncodeToString(r[:])), nil
}

// index.meta layout (little-endian):
//
//	0  1  version
//	1  4  dims
//	5  1  encoding (1 = float32)
//	6  1  metric (1 = cosine)
//	7  1  label length n
//	8  n  label
//	8+n 4 CRC-32C of everything before it
func writeMeta(dir string, dims int, label string) error {
	b := []byte{metaVersion}
	b = binary.LittleEndian.AppendUint32(b, uint32(dims))
	b = append(b, encFloat32, metricCosine, byte(len(label)))
	b = append(b, label...)
	b = binary.LittleEndian.AppendUint32(b, crc32.Checksum(b, castagnoli))
	f, err := os.OpenFile(filepath.Join(dir, metaFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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

func readMeta(dir string) (dims int, label string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", fmt.Errorf("%w: %s is missing", logfile.ErrCorrupt, metaFile)
	}
	if err != nil {
		return 0, "", err
	}
	bad := fmt.Errorf("%w: %s is damaged", logfile.ErrCorrupt, metaFile)
	if len(b) < 12 || len(b) != 12+int(b[7]) {
		return 0, "", bad
	}
	n := len(b) - 4
	if crc32.Checksum(b[:n], castagnoli) != binary.LittleEndian.Uint32(b[n:]) {
		return 0, "", bad
	}
	dims = int(binary.LittleEndian.Uint32(b[1:]))
	if b[0] != metaVersion || b[5] != encFloat32 || b[6] != metricCosine || dims < 1 || dims > MaxDims {
		return 0, "", bad
	}
	return dims, string(b[8:n]), nil
}

// load opens ix's log and rebuilds its vectors from it, if not done yet.
// Callers hold l.write, which is also what makes reading l.st safe here.
//
// Replay rules (the main log is the truth):
//   - a VECTOR record for an id the main log does not have, or whose tag
//     does not match that id's record, is corruption (files from two
//     different points in time); the index fails to load;
//   - a redacted record is skipped;
//   - a vector of a live target is searchable; a later one replaces it;
//   - a vector of a dead target is stale: kept on disk until Compact;
//   - a vector of an ERASED target is erased now. This finishes an erase
//     that stopped before reaching this index, whatever the reason.
func (l *Ledger) load(ix *vindex) error {
	if l.closed {
		return logfile.ErrClosed
	}
	if l.indexes[ix.name] != ix {
		return ErrNoIndex // dropped meanwhile
	}
	if ix.log != nil {
		return nil
	}
	dims, label, err := readMeta(ix.dir)
	if err != nil {
		return fmt.Errorf("index %q: %w", ix.name, err)
	}
	idx := vecindex.New(dims)
	vec := make([]float32, dims)
	var stale []staleRec
	var sweep []uint64
	log, rep, err := logfile.Open(ix.dir, l.opts, func(r *record.Record) error {
		target, err := l.applyVector(r, vec)
		if errors.Is(err, errMalformed) {
			return fmt.Errorf("%w: record %d: %v", logfile.ErrCorrupt, r.ID, err)
		}
		if err != nil {
			return fmt.Errorf("record %d: %w", r.ID, err) // reading the main log failed
		}
		switch n := l.st.node(target); {
		case r.Redacted():
		case n.redacted:
			sweep = append(sweep, r.ID)
		case n.dead:
			stale = append(stale, staleRec{r.ID, target})
		default:
			if old, ok := idx.Put(uint64(target), r.ID, vec); ok {
				stale = append(stale, staleRec{old, target})
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("index %q: %w", ix.name, err)
	}
	if err := log.Redact(sweep); err != nil {
		log.Close()
		return fmt.Errorf("index %q: erasing vectors of erased items: %w", ix.name, err)
	}
	ix.mu.Lock()
	ix.dims, ix.label, ix.log, ix.report, ix.idx, ix.stale = dims, label, log, rep, idx, stale
	ix.mu.Unlock()
	return nil
}

// applyVector checks one record of an index log and decodes its vector
// into vec (unless it is redacted). It returns the target.
func (l *Ledger) applyVector(r *record.Record, vec []float32) (ID, error) {
	if r.Type != record.TypeVector {
		return 0, fmt.Errorf("%w: type %d in an index log", errMalformed, r.Type)
	}
	target, tag, err := decodeVector(r.Structure)
	if err != nil {
		return 0, err
	}
	if !l.st.isLotOrMemory(target) {
		return 0, fmt.Errorf("%w: vector for %d, which the main log does not have as a lot or memory", errMalformed, target)
	}
	if !r.Redacted() {
		data := r.Data()
		if len(data) != 4*len(vec) {
			return 0, fmt.Errorf("%w: vector of %d bytes, want %d", errMalformed, len(data), 4*len(vec))
		}
		var sum float64
		for i := range vec {
			vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
			f := float64(vec[i])
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return 0, fmt.Errorf("%w: value %d is %v", errMalformed, i, vec[i])
			}
			sum += f * f
		}
		// Embed stores vectors of length 1; float32 rounding moves the sum
		// of squares by about 1e-7. Anything further off was not written
		// by Embed, and would give scores outside -1 to 1.
		if math.Abs(sum-1) > 1e-3 {
			return 0, fmt.Errorf("%w: vector of length %.4f, want 1", errMalformed, math.Sqrt(sum))
		}
	}
	// Checked last: it reads the target's record from the main log.
	want, err := l.tagOf(target)
	if err != nil {
		return 0, err
	}
	if tag != want {
		return 0, fmt.Errorf("%w: vector for %d was written for a different record %d (the index and the main log are from different histories)", errMalformed, target, target)
	}
	return target, nil
}

// tagOf returns the tag a VECTOR record for id must carry: the first bytes
// of id's chain hash (see tagSize). It reads id's record from the main log,
// once per vector at Embed and at load. Callers hold l.write (or are in
// Open).
func (l *Ledger) tagOf(id ID) ([tagSize]byte, error) {
	var tag [tagSize]byte
	r, err := l.log.Read(uint64(id))
	if err != nil {
		return tag, err
	}
	h := r.ChainHash()
	copy(tag[:], h[:])
	return tag, nil
}

// loadAll loads every index on disk and stops at the first that fails.
// Erase and Compact use it so they never erase text while vectors of it
// stay behind in an index they cannot reach. Holders of l.write only.
func (l *Ledger) loadAll() error {
	if err := l.removeLeftovers(); err != nil {
		return fmt.Errorf("removing a dropped index: %w", err)
	}
	for _, ix := range l.sortedIndexes() {
		if err := l.load(ix); err != nil {
			return fmt.Errorf("%w (drop the index, then retry)", err)
		}
	}
	return nil
}

// loaded returns index name, loading it first if needed.
func (l *Ledger) loaded(name string) (*vindex, error) {
	l.mu.RLock()
	ix := l.indexes[name]
	l.mu.RUnlock()
	if ix != nil {
		ix.mu.RLock()
		ok := ix.idx != nil
		ix.mu.RUnlock()
		if ok {
			return ix, nil
		}
	}
	l.write.Lock()
	defer l.write.Unlock()
	// Look again: the index may have been dropped, or created, meanwhile.
	if ix = l.indexes[name]; ix == nil {
		return nil, ErrNoIndex
	}
	return ix, l.load(ix)
}

// Embed stores vec as the vector of lot or memory id in the named index,
// replacing any earlier one. id must be alive.
func (l *Ledger) Embed(name string, id ID, vec []float32) error {
	return l.EmbedBatch(name, []Embedding{{id, vec}})
}

// EmbedBatch stores many vectors with one fsync per MaxBatchBytes of
// records (about 320 vectors of 768 values). If it fails, the vectors
// before the failing batch are stored; embedding them again is harmless.
func (l *Ledger) EmbedBatch(name string, items []Embedding) error {
	if len(items) == 0 {
		return fmt.Errorf("%w: no vectors", ErrBadArgument)
	}
	l.write.Lock()
	defer l.write.Unlock()
	ix := l.indexes[name]
	if ix == nil {
		return ErrNoIndex
	}
	if err := l.load(ix); err != nil {
		return err
	}
	// Every check comes before the first write, so a bad item stores
	// nothing. Reading st without mu is safe: only l.write holders change it.
	vecs := make([][]float32, len(items))
	tags := make([][tagSize]byte, len(items))
	for i, it := range items {
		v, err := vecindex.Normalize(it.Vector, ix.dims)
		if err != nil {
			return fmt.Errorf("vector for %d: %w", it.ID, err)
		}
		if !l.st.isLotOrMemory(it.ID) {
			return fmt.Errorf("%w: %d", ErrUnknownID, it.ID)
		}
		if l.st.node(it.ID).dead {
			return fmt.Errorf("%w: %d", ErrRecalled, it.ID)
		}
		if tags[i], err = l.tagOf(it.ID); err != nil {
			return err
		}
		vecs[i] = v
	}

	for start := 0; start < len(items); {
		var entries []logfile.Entry
		size := 0
		for i := start; i < len(items); i++ {
			e := logfile.Entry{Type: record.TypeVector, Structure: encodeVector(items[i].ID, tags[i]), Data: vectorBytes(vecs[i])}
			n := record.PrefixSize + record.HeaderSize + len(e.Structure) + record.SaltSize + len(e.Data)
			if len(entries) > 0 && size+n > logfile.MaxBatchBytes {
				break
			}
			entries = append(entries, e)
			size += n
		}
		first, err := ix.log.AppendBatch(entries)
		if err != nil {
			// Some of these records may be on disk anyway (a short write
			// before a full disk), and the log now refuses every write.
			// Unloading makes the next use read the log again, so what
			// did reach the disk is known, and reachable by Erase.
			return errors.Join(err, ix.unload())
		}
		// On disk; now searchable.
		ix.mu.Lock()
		for i := range entries {
			it := start + i
			if old, ok := ix.idx.Put(uint64(items[it].ID), first+uint64(i), vecs[it]); ok {
				ix.stale = append(ix.stale, staleRec{old, items[it].ID})
			}
		}
		ix.mu.Unlock()
		start += len(entries)
	}
	return nil
}

func vectorBytes(v []float32) []byte {
	b := make([]byte, 0, 4*len(v))
	for _, x := range v {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(x))
	}
	return b
}

// Search returns the k lots or memories whose vectors are closest to
// query, best first. Every result was alive when the search ran. The first
// search of an index loads it, which reads its whole log. It returns fewer
// than k only when the index holds fewer live vectors, or, rarely, when
// recalls keep landing during the search.
func (l *Ledger) Search(name string, query []float32, k int) ([]Hit, error) {
	if k < 1 {
		return nil, fmt.Errorf("%w: k must be at least 1, got %d", ErrBadArgument, k)
	}
	for try := 1; ; try++ {
		ix, err := l.loaded(name)
		if err != nil {
			return nil, err
		}
		ix.mu.RLock()
		if ix.idx == nil {
			ix.mu.RUnlock()
			continue // dropped or unloaded meanwhile: look it up again
		}
		q, err := vecindex.Normalize(query, ix.dims)
		var raw []vecindex.Hit
		if err == nil {
			// Holding ix.mu (read) keeps every row's record unredacted:
			// a vector leaves the index before its record is erased.
			raw, err = ix.idx.Search(q, k, ix.readVector)
		}
		ix.mu.RUnlock()
		if err != nil {
			return nil, err
		}

		// A Recall marks the dead in the state first and removes their
		// rows right after; one that ran between those two steps must not
		// leak through. Checking k ids against the state closes that window.
		hits := make([]Hit, 0, len(raw))
		l.mu.RLock()
		for _, h := range raw {
			if !l.st.node(ID(h.Target)).dead {
				hits = append(hits, Hit{ID(h.Target), h.Score})
			}
		}
		l.mu.RUnlock()
		// If something died, the next best live vectors were left out.
		// The Recall that killed it removes its rows before it lets go of
		// the write lock, so wait for that, then search again. A few tries
		// are plenty: each one needs yet another Recall to land in between.
		if len(hits) == len(raw) || len(raw) < k || try == 3 {
			return hits, nil
		}
		l.write.Lock()
		//lint:ignore SA2001 an empty critical section is the point: it waits for the Recall
		l.write.Unlock()
	}
}

// readVector reads the full float32 vector of VECTOR record rec from the
// index log; Search needs it for the rows it scores exactly. The record was
// checked when the index was loaded (applyVector). Callers hold ix.mu.
func (ix *vindex) readVector(rec uint64) ([]float32, error) {
	r, err := ix.log.Read(rec)
	if err != nil {
		return nil, err
	}
	data := r.Data()
	if r.Redacted() || len(data) != 4*ix.dims {
		return nil, fmt.Errorf("%w: vector record %d holds %d bytes, want %d", logfile.ErrCorrupt, rec, len(data), 4*ix.dims)
	}
	vec := make([]float32, ix.dims)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
	}
	return vec, nil
}

// Indexes describes every index of this agent, in name order. It loads
// each one to count its vectors.
func (l *Ledger) Indexes() []IndexInfo {
	l.write.Lock()
	defer l.write.Unlock()
	var out []IndexInfo
	for _, ix := range l.sortedIndexes() {
		info := IndexInfo{Name: ix.name, Err: l.load(ix)}
		if info.Err == nil {
			info.Dims, info.Label, info.Vectors, info.Report = ix.dims, ix.label, ix.idx.Len(), ix.report
		}
		out = append(out, info)
	}
	return out
}

// VerifyIndex re-reads one index's log from disk and checks it like Verify
// checks the main log. It returns the index's own head: embedding never
// changes the main log's head.
func (l *Ledger) VerifyIndex(name string) ([32]byte, error) {
	for {
		ix, err := l.loaded(name)
		if err != nil {
			return [32]byte{}, err
		}
		ix.mu.RLock()
		log := ix.log
		ix.mu.RUnlock()
		if log == nil {
			continue // dropped or unloaded meanwhile: look it up again
		}
		head, err := log.Verify()
		if err != nil {
			// A DropIndex, Close or failed write may have closed the log
			// under the check. Then its error says nothing about the
			// files: look again, which reports what happened.
			ix.mu.RLock()
			changed := ix.log != log
			ix.mu.RUnlock()
			if changed {
				continue
			}
		}
		return head, err
	}
}

// forgetVectors removes the vectors of things that just died from every
// loaded index, so no search returns them. Their records go on the stale
// list: the bytes stay on disk until Compact or Erase, like the text of an
// Untrusted recall. Indexes not loaded yet need nothing: loading skips
// dead targets. Holders of l.write only.
func (l *Ledger) forgetVectors(target ID, removed []ID) {
	for _, ix := range l.indexes {
		if ix.idx == nil {
			continue
		}
		ix.mu.Lock()
		for _, id := range append([]ID{target}, removed...) {
			if rec, ok := ix.idx.Remove(uint64(id)); ok {
				ix.stale = append(ix.stale, staleRec{rec, id})
			}
		}
		ix.mu.Unlock()
	}
}

// eraseVectors redacts, in every loaded index, the vectors of the erased
// ids. Those are dead, so their vectors are all on the stale lists by now.
// Holders of l.write only.
func (l *Ledger) eraseVectors(ids []ID) error {
	if len(ids) == 0 {
		return nil
	}
	want := make(map[ID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return l.redactStale(func(target ID) bool { return want[target] })
}

// compactVectors redacts every stale vector: those of dead targets and
// those replaced by a newer vector. Holders of l.write only.
func (l *Ledger) compactVectors() error {
	return l.redactStale(func(ID) bool { return true })
}

// redactStale redacts the stale vectors that match, in every loaded index.
// It goes on after a failure, so as much as possible is erased, and
// unloads an index whose redaction failed: what it did erase is known
// again only from the disk.
func (l *Ledger) redactStale(match func(target ID) bool) error {
	var errs []error
	for _, ix := range l.sortedIndexes() {
		if ix.log == nil {
			continue
		}
		var recs []uint64
		var keep []staleRec
		for _, s := range ix.stale {
			if match(s.target) {
				recs = append(recs, s.rec)
			} else {
				keep = append(keep, s)
			}
		}
		if err := ix.log.Redact(recs); err != nil {
			errs = append(errs, fmt.Errorf("index %q: %w", ix.name, errors.Join(err, ix.unload())))
			continue
		}
		ix.stale = keep
	}
	return errors.Join(errs...)
}

// unload closes ix's log and forgets what was loaded from it, so the next
// use reads it again from disk. After a failed write that is the only way
// to know which records reached the disk, and the log refuses further
// writes until it is opened again anyway. Holders of l.write only.
func (ix *vindex) unload() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var err error
	if ix.log != nil {
		err = ix.log.Close()
	}
	ix.log, ix.idx, ix.stale = nil, nil, nil
	return err
}

// closeIndexes closes every loaded index log.
func (l *Ledger) closeIndexes() error {
	var errs []error
	for _, ix := range l.indexes {
		errs = append(errs, ix.unload())
	}
	return errors.Join(errs...)
}
