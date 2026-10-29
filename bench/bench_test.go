package bench

import (
	"math/rand/v2"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/benchgen"
)

// The same measurements as internal/ledger/bench_test.go, on SQLite and
// PostgreSQL. Run from this directory with:
//
//	go test -run '^$' -bench . -benchmem -timeout 60m
//
// Go runs benchmarks in source order; the read-only ones come first because
// Remember and Recall add rows to the shared datasets.

var engineNames = []string{"sqlite", "postgres"}

// BenchmarkStartup opens a fresh connection and reads one memory: the SQL
// counterpart of cheesedb's Open, which must replay its log first. It also
// reports disk use per memory.
func BenchmarkStartup(b *testing.B) {
	for _, name := range engineNames {
		for _, size := range sizes {
			b.Run(name+"/"+sizeName(size), func(b *testing.B) {
				e := getEngine(b, name, size)
				disk := e.diskBytes(b)
				b.ReportAllocs()
				for b.Loop() {
					db, err := e.open()
					if err != nil {
						b.Fatal(err)
					}
					if err := e.get(db, 2); err != nil {
						b.Fatal(err)
					}
					db.Close()
				}
				b.ReportMetric(float64(disk)/float64(size), "disk-B/memory")
			})
		}
	}
}

func BenchmarkGet(b *testing.B) {
	for _, name := range engineNames {
		for _, size := range sizes {
			b.Run(name+"/"+sizeName(size), func(b *testing.B) {
				e := getEngine(b, name, size)
				n := int(e.nextID - 1)
				rng := rand.New(rand.NewPCG(1, 1))
				b.ReportAllocs()
				for b.Loop() {
					id := uint64(1 + rng.IntN(n))
					for !benchgen.IsMemory(id) {
						id++
					}
					if err := e.get(e.db, id); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkRemember(b *testing.B) {
	for _, name := range engineNames {
		for _, size := range sizes {
			b.Run(name+"/"+sizeName(size), func(b *testing.B) {
				e := getEngine(b, name, size)
				if err := e.bulk([]benchgen.Op{{Lot: true}}); err != nil {
					b.Fatal(err)
				}
				lot := e.nextID - 1
				b.ReportAllocs()
				for b.Loop() {
					if _, err := e.remember([][]uint64{{lot}}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkRecall is a durable Fair recall of a lot that 20 memories depend
// on; each iteration first adds a fresh probe, untimed.
func BenchmarkRecall(b *testing.B) {
	for _, name := range engineNames {
		for _, size := range sizes {
			b.Run(name+"/"+sizeName(size), func(b *testing.B) { recallProbe(b, name, size, 10) })
		}
	}
}

// BenchmarkRecallLarge removes 10,000 memories in one recall, at 1M.
func BenchmarkRecallLarge(b *testing.B) {
	for _, name := range engineNames {
		b.Run(name+"/1M", func(b *testing.B) { recallProbe(b, name, 1_000_000, 5000) })
	}
}

func recallProbe(b *testing.B, name string, size, width int) {
	e := getEngine(b, name, size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := e.nextID
		if err := e.bulk(benchgen.Probe(p, width)); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		removed, err := e.recallFair(p)
		if err != nil || removed != 2*width {
			b.Fatalf("removed %d, err %v; want %d", removed, err, 2*width)
		}
	}
}
