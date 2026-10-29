package ledger

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/govinargo-labs/cheesedb/internal/logfile"
	"github.com/govinargo-labs/cheesedb/internal/record"
)

func mustOpen(t *testing.T, dir string) *Ledger {
	t.Helper()
	l, _, err := Open(dir, logfile.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

// must unwraps a (value, error) pair. It panics rather than taking t,
// because Go does not allow must(f()) when f returns two values. The
// panic still fails the test and points at the line.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// refExample builds the reference example:
// lots A, B, C; M1 learned from [A] or [B]; M2 from [C]; M3 from [M1 and M2].
type refExample struct{ a, b, c, m1, m2, m3 ID }

func buildRef(t *testing.T, l *Ledger) refExample {
	t.Helper()
	var r refExample
	r.a = must(l.AddLot("user_message", []byte("I live in Pune")))
	r.b = must(l.AddLot("web_page", []byte("profile page: lives in Pune")))
	r.c = must(l.AddLot("weather_tool", []byte("Pune: rain")))
	r.m1 = must(l.Remember([]byte("user lives in Pune"), Leg{r.a}, Leg{r.b}))
	r.m2 = must(l.Remember([]byte("Pune is rainy"), Leg{r.c}))
	r.m3 = must(l.Remember([]byte("carry an umbrella"), Leg{r.m1, r.m2}))
	return r
}

// TestReferenceExample is the reference example, which must be a test.
// Each case also reopens the ledger, to prove replay rebuilds the same
// answer from the log alone.
func TestReferenceExample(t *testing.T) {
	cases := []struct {
		name    string
		recall  func(r refExample) (ID, Mode)
		removed func(r refExample) []ID
	}{
		{"Recall(B, Fair): all survive, M1 still has leg A",
			func(r refExample) (ID, Mode) { return r.b, Fair },
			func(r refExample) []ID { return nil }},
		{"Recall(B, Strict): M1 and M3 removed, M2 survives",
			func(r refExample) (ID, Mode) { return r.b, Strict },
			func(r refExample) []ID { return []ID{r.m1, r.m3} }},
		{"Recall(C, Fair): M2 and M3 removed, M1 survives",
			func(r refExample) (ID, Mode) { return r.c, Fair },
			func(r refExample) []ID { return []ID{r.m2, r.m3} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := mustOpen(t, dir)
			r := buildRef(t, l)
			target, mode := tc.recall(r)
			wantRemoved := tc.removed(r)

			removed := must(l.Recall(target, mode, Untrusted))
			if !slices.Equal(removed, wantRemoved) {
				t.Fatalf("Recall returned %v, want %v", removed, wantRemoved)
			}
			var wantActive []ID
			for _, m := range []ID{r.m1, r.m2, r.m3} {
				if !slices.Contains(wantRemoved, m) {
					wantActive = append(wantActive, m)
				}
			}
			if got := l.Active(); !slices.Equal(got, wantActive) {
				t.Fatalf("Active() = %v, want %v", got, wantActive)
			}
			l.Close()

			l = mustOpen(t, dir)
			defer l.Close()
			if got := l.Active(); !slices.Equal(got, wantActive) {
				t.Fatalf("after reopen, Active() = %v, want %v", got, wantActive)
			}
		})
	}
}

// TestVerify: a ledger in normal use verifies cleanly, before and after a
// recall and a reopen.
func TestVerify(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	r := buildRef(t, l)
	if _, err := l.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	must(l.Recall(r.c, Fair, Erase))
	l.Close()

	l = mustOpen(t, dir)
	defer l.Close()
	if _, err := l.Verify(); err != nil {
		t.Fatalf("Verify after reopen: %v", err)
	}
}

// TestRecallMemoryDirectly: a recalled memory is dead even though its legs
// are fine, and its children follow.
func TestRecallMemoryDirectly(t *testing.T) {
	l := mustOpen(t, t.TempDir())
	defer l.Close()
	r := buildRef(t, l)
	if got := must(l.Recall(r.m1, Fair, Untrusted)); !slices.Equal(got, []ID{r.m1, r.m3}) {
		t.Fatalf("Recall(M1) removed %v, want [M1 M3]", got)
	}
	if got := l.Active(); !slices.Equal(got, []ID{r.m2}) {
		t.Fatalf("Active() = %v, want [M2]", got)
	}
}

// TestFairThenStrict: recalling the same lot again, more strictly, removes
// what the Fair recall left alive.
func TestFairThenStrict(t *testing.T) {
	l := mustOpen(t, t.TempDir())
	defer l.Close()
	r := buildRef(t, l)
	if got := must(l.Recall(r.b, Fair, Untrusted)); got != nil {
		t.Fatalf("Fair removed %v, want nothing", got)
	}
	if got := must(l.Recall(r.b, Strict, Untrusted)); !slices.Equal(got, []ID{r.m1, r.m3}) {
		t.Fatalf("Strict removed %v, want [M1 M3]", got)
	}
}

// TestRememberRejects: every rule is enforced by the engine, and a refused
// Remember writes nothing (the next record still gets the next id).
func TestRememberRejects(t *testing.T) {
	l := mustOpen(t, t.TempDir())
	defer l.Close()
	r := buildRef(t, l)
	rec := ID(7) // the RECALL record below gets id 7
	must(l.Recall(r.c, Fair, Untrusted))

	cases := []struct {
		name string
		legs []Leg
		want error
	}{
		{"no legs", nil, ErrBadLegs},
		{"empty leg", []Leg{{r.a}, {}}, ErrBadLegs},
		{"parent twice in one leg", []Leg{{r.a, r.a}}, ErrBadLegs},
		{"identical legs", []Leg{{r.a, r.b}, {r.b, r.a}}, ErrBadLegs},
		{"unknown id", []Leg{{99}}, ErrUnknownID},
		{"id 0", []Leg{{0}}, ErrUnknownID},
		{"parent is a RECALL record", []Leg{{rec}}, ErrUnknownID},
		{"recalled lot", []Leg{{r.c}}, ErrDeadParent},
		{"dead memory, even next to a live leg", []Leg{{r.a}, {r.m2}}, ErrDeadParent},
	}
	for _, tc := range cases {
		if _, err := l.Remember([]byte("x"), tc.legs...); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if id := must(l.AddLot("user_message", nil)); id != 8 {
		t.Fatalf("next id is %d, want 8: a refused Remember must not write", id)
	}
}

func TestRecallAndAddLotReject(t *testing.T) {
	l := mustOpen(t, t.TempDir())
	defer l.Close()
	r := buildRef(t, l)
	must(l.Recall(r.c, Fair, Untrusted)) // id 7

	for name, err := range map[string]error{
		"unknown id":         second(l.Recall(99, Fair, Untrusted)),
		"id 0":               second(l.Recall(0, Fair, Untrusted)),
		"a RECALL record":    second(l.Recall(7, Fair, Untrusted)),
		"mode 0":             second(l.Recall(r.a, 0, Untrusted)),
		"reason 0":           second(l.Recall(r.a, Fair, 0)),
		"empty source type":  second(l.AddLot("", nil)),
		"invalid UTF-8 type": second(l.AddLot("\xff", nil)),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func second[T any](_ T, err error) error { return err }

// TestGet: content comes back from disk with the right kind and status.
// A recalled memory's content is still there until redaction.
func TestGet(t *testing.T) {
	l := mustOpen(t, t.TempDir())
	defer l.Close()
	r := buildRef(t, l)
	must(l.Recall(r.c, Fair, Untrusted))

	for _, want := range []Item{
		{r.a, record.TypeLot, []byte("I live in Pune"), true, false},
		{r.c, record.TypeLot, []byte("Pune: rain"), false, false},
		{r.m1, record.TypeMemory, []byte("user lives in Pune"), true, false},
		{r.m3, record.TypeMemory, []byte("carry an umbrella"), false, false},
	} {
		if got := must(l.Get(want.ID)); !reflect.DeepEqual(got, want) {
			t.Errorf("Get(%d) = %+v, want %+v", want.ID, got, want)
		}
	}
	if _, err := l.Get(7); !errors.Is(err, ErrUnknownID) {
		t.Errorf("Get(RECALL record) = %v, want ErrUnknownID", err)
	}
}

// TestReplayMatchesLive: after random operations, a reopened ledger has
// exactly the same in-memory state (every flag and counter) as the live one.
func TestReplayMatchesLive(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	rng := rand.New(rand.NewPCG(3, 4))
	for range 200 {
		n := len(l.st.nodes)
		pick := func() ID { return ID(n - rng.IntN(min(n, 10))) }
		switch r := rng.IntN(10); {
		case r < 3 || n == 0:
			must(l.AddLot("tool", []byte("lot")))
		case r < 8:
			l.Remember([]byte("mem"), Leg{pick()}, Leg{pick(), pick()}) // refusals are fine
		default:
			l.Recall(pick(), Mode(1+rng.IntN(2)), Untrusted)
		}
	}
	live := l.st
	l.Close()

	l = mustOpen(t, dir)
	defer l.Close()
	if !reflect.DeepEqual(l.st, live) {
		t.Fatal("replayed state differs from the live state")
	}
}

// TestCorruptHistoryRefused: a log the API could never have written (here,
// a memory built on a recalled lot, or legs that do not decode) is refused
// on Open rather than loaded.
func TestCorruptHistoryRefused(t *testing.T) {
	for name, memoryStructure := range map[string][]byte{
		"memory with a recalled parent": encodeMemory([]Leg{{1}}),
		"memory with no legs":           uv(0),
	} {
		dir := t.TempDir()
		raw, _, err := logfile.Open(dir, logfile.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = raw.AppendBatch([]logfile.Entry{
			{Type: record.TypeLot, Structure: encodeLot("web_page")},
			{Type: record.TypeRecall, Structure: encodeRecall(1, Fair, Untrusted)},
			{Type: record.TypeMemory, Structure: memoryStructure},
		})
		if err != nil {
			t.Fatal(err)
		}
		raw.Close()

		if _, _, err := Open(dir, logfile.Options{}); !errors.Is(err, logfile.ErrCorrupt) {
			t.Errorf("%s: got %v, want ErrCorrupt", name, err)
		}
	}
}

// TestConcurrentUse: writers and readers at once. Writers race to use
// parents that others are recalling, so some Remembers are refused, but the
// write lock means no memory is ever stored on a dead parent: the reopen
// would fail with ErrCorrupt if one were.
func TestConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Go(func() {
			for i := range 12 {
				a, err := l.AddLot("tool", fmt.Appendf(nil, "w%d-%d", w, i))
				if err != nil {
					t.Error(err)
					return
				}
				p := ID(1 + rand.IntN(int(a)))
				_, err = l.Remember([]byte("m"), Leg{a}, Leg{p})
				if err != nil && !errors.Is(err, ErrDeadParent) && !errors.Is(err, ErrUnknownID) && !errors.Is(err, ErrBadLegs) {
					t.Error(err)
				}
				if i%3 == 0 {
					_, err = l.Recall(ID(1+rand.IntN(int(a))), Fair, Untrusted)
					if err != nil && !errors.Is(err, ErrUnknownID) {
						t.Error(err)
					}
				}
			}
		})
	}
	wg.Go(func() { // a reader running alongside
		for range 200 {
			for _, id := range l.Active() {
				l.Get(id)
			}
		}
	})
	wg.Wait()
	live := l.st
	l.Close()

	l = mustOpen(t, dir)
	defer l.Close()
	if !reflect.DeepEqual(l.st, live) {
		t.Fatal("replayed state differs from the live state")
	}
}
