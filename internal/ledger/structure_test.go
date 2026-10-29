package ledger

import (
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
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

	for _, s := range []string{"", strings.Repeat("x", 256), "\xff"} {
		if _, err := decodeLot([]byte(s)); !errors.Is(err, errMalformed) {
			t.Errorf("lot %q: got %v, want errMalformed", s, err)
		}
	}
}
