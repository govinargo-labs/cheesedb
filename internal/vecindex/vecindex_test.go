package vecindex

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

func randVec(rng *rand.Rand, dims int) []float32 {
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(rng.NormFloat64())
	}
	n, err := Normalize(v, dims)
	if err != nil {
		panic(err)
	}
	return n
}

// reference is the ten-line definition Search must match: score every
// vector, sort, take k.
func reference(vecs map[uint64][]float32, q []float32, k int) []Hit {
	var all []Hit
	for t, v := range vecs {
		all = append(all, Hit{Target: t, Score: dot(q, v)})
	}
	all = topK(all).sorted()
	return all[:min(k, len(all))]
}

// exactFrom plays the disk for Search: the full vector of each record.
func exactFrom(byRec map[uint64][]float32) Exact {
	return func(rec uint64) ([]float32, error) {
		v, ok := byRec[rec]
		if !ok {
			return nil, fmt.Errorf("no record %d", rec)
		}
		return v, nil
	}
}

func search(t testing.TB, x *Index, q []float32, k int, byRec map[uint64][]float32) []Hit {
	t.Helper()
	hits, err := x.Search(q, k, exactFrom(byRec))
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// checkConsistent checks the row tables agree with each other and with the
// model of what should be stored.
func checkConsistent(t *testing.T, x *Index, model map[uint64][]float32, recs map[uint64]uint64) {
	t.Helper()
	n := len(model)
	if x.Len() != n || len(x.codes) != n*x.dims || len(x.scale) != n || len(x.slack) != n || len(x.rec) != n {
		t.Fatalf("Len %d, codes %d, scale %d, slack %d, rec %d; want %d rows", x.Len(), len(x.codes), len(x.scale), len(x.slack), len(x.rec), n)
	}
	want := make([]int8, x.dims)
	for r, tg := range x.target {
		if x.rowOf(tg) != r {
			t.Fatalf("row[%d] = %d, want %d", tg, x.rowOf(tg), r)
		}
		sc, sl := quantize(want, model[tg])
		if !slices.Equal(x.codes[r*x.dims:(r+1)*x.dims], want) || x.scale[r] != sc || x.slack[r] != sl {
			t.Fatalf("row %d holds the wrong vector for target %d", r, tg)
		}
		if x.rec[r] != recs[tg] {
			t.Fatalf("rec of target %d = %d, want %d", tg, x.rec[r], recs[tg])
		}
	}
	for i, r := range x.row {
		if _, ok := model[uint64(i+1)]; ok != (r >= 0) {
			t.Fatalf("row[%d] = %d, but stored = %v", i+1, r, ok)
		}
	}
}

// TestRandomOperations runs a long random mix of put, replace and remove
// and checks, after every step, that the tables agree and that Search
// equals the reference. Removed targets must never come back.
func TestRandomOperations(t *testing.T) {
	const dims = 8
	rng := rand.New(rand.NewPCG(1, 2))
	x := New(dims)
	model := map[uint64][]float32{}
	recs := map[uint64]uint64{}
	byRec := map[uint64][]float32{}
	removed := map[uint64]bool{}
	var nextRec uint64
	for step := range 3000 {
		target := uint64(rng.IntN(200) + 1)
		if rng.IntN(3) == 0 {
			rec, ok := x.Remove(target)
			if _, had := model[target]; ok != had || (ok && rec != recs[target]) {
				t.Fatalf("step %d: Remove(%d) = %d, %v", step, target, rec, ok)
			}
			delete(model, target)
			delete(recs, target)
			removed[target] = true
		} else {
			nextRec++
			v := randVec(rng, dims)
			old, replaced := x.Put(target, nextRec, v)
			if prev, had := recs[target]; replaced != had || (had && old != prev) {
				t.Fatalf("step %d: Put(%d) = %d, %v", step, target, old, replaced)
			}
			model[target], recs[target], byRec[nextRec] = v, nextRec, v
			delete(removed, target)
		}
		checkConsistent(t, x, model, recs)
		if step%50 == 0 {
			q := randVec(rng, dims)
			got := search(t, x, q, 10, byRec)
			if want := reference(model, q, 10); !slices.Equal(got, want) {
				t.Fatalf("step %d: Search = %v, want %v", step, got, want)
			}
			for _, h := range got {
				if removed[h.Target] {
					t.Fatalf("step %d: removed target %d returned", step, h.Target)
				}
			}
		}
	}
}

// TestParallelSearchMatches makes the index big enough to split the scan
// across cores and checks the answer is exactly the reference's.
func TestParallelSearchMatches(t *testing.T) {
	const dims, n = 64, parallelWork/64 + 1000
	rng := rand.New(rand.NewPCG(3, 4))
	x := New(dims)
	model := map[uint64][]float32{}
	for i := range n {
		v := randVec(rng, dims)
		x.Put(uint64(i+1), uint64(i+1), v)
		model[uint64(i+1)] = v
	}
	for range 5 {
		q := randVec(rng, dims)
		for _, k := range []int{1, 10, 100} {
			if got, want := search(t, x, q, k, model), reference(model, q, k); !slices.Equal(got, want) {
				t.Fatalf("k=%d: Search differs from the reference", k)
			}
		}
	}
}

// TestSlackBound: for any query, the score from the one-byte copy is
// within the row's slack (plus float32 rounding) of the exact score. The
// exactness of Search rests on this.
func TestSlackBound(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for _, dims := range []int{2, 64, 768, MaxDims} {
		codes := make([]int8, dims)
		for range 200 {
			v := randVec(rng, dims)
			if rng.IntN(4) == 0 { // a few vectors with one large value
				v[rng.IntN(dims)] += 5
				v, _ = Normalize(v, dims)
			}
			sc, sl := quantize(codes, v)
			q := randVec(rng, dims)
			got, want := float64(sc*dot8(q, codes)), float64(dot(q, v))
			if math.Abs(got-want) > float64(sl+roundOff(dims)) {
				t.Fatalf("dims %d: estimate %v, exact %v, off by more than slack %v", dims, got, want, sl)
			}
		}
	}
}

// TestNearTies: vectors so close together that the one-byte copies cannot
// tell them apart. Search must then score them exactly and still give the
// reference answer, order and ties included.
func TestNearTies(t *testing.T) {
	const dims, n = 32, 500
	rng := rand.New(rand.NewPCG(9, 10))
	base := randVec(rng, dims)
	x := New(dims)
	model := map[uint64][]float32{}
	for i := range n {
		v := slices.Clone(base)
		v[rng.IntN(dims)] += float32(rng.NormFloat64()) * 1e-4
		if i%7 == 0 {
			v = slices.Clone(base) // exact duplicates: equal scores, ordered by id
		}
		v, _ = Normalize(v, dims)
		x.Put(uint64(i+1), uint64(i+1), v)
		model[uint64(i+1)] = v
	}
	for _, k := range []int{1, 5, 50} {
		q := base
		if got, want := search(t, x, q, k, model), reference(model, q, k); !slices.Equal(got, want) {
			t.Fatalf("k=%d: got %v, want %v", k, got, want)
		}
	}
}

// TestCoarseRanking: in 2 dimensions with thousands of vectors, rounding to
// one byte moves scores by more than the gaps between them, so the
// estimates rank rows wrongly all the time. Search must still equal the
// reference: this is where dropping the slack from either bound fails.
func TestCoarseRanking(t *testing.T) {
	const dims, n = 2, 3000
	rng := rand.New(rand.NewPCG(13, 14))
	x := New(dims)
	model := map[uint64][]float32{}
	for i := range n {
		v := randVec(rng, dims)
		x.Put(uint64(i+1), uint64(i+1), v)
		model[uint64(i+1)] = v
	}
	for range 300 {
		q := randVec(rng, dims)
		for _, k := range []int{1, 3, 20} {
			if got, want := search(t, x, q, k, model), reference(model, q, k); !slices.Equal(got, want) {
				t.Fatalf("k=%d: got %v, want %v", k, got, want)
			}
		}
	}
}

// TestFewExactReads: on ordinary vectors, Search reads only a handful of
// exact vectors, not one per row: the one-byte pass does the work.
func TestFewExactReads(t *testing.T) {
	const dims, n = 768, 20_000
	rng := rand.New(rand.NewPCG(11, 12))
	x := New(dims)
	model := map[uint64][]float32{}
	for i := range n {
		v := randVec(rng, dims)
		x.Put(uint64(i+1), uint64(i+1), v)
		model[uint64(i+1)] = v
	}
	reads := 0
	exact := func(rec uint64) ([]float32, error) { reads++; return model[rec], nil }
	q := randVec(rng, dims)
	got, err := x.Search(q, 10, exact)
	if err != nil || !slices.Equal(got, reference(model, q, 10)) {
		t.Fatalf("Search = %v, %v; want the reference", got, err)
	}
	if reads > 200 {
		t.Fatalf("Search read %d exact vectors out of %d rows; the one-byte pass should leave few", reads, n)
	}
	t.Logf("%d exact reads for %d rows", reads, n)
}

// TestSearchEdges: k larger than the index, k of zero, an empty index, and
// equal scores ordered by id.
func TestSearchEdges(t *testing.T) {
	x := New(2)
	byRec := map[uint64][]float32{1: {1, 0}, 2: {1, 0}, 3: {0, 1}}
	if got := search(t, x, []float32{1, 0}, 5, byRec); got != nil {
		t.Fatalf("empty index: %v", got)
	}
	x.Put(7, 1, []float32{1, 0})
	x.Put(3, 2, []float32{1, 0})
	x.Put(5, 3, []float32{0, 1})
	if got := search(t, x, []float32{1, 0}, 0, byRec); got != nil {
		t.Fatalf("k=0: %v", got)
	}
	want := []Hit{{3, 1}, {7, 1}, {5, 0}}
	if got := search(t, x, []float32{1, 0}, 10, byRec); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// More equal scores than k, arriving highest id first: the lowest ids
	// must win, not the first ones seen.
	x = New(2)
	byRec = map[uint64][]float32{}
	for id := uint64(10); id >= 1; id-- {
		x.Put(id, id, []float32{1, 0})
		byRec[id] = []float32{1, 0}
	}
	want = []Hit{{1, 1}, {2, 1}, {3, 1}}
	if got := search(t, x, []float32{1, 0}, 3, byRec); !slices.Equal(got, want) {
		t.Fatalf("ties: got %v, want %v", got, want)
	}
}

// TestRemoveLeavesNoBytes: after Remove, no trace of the removed vector is
// left in memory past the end of the block. It tries both cases: removing a
// row in the middle (the moved row writes over it) and removing the last
// row (nothing writes over it, so Remove has to wipe it).
func TestRemoveLeavesNoBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 4))
	x := New(8)
	for id := uint64(1); id <= 4; id++ {
		x.Put(id, id, randVec(rng, 8))
	}
	for _, target := range []uint64{4, 2} { // the last row, then a middle one
		x.Remove(target)
		tail := x.codes[len(x.codes):cap(x.codes)]
		if slices.ContainsFunc(tail, func(c int8) bool { return c != 0 }) {
			t.Fatalf("after Remove(%d): spare capacity still holds %v", target, tail)
		}
	}
}

