// Package vecindex keeps vectors in memory and finds the ones closest to a
// search. Every search looks at every vector, so it never misses a match
// (this is called "exact search").
//
// To save memory, each number in a vector is kept as one byte instead of
// four (int8 instead of float32, known as "scalar quantization"). That makes
// the index four times smaller, but the numbers are slightly rounded. The
// full, precise vectors stay on disk.
//
// So a search runs in two rounds. First it gives every vector a quick score
// from its rounded copy. Then it reads the precise vector from disk for each
// one that could still make the top results, and scores those properly.
// Every vector remembers how far off its rounded copy can be, so a vector is
// only skipped when it could not make the top results even in the best case.
// The answer is the same as if every precise vector had been checked.
//
// This package never reads or writes files and never takes locks, just like
// the ledger's state. The ledger decides what goes in and when, and only
// after the data is safely on disk.
//
// Every vector is scaled to length 1 before it is stored (see Normalize).
// That way, how alike two vectors are (their "cosine similarity") is simply
// their dot product: multiply the matching numbers and add them up.
//
// Removing a vector really removes it. The last vector moves into the empty
// spot ("swap-remove") and the leftover bytes are wiped. Nothing is just
// marked as deleted, so a recalled memory's vector cannot show up in a
// search, and its bytes are gone from the index. One gap: when the block
// grows, Go copies it into a bigger one, and the old copy is not wiped; it
// stays in the process's memory until the garbage collector reuses it.
// See ARCHITECTURE.md section 9.
package vecindex

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"
)

// MaxDims is the most numbers a vector may have. Common embedding models use
// far fewer, and the limit stops one bad request from filling memory and disk.
const MaxDims = 8192

// parallelWork is how much work a search must have (vectors × numbers per
// vector) before it is split across CPU cores. That is about 1 ms of work,
// far more than the cost of starting the extra goroutines. It is a first
// guess; measure to settle it.
const parallelWork = 1 << 20

// ErrBadVector means a vector cannot be used: it has the wrong number of
// values, contains NaN or infinity, or is all zeros (so it points nowhere).
var ErrBadVector = errors.New("vecindex: bad vector")

// Hit is one search result: which lot or memory matched, and how close it is
// to the search, from -1 (the opposite) to 1 (the same direction).
type Hit struct {
	Target uint64
	Score  float32
}

// Normalize checks v and returns a copy scaled to length 1. Doing this once,
// before the vector is stored, makes every later comparison a simple dot
// product. Replaying the log never has to redo it, so replay always gives
// the same result.
func Normalize(v []float32, dims int) ([]float32, error) {
	if len(v) != dims {
		return nil, fmt.Errorf("%w: %d values, the index holds %d", ErrBadVector, len(v), dims)
	}
	// Add up in float64, because squaring big float32 numbers can overflow.
	// Check every value too: one NaN would quietly break every ranking.
	var sum float64
	for i, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%w: value %d is %v", ErrBadVector, i, x)
		}
		sum += f * f
	}
	if sum == 0 {
		return nil, fmt.Errorf("%w: every value is zero", ErrBadVector)
	}
	scale := 1 / math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) * scale)
	}
	return out, nil
}

// Index holds one vector for each target (a lot or memory id).
type Index struct {
	dims   int
	codes  []int8    // all rounded vectors in one block; row r is codes[r*dims:(r+1)*dims]
	scale  []float32 // per row: the size of one rounding step (vector ≈ scale × codes)
	slack  []float32 // per row: the most the rounded copy can be off by, for any search
	target []uint64  // per row: the lot or memory the vector belongs to
	rec    []uint64  // per row: the VECTOR record holding the precise vector on disk
	row    []int32   // row[target-1] is the target's row, or -1 if it has no vector
}

// Exact reads the precise float32 vector stored in VECTOR record rec on disk.
// Search calls it only for the few vectors it must score precisely.
type Exact func(rec uint64) ([]float32, error)

// New returns an empty index for vectors with dims numbers each.
func New(dims int) *Index { return &Index{dims: dims} }

// Dims returns how many numbers each vector has.
func (x *Index) Dims() int { return x.dims }

// Len returns how many targets have a vector.
func (x *Index) Len() int { return len(x.target) }

// rowOf returns the row holding target's vector, or -1 if there is none.
func (x *Index) rowOf(target uint64) int {
	if target == 0 || target > uint64(len(x.row)) {
		return -1
	}
	return int(x.row[target-1])
}

