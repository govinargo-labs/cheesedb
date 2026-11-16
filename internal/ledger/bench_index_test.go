package ledger

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// BenchmarkIndex measures a search index of 768 values per memory, from
// 1,000 to 1M memories: cheesedb's own cost, with no embedding model. The
// vectors are random, which costs exactly what real ones cost: a search
// compares every value of every vector, whatever they are. Building the 1M
// dataset takes about a minute and 3.4 GB of temporary disk, so it runs
// only when asked, with a fixed count:
//
//	CHEESEDB_BENCH_INDEX=1 go test -run '^$' -bench Index -benchtime=3x -count=3 -timeout 90m ./internal/ledger/
//
// "search" has the index loaded, like a Go program that keeps the store
// open. The "open+" cases open the ledger first, like each cheesedb command
// and MCP tool call: a search or an erase then loads the whole index.
// Recalls take a new lot each time, so they change the dataset slightly.
func BenchmarkIndex(b *testing.B) {
	if os.Getenv("CHEESEDB_BENCH_INDEX") == "" {
		b.Skip("slow: set CHEESEDB_BENCH_INDEX=1 and -benchtime=3x (see the comment)")
	}
	for _, n := range append([]int{1000}, benchSizes...) {
		b.Run(sizeName(n), func(b *testing.B) {
			dir, mems := indexDataset(b, n)
			q := randomUnit(rand.New(rand.NewPCG(9, 9)))
			lot := ID(10*(n/20) + 1) // every 10th id from 1 is a lot; start in the middle
			nextLot := func() ID { lot += 10; return lot }

			b.Run("store-vectors", func(b *testing.B) {
				l := openBench(b, dir)
				defer l.Close()
				for i := 0; b.Loop(); i++ {
					name := fmt.Sprintf("copy%d", i)
					if err := l.CreateIndex(name, benchDims, "random"); err != nil {
						b.Fatal(err)
					}
					storeVectors(b, l, name, mems)
					b.StopTimer()
					if err := l.DropIndex(name); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
			})
			b.Run("search", func(b *testing.B) {
				l := openBench(b, dir)
				defer l.Close()
				if _, err := l.Search("default", q, 10); err != nil { // load the index
					b.Fatal(err)
				}
				var m runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&m)
				for b.Loop() {
					if _, err := l.Search("default", q, 10); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(m.HeapAlloc)/(1<<20), "heap-MB")
				b.ReportMetric(float64(dirBytes(b, filepath.Join(dir, indexFolder("default"))))/float64(len(mems)), "index-B/memory")
			})
			b.Run("open+search", func(b *testing.B) {
				for b.Loop() {
					l := openBench(b, dir)
					if _, err := l.Search("default", q, 10); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					l.Close()
					b.StartTimer()
				}
			})
			b.Run("recall-erase", func(b *testing.B) {
				l := openBench(b, dir)
				defer l.Close()
				if _, err := l.Search("default", q, 10); err != nil {
					b.Fatal(err)
				}
				for b.Loop() {
					if _, err := l.Recall(nextLot(), Fair, Erase); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("open+recall-erase", func(b *testing.B) {
				for b.Loop() {
					l := openBench(b, dir)
					if _, err := l.Recall(nextLot(), Fair, Erase); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					l.Close()
					b.StartTimer()
				}
			})
			b.Run("open+recall-untrusted", func(b *testing.B) {
				for b.Loop() {
					l := openBench(b, dir)
					if _, err := l.Recall(nextLot(), Fair, Untrusted); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					l.Close()
					b.StartTimer()
				}
			})
		})
	}
}

const benchDims = 768

// indexDatasets caches one folder per size holding the benchgen graph and
// an index "default" with a vector for every memory.
var indexDatasets = map[int]struct {
	dir  string
	mems []ID
}{}

func indexDataset(b *testing.B, n int) (string, []ID) {
	if d, ok := indexDatasets[n]; ok {
		return d.dir, d.mems
	}
	dir := benchDir(b, "index-"+sizeName(n))
	writeDataset(b, dir, n)
	l := openBench(b, dir)
	defer l.Close()
	mems := l.Active()
	if err := l.CreateIndex("default", benchDims, "random"); err != nil {
		b.Fatal(err)
	}
	storeVectors(b, l, "default", mems)
	indexDatasets[n] = struct {
		dir  string
		mems []ID
	}{dir, mems}
	return dir, mems
}

// storeVectors gives every memory a random vector, in batches like
// EmbedAll's, made as they are written so 1M vectors never sit in memory.
func storeVectors(b *testing.B, l *Ledger, index string, mems []ID) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < len(mems); i += 300 {
		var items []Embedding
		for _, id := range mems[i:min(i+300, len(mems))] {
			items = append(items, Embedding{id, randomUnit(rng)})
		}
		if err := l.EmbedBatch(index, items); err != nil {
			b.Fatal(err)
		}
	}
}

func randomUnit(rng *rand.Rand) []float32 {
	v := make([]float32, benchDims)
	var sum float64
	for i := range v {
		v[i] = float32(rng.NormFloat64())
		sum += float64(v[i]) * float64(v[i])
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(sum))
	}
	return v
}

func dirBytes(b *testing.B, dir string) int64 {
	var total int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	return total
}