// TestNormalize: bad input is refused loudly; good input comes back with
// length 1 and the caller's slice untouched.
func TestNormalize(t *testing.T) {
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	for name, v := range map[string][]float32{
		"wrong length": {1, 2},
		"NaN":          {1, nan, 0},
		"infinity":     {1, 0, inf},
		"-infinity":    {-inf, 0, 0},
		"all zero":     {0, 0, 0},
	} {
		if _, err := Normalize(v, 3); !errors.Is(err, ErrBadVector) {
			t.Errorf("%s: got %v, want ErrBadVector", name, err)
		}
	}
	in := []float32{3, 4, 0}
	out, err := Normalize(in, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out, []float32{0.6, 0.8, 0}) || !slices.Equal(in, []float32{3, 4, 0}) {
		t.Fatalf("Normalize(%v) = %v", in, out)
	}
	// Large values must not overflow on the way.
	big, err := Normalize([]float32{math.MaxFloat32, math.MaxFloat32}, 2)
	if err != nil || math.Abs(float64(big[0])-math.Sqrt2/2) > 1e-6 {
		t.Fatalf("Normalize(max, max) = %v, %v", big, err)
	}
}

func BenchmarkSearch(b *testing.B) {
	for _, n := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			const dims = 768
			rng := rand.New(rand.NewPCG(5, 6))
			x := New(dims)
			byRec := map[uint64][]float32{}
			for i := range n {
				v := randVec(rng, dims)
				x.Put(uint64(i+1), uint64(i+1), v)
				byRec[uint64(i+1)] = v
			}
			q := randVec(rng, dims)
			b.ReportAllocs()
			for b.Loop() {
				search(b, x, q, 10, byRec)
			}
		})
	}
}
