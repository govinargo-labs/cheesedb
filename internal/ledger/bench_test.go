package ledger

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/benchgen"
	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

// Benchmarks. Run with:
//
//	go test -run '^$' -bench . -benchmem ./internal/ledger/
//
// Every benchmark uses the shared workload from benchgen, which the /bench
// module also uses for SQLite and PostgreSQL. Results go in BENCHMARKS.md.
//
// Go runs benchmarks in source order. Open and Get come first because
// Remember and Recall append to the shared datasets.

var benchSizes = []int{10_000, 100_000, 1_000_000}

func sizeName(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%dM", n/1_000_000)
	}
	return fmt.Sprintf("%dk", n/1000)
}

// datasets caches one ledger directory per size for the whole run, since
// building the 1M one takes a few seconds.
var datasets struct {
	sync.Mutex
	root string
	dirs map[int]string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if datasets.root != "" {
		os.RemoveAll(datasets.root)
	}
	os.Exit(code)
}

// dataset returns a directory holding a ledger of size memories. It is
// written straight through the log in batches of up to MaxBatchBytes (one
// fsync each), because writing a million memories one Remember at a time,
// each with its own fsync, would take over an hour. The files are the same
// as Remember would produce.
func dataset(b *testing.B, size int) string {
	datasets.Lock()
	defer datasets.Unlock()
	if dir, ok := datasets.dirs[size]; ok {
		return dir
	}
	if datasets.root == "" {
		root, err := os.MkdirTemp("", "cheesedb-bench-")
		if err != nil {
			b.Fatal(err)
		}
		datasets.root, datasets.dirs = root, map[int]string{}
	}
	dir := filepath.Join(datasets.root, sizeName(size))
	raw, _, err := logfile.Open(dir, logfile.Options{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	var batch []logfile.Entry
	bytes := 0
	flush := func() {
		if len(batch) > 0 {
			if _, err := raw.AppendBatch(batch); err != nil {
				b.Fatal(err)
			}
		}
		batch, bytes = batch[:0], 0
	}
	for _, op := range benchgen.Dataset(size) {
		e := logfile.Entry{Type: record.TypeLot, Structure: encodeLot(benchgen.SourceType), Data: benchgen.Content}
		if !op.Lot {
			e = logfile.Entry{Type: record.TypeMemory, Structure: encodeMemory(toLegs(op.Legs)), Data: benchgen.Content}
		}
		n := record.PrefixSize + record.HeaderSize + len(e.Structure) + record.SaltSize + len(e.Data)
		if bytes+n > logfile.MaxBatchBytes {
			flush()
		}
		batch, bytes = append(batch, e), bytes+n
	}
	flush()
	if err := raw.Close(); err != nil {
		b.Fatal(err)
	}
	datasets.dirs[size] = dir
	return dir
}

func toLegs(legs [][]uint64) []Leg {
	out := make([]Leg, len(legs))
	for i, l := range legs {
		out[i] = make(Leg, len(l))
		for j, p := range l {
			out[i][j] = ID(p)
		}
	}
	return out
}

func openBench(b *testing.B, dir string) *Ledger {
	b.Helper()
	l, _, err := Open(dir, logfile.Options{})
	if err != nil {
		b.Fatal(err)
	}
	return l
}

// BenchmarkOpen measures startup: replaying the whole log to rebuild the
// state. It also reports RAM and disk per memory, since it has a whole
// ledger loaded anyway.
func BenchmarkOpen(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(sizeName(size), func(b *testing.B) {
			dir := dataset(b, size)
			heap := heapPerMemory(b, dir, size)
			disk := diskBytes(b, dir)
			b.ReportAllocs()
			for b.Loop() {
				openBench(b, dir).Close()
			}
			b.ReportMetric(heap, "heap-B/memory")
			b.ReportMetric(float64(disk)/float64(size), "disk-B/memory")
		})
	}
}

// heapPerMemory measures how much Go heap an open ledger keeps: the state
// (family tree, counters) and the offset table. Lots and RECALLs are
// included, spread over the memories.
func heapPerMemory(b *testing.B, dir string, size int) float64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	l := openBench(b, dir)
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(l)
	l.Close()
	return float64(after.HeapAlloc-before.HeapAlloc) / float64(size)
}

func diskBytes(b *testing.B, dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			b.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

// BenchmarkGet reads a random memory by id: offset table lookup, one pread,
// crc check.
func BenchmarkGet(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(sizeName(size), func(b *testing.B) {
			l := openBench(b, dataset(b, size))
			defer l.Close()
			n := len(l.st.nodes)
			rng := rand.New(rand.NewPCG(1, 1))
			b.ReportAllocs()
			for b.Loop() {
				id := ID(1 + rng.IntN(n))
				for !benchgen.IsMemory(uint64(id)) {
					id++
				}
				if _, err := l.Get(id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRemember stores one memory with one leg: check the parent, write,
// fsync, update the state.
func BenchmarkRemember(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(sizeName(size), func(b *testing.B) {
			l := openBench(b, dataset(b, size))
			defer l.Close()
			lot := must(l.AddLot(benchgen.SourceType, benchgen.Content))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := l.Remember(benchgen.Content, Leg{lot}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRecall is a full, durable Recall (write the RECALL record, fsync,
// walk) of a lot that 20 memories depend on. Each iteration first adds a
// fresh probe through the API, with the timer stopped.
func BenchmarkRecall(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(sizeName(size), func(b *testing.B) {
			l := openBench(b, dataset(b, size))
			defer l.Close()
			b.ReportAllocs()
			b.ResetTimer() // b.Loop does this itself; a classic loop must, or Open is counted
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				p := addProbe(l, 10)
				b.StartTimer()
				removed, err := l.Recall(p, Fair, Untrusted)
				if err != nil || len(removed) != 20 {
					b.Fatalf("removed %d, err %v; want 20", len(removed), err)
				}
			}
		})
	}
}

func addProbe(l *Ledger, width int) ID {
	first := ID(len(l.st.nodes)) + 1 // single goroutine: safe to read
	for _, op := range benchgen.Probe(uint64(first), width) {
		if op.Lot {
			must(l.AddLot(benchgen.SourceType, benchgen.Content))
		} else {
			must(l.Remember(benchgen.Content, toLegs(op.Legs)...))
		}
	}
	return first
}

// BenchmarkRecallCascade measures only the in-memory walk of a recall (no
// disk), for a small cascade (20 memories) and a large one (10,000). If
// recall cost depends only on what it reaches, the small case takes the
// same time at every ledger size. A full Recall costs this plus one durable
// write (see BenchmarkRecall).
func BenchmarkRecallCascade(b *testing.B) {
	for _, size := range benchSizes {
		for _, width := range []int{10, 5000} {
			b.Run(fmt.Sprintf("%s/removes-%d", sizeName(size), 2*width), func(b *testing.B) {
				l := openBench(b, dataset(b, size))
				s := l.st // a copy of the state; the ledger itself is not written
				l.Close()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					p := addProbeToState(b, &s, width)
					b.StartTimer()
					removed, err := s.recall(ID(len(s.nodes))+1, p, Fair)
					if err != nil || len(removed) != 2*width {
						b.Fatalf("removed %d, err %v; want %d", len(removed), err, 2*width)
					}
				}
			})
		}
	}
}

func addProbeToState(b *testing.B, s *state, width int) ID {
	first := ID(len(s.nodes)) + 1
	for i, op := range benchgen.Probe(uint64(first), width) {
		id := first + ID(i)
		var err error
		if op.Lot {
			err = s.addLot(id)
		} else {
			err = s.addMemory(id, toLegs(op.Legs))
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	return first
}