// Put stores vec as target's vector; record rec holds it on disk. vec must
// come from Normalize. If target already has a vector, the new one replaces
// it and Put returns the record that held the old one, so the caller can
// erase it later.
func (x *Index) Put(target, rec uint64, vec []float32) (old uint64, replaced bool) {
	if len(vec) != x.dims {
		panic(fmt.Sprintf("vecindex: Put with %d values, want %d", len(vec), x.dims))
	}
	if r := x.rowOf(target); r >= 0 {
		x.scale[r], x.slack[r] = quantize(x.codes[r*x.dims:(r+1)*x.dims], vec)
		old, x.rec[r] = x.rec[r], rec
		return old, true
	}
	for uint64(len(x.row)) < target {
		x.row = append(x.row, -1)
	}
	r := len(x.target)
	x.row[target-1] = int32(r)
	x.codes = slices.Grow(x.codes, x.dims)[:(r+1)*x.dims]
	sc, sl := quantize(x.codes[r*x.dims:], vec)
	x.scale = append(x.scale, sc)
	x.slack = append(x.slack, sl)
	x.target = append(x.target, target)
	x.rec = append(x.rec, rec)
	return 0, false
}

// quantize rounds vec to one byte per number, writes the bytes into dst, and
// returns the step size and the error bound.
//
// The step is the biggest number (ignoring its sign) divided by 127, so each
// number becomes a whole count of steps from -127 to 127, and
// value ≈ step × code. For example:
//
//	vec:   [0.6, -0.8]
//	step:  0.8 / 127 ≈ 0.0063
//	 0.6 / 0.0063 ≈   95.2 → stored as   95 (read back: 95 × 0.0063 ≈ 0.598)
//	-0.8 / 0.0063 =  -127  → stored as -127 (read back: -0.8, exact)
//
// So 8 bytes become 2. Rounding leaves a small error in each number (0.002
// above). For a search vector of length 1, the score can be off by at most
// the length of all those errors taken together (the Cauchy-Schwarz
// inequality, |q · e| ≤ |q| |e|).
func quantize(dst []int8, vec []float32) (scale, slack float32) {
	var maxAbs float64
	for _, v := range vec {
		maxAbs = max(maxAbs, math.Abs(float64(v)))
	}
	step := maxAbs / 127
	if step == 0 {
		step = 1 // an all-zero vector; Normalize refuses these, but stay safe
	}
	var sum float64
	for i, v := range vec {
		c := math.Round(float64(v) / step)
		dst[i] = int8(max(-127, min(127, c)))
		e := float64(v) - float64(dst[i])*step
		sum += e * e
	}
	// Round up, so turning it into a float32 never makes the bound too small.
	return float32(step), float32(math.Nextafter(math.Sqrt(sum), math.Inf(1)))
}

// Remove deletes target's vector and returns the record that held it on
// disk. If target has no vector, it does nothing.
//
// The last row moves into the empty spot, so the rows stay packed together
// and a search never has to step over a hole.
func (x *Index) Remove(target uint64) (rec uint64, ok bool) {
	r := x.rowOf(target)
	if r < 0 {
		return 0, false
	}
	rec = x.rec[r]
	last := len(x.target) - 1
	if r != last {
		copy(x.codes[r*x.dims:(r+1)*x.dims], x.codes[last*x.dims:])
		x.scale[r], x.slack[r] = x.scale[last], x.slack[last]
		x.target[r] = x.target[last]
		x.rec[r] = x.rec[last]
		x.row[x.target[r]-1] = int32(r)
	}
	// Making a slice shorter does not erase anything: the old bytes stay in
	// memory past the new end. If the removed vector was the last row, those
	// bytes are that vector, so wipe them now instead of waiting for a later
	// Put to write over them.
	clear(x.codes[last*x.dims:])
	x.codes = x.codes[:last*x.dims]
	x.scale, x.slack = x.scale[:last], x.slack[:last]
	x.target = x.target[:last]
	x.rec = x.rec[:last]
	x.row[target-1] = -1
	return rec, true
}

