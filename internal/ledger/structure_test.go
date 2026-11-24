package ledger

import (
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// uv builds raw structural bytes from numbers, so tests can write things
// the encoder never would.
func uv(nums ...uint64) []byte {
	var b []byte
	for _, n := range nums {
		b = binary.AppendUvarint(b, n)
	}
	return b
}

func TestStructureRoundTrip(t *testing.T) {
	if s, err := decodeLot(encodeLot("web_page")); err != nil || s != "web_page" {
		t.Errorf("lot: got %q, %v", s, err)
	}

	legs := []Leg{{1}, {2, 300}, {5, 7, 1_000_000}}
	if got, err := decodeMemory(encodeMemory(legs), 2_000_000); err != nil || !reflect.DeepEqual(got, legs) {
		t.Errorf("memory: got %v, %v", got, err)
	}

	target, m, r, err := decodeRecall(encodeRecall(42, Strict, Erase), 50)
	if err != nil || target != 42 || m != Strict || r != Erase {
		t.Errorf("recall: got %d %d %d, %v", target, m, r, err)
	}

	tag := [tagSize]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if got, gotTag, err := decodeVector(encodeVector(1_000_000, tag)); err != nil || got != 1_000_000 || gotTag != tag {
		t.Errorf("vector: got %d %x, %v", got, gotTag, err)
	}
}

// TestCanonicalLegs: parents are sorted inside each leg, and the duplicate
// rules are enforced, whatever order the caller used.
func TestCanonicalLegs(t *testing.T) {
	got, err := canonicalLegs([]Leg{{5, 2}, {3}})
	if err != nil || !reflect.DeepEqual(got, []Leg{{2, 5}, {3}}) {
		t.Fatalf("got %v, %v; want [[2 5] [3]]", got, err)
	}

	for name, legs := range map[string][]Leg{
		"no legs":                         nil,
		"empty leg":                       {{1}, {}},
		"parent twice in a leg":           {{1, 2, 1}},
		"identical legs":                  {{1, 2}, {3}, {1, 2}},
		"identical legs in another order": {{1, 2}, {2, 1}},
	} {
		if _, err := canonicalLegs(legs); !errors.Is(err, ErrBadLegs) {
			t.Errorf("%s: got %v, want ErrBadLegs", name, err)
		}
	}
}

// TestManyLegsAreQuick: finding identical legs must not compare every leg
// with every other one. 200,000 legs used to take about 15 s to write and
// as long again to replay; any token holder could send that in one request.
// The time limit is far above what the one-pass check needs, even under
// -race, and far below what the old check took.
func TestManyLegsAreQuick(t *testing.T) {
	const n = 200_000
	const first = 1000 // the one-parent legs start here, above the ids below
	legs := make([]Leg, n)
	for i := range legs {
		legs[i] = Leg{ID(first + i)}
	}
	// Legs that are not equal but whose parents run together must not be
	// mistaken for each other: [1 2] is not [1] and [2], and 300 is two bytes.
	legs[0], legs[1], legs[2], legs[3], legs[4] = Leg{1, 2}, Leg{1}, Leg{2}, Leg{1, 300}, Leg{300}

	start := time.Now()
	canon, err := canonicalLegs(legs)
	if err != nil {
		t.Fatalf("canonicalLegs: %v", err)
	}
	got, err := decodeMemory(encodeMemory(canon), first+n)
	if err != nil {
		t.Fatalf("decodeMemory: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("%d legs took %v to check, encode and decode", n, took)
	}
	if !reflect.DeepEqual(got, canon) {
		t.Fatal("decoded legs differ from the encoded ones")
	}

	// A copy of the very first leg, at the very end, is still caught.
	legs = append(legs, Leg{2, 1})
	if _, err := canonicalLegs(legs); !errors.Is(err, ErrBadLegs) || !strings.Contains(err.Error(), fmt.Sprintf("legs 0 and %d", n)) {
		t.Errorf("identical first and last leg: got %v, want ErrBadLegs naming legs 0 and %d", err, n)
	}
}

// TestDecodeRejectsMalformed: every shape the encoder cannot produce is
// refused, so a damaged or forged structure never reaches the state.
func TestDecodeRejectsMalformed(t *testing.T) {
	const self = 10
	memory := map[string][]byte{
		"empty":                 nil,
		"zero legs":             uv(0),
		"empty leg":             uv(1, 0),
		"parent 0":              uv(1, 1, 0),
		"parent = own id":       uv(1, 1, self),
		"parent above own id":   uv(1, 1, self+5),
		"parents unsorted":      uv(1, 2, 5, 3),
		"parent repeated":       uv(1, 2, 3, 3),
		"identical legs":        uv(2, 1, 4, 1, 4),
		"trailing byte":         append(uv(1, 1, 4), 0),
		"count past the end":    uv(1_000_000),
		"cut inside a leg":      uv(1, 3, 2, 4),
		"truncated uvarint":     {0x80},
		"legs count claims two": uv(2, 1, 4),
	}
	for name, b := range memory {
		if _, err := decodeMemory(b, self); !errors.Is(err, errMalformed) {
			t.Errorf("memory %s: got %v, want errMalformed", name, err)
		}
	}

	recall := map[string][]byte{
		"empty":           nil,
		"target 0":        append(uv(0), byte(Fair), byte(Untrusted)),
		"target = own id": append(uv(self), byte(Fair), byte(Untrusted)),
		"mode 0":          append(uv(3), 0, byte(Untrusted)),
		"reason 9":        append(uv(3), byte(Fair), 9),
		"no reason":       append(uv(3), byte(Fair)),
		"trailing byte":   append(uv(3), byte(Fair), byte(Untrusted), 0),
	}
	for name, b := range recall {
		if _, _, _, err := decodeRecall(b, self); !errors.Is(err, errMalformed) {
			t.Errorf("recall %s: got %v, want errMalformed", name, err)
		}
	}

	tag := make([]byte, tagSize)
	vector := map[string][]byte{
		"empty":             nil,
		"target 0":          append(uv(0), tag...),
		"no tag":            uv(3),
		"short tag":         append(uv(3), tag[1:]...),
		"trailing byte":     append(append(uv(3), tag...), 0),
		"truncated uvarint": {0x80},
	}
	for name, b := range vector {
		if _, _, err := decodeVector(b); !errors.Is(err, errMalformed) {
			t.Errorf("vector %s: got %v, want errMalformed", name, err)
		}
	}

	for _, s := range []string{"", strings.Repeat("x", 256), "\xff"} {
		if _, err := decodeLot([]byte(s)); !errors.Is(err, errMalformed) {
			t.Errorf("lot %q: got %v, want errMalformed", s, err)
		}
	}
}