// Search returns the k targets whose vectors are closest to q, best first.
// q must come from Normalize. Ties are ordered by target id, so the same
// index and search always give the same answer.
//
// It works in two rounds:
//
//  1. Give every row a quick score from its rounded copy. Taking away the
//     row's slack gives a score the row is certain to reach. The k-th best
//     of these certain scores becomes the bar.
//  2. For every row whose quick score plus slack can still reach the bar,
//     read the precise vector (through exact) and score it properly. Any
//     other row cannot be in the top k, so it is skipped.
//
// The slack also covers the tiny rounding errors of float32 math in both
// rounds (see roundOff).
func (x *Index) Search(q []float32, k int, exact Exact) ([]Hit, error) {
	n := len(x.target)
	if k <= 0 || n == 0 {
		return nil, nil
	}
	parts := 1
	if n*x.dims >= parallelWork {
		parts = min(runtime.GOMAXPROCS(0), n)
	}
	est := make([]float32, n)
	sure := make([]topK, parts)
	var wg sync.WaitGroup
	for p := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sure[p] = x.estimate(q, k, est, p*n/parts, (p+1)*n/parts)
		}()
	}
	wg.Wait()
	var all topK
	for _, t := range sure {
		all = append(all, t...)
	}
	all = all.sorted()
	bar := float32(math.Inf(-1))
	if len(all) >= k {
		bar = all[k-1].Score
	}
	round := roundOff(x.dims)
	var best topK
	for r, e := range est {
		if e+x.slack[r]+round < bar {
			continue
		}
		v, err := exact(x.rec[r])
		if err != nil {
			return nil, err
		}
		if len(v) != x.dims {
			return nil, fmt.Errorf("%w: record %d holds %d values, the index holds %d", ErrBadVector, x.rec[r], len(v), x.dims)
		}
		best = best.offer(Hit{Target: x.target[r], Score: dot(q, v)}, k)
	}
	return best.sorted(), nil
}

// estimate gives each row from "from" up to (not including) "to" a quick
// score from its rounded copy and stores it in est. It returns the k best
// scores those rows are certain to reach.
func (x *Index) estimate(q []float32, k int, est []float32, from, to int) topK {
	t := make(topK, 0, min(k, to-from))
	round := roundOff(x.dims)
	for r := from; r < to; r++ {
		est[r] = x.scale[r] * dot8(q, x.codes[r*x.dims:(r+1)*x.dims])
		t = t.offer(Hit{Target: x.target[r], Score: est[r] - x.slack[r] - round}, k)
	}
	return t
}

// roundOff is the most that float32 rounding can shift a sum of dims
// products. For two vectors of length 1, the products add up to at most 1 in
// size (Cauchy-Schwarz again), and adding n float32 numbers can be off by
// about n × 2^-24 of that. The quick score and the precise score both round,
// so the bound is doubled, plus a little extra to be safe.
func roundOff(dims int) float32 { return float32(dims) * 3 / (1 << 24) }

func dot8(q []float32, c []int8) float32 {
	c = c[:len(q)] // check the length once here, so Go skips the check inside the loop
	var s float32
	for i, v := range q {
		s += v * float32(c[i])
	}
	return s
}

func dot(a, b []float32) float32 {
	b = b[:len(a)] // check the length once here, so Go skips the check inside the loop
	var s float32
	for i, v := range a {
		s += v * b[i]
	}
	return s
}

// better decides the result order: higher score first, then lower target id.
func better(a, b Hit) bool {
	return a.Score > b.Score || (a.Score == b.Score && a.Target < b.Target)
}

// topK holds the best k hits seen so far, arranged as a min-heap: the worst
// of them sits at the top, so a new hit only has to beat that one to get in.
// Each offer takes O(log k) steps, however many rows there are.
type topK []Hit

func (t topK) offer(h Hit, k int) topK {
	if len(t) < k {
		t = append(t, h)
		for i := len(t) - 1; i > 0; { // sift up: move the new hit up to its place
			p := (i - 1) / 2
			if !better(t[p], t[i]) {
				break
			}
			t[p], t[i] = t[i], t[p]
			i = p
		}
		return t
	}
	if !better(h, t[0]) {
		return t
	}
	t[0] = h
	for i := 0; ; { // sift down: move the new hit down to its place
		l, r, worst := 2*i+1, 2*i+2, i
		if l < len(t) && better(t[worst], t[l]) {
			worst = l
		}
		if r < len(t) && better(t[worst], t[r]) {
			worst = r
		}
		if worst == i {
			return t
		}
		t[i], t[worst] = t[worst], t[i]
		i = worst
	}
}

func (t topK) sorted() []Hit {
	slices.SortFunc(t, func(a, b Hit) int {
		if better(a, b) {
			return -1
		}
		if better(b, a) {
			return 1
		}
		return 0
	})
	return t
}
