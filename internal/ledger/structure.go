package ledger

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

// This file encodes the structural section of each record type: the part
// that redaction never removes, because replay needs it to rebuild the
// family tree. All integers are uvarints (small numbers take 1 byte).
//
//	LOT     source type text (1-255 bytes, UTF-8)
//	MEMORY  legCount, then per leg: parentCount, parent ids (sorted)
//	RECALL  target id, mode (1 byte), reason (1 byte)
//
// Decoding is strict: anything the encoder would never produce is refused,
// so every structure has exactly one valid encoding.

// errMalformed is wrapped by every decoding error here. Replay turns it into
// logfile.ErrCorrupt.
var errMalformed = errors.New("malformed structure")

const maxSourceType = 255

func checkSourceType(s string) error {
	if len(s) == 0 || len(s) > maxSourceType || !utf8.ValidString(s) {
		return fmt.Errorf("%w: source type must be 1-%d bytes of UTF-8, got %q", ErrBadArgument, maxSourceType, s)
	}
	return nil
}

func encodeLot(sourceType string) []byte { return []byte(sourceType) }

func decodeLot(b []byte) (string, error) {
	s := string(b)
	if err := checkSourceType(s); err != nil {
		return "", fmt.Errorf("%w: %v", errMalformed, err)
	}
	return s, nil
}

// canonicalLegs checks the caller's legs and returns a copy with each leg's
// parents sorted. A leg needs ALL its parents, so their order means nothing;
// sorting gives one way to write each leg and makes duplicates sit next to
// each other.
func canonicalLegs(legs []Leg) ([]Leg, error) {
	if len(legs) == 0 {
		return nil, fmt.Errorf("%w: a memory needs at least one leg", ErrBadLegs)
	}
	out := make([]Leg, len(legs))
	for i, leg := range legs {
		if len(leg) == 0 {
			return nil, fmt.Errorf("%w: leg %d has no parents", ErrBadLegs, i)
		}
		leg = slices.Clone(leg)
		slices.Sort(leg)
		for j := 1; j < len(leg); j++ {
			if leg[j] == leg[j-1] {
				return nil, fmt.Errorf("%w: leg %d lists parent %d twice", ErrBadLegs, i, leg[j])
			}
		}
		for k := range i {
			if slices.Equal(out[k], leg) {
				return nil, fmt.Errorf("%w: legs %d and %d are identical", ErrBadLegs, k, i)
			}
		}
		out[i] = leg
	}
	return out, nil
}

// encodeMemory writes legs that canonicalLegs has already checked.
func encodeMemory(legs []Leg) []byte {
	b := binary.AppendUvarint(nil, uint64(len(legs)))
	for _, leg := range legs {
		b = binary.AppendUvarint(b, uint64(len(leg)))
		for _, p := range leg {
			b = binary.AppendUvarint(b, uint64(p))
		}
	}
	return b
}

// decodeMemory reads the legs of memory self. Besides the shape, it checks
// what the bytes alone can prove: every parent is a real id below self.
// Whether those parents exist and are alive is the state's job.
func decodeMemory(b []byte, self ID) ([]Leg, error) {
	d := decoder{b: b}
	nLegs := d.count()
	if d.err == nil && nLegs == 0 {
		return nil, fmt.Errorf("%w: memory with no legs", errMalformed)
	}
	legs := make([]Leg, 0, nLegs)
	for range nLegs {
		nParents := d.count()
		if d.err == nil && nParents == 0 {
			return nil, fmt.Errorf("%w: empty leg", errMalformed)
		}
		leg := make(Leg, 0, nParents)
		for range nParents {
			p := ID(d.uvarint())
			if d.err != nil {
				break
			}
			if p == 0 || p >= self {
				return nil, fmt.Errorf("%w: parent %d of memory %d must be between 1 and %d", errMalformed, p, self, self-1)
			}
			if len(leg) > 0 && p <= leg[len(leg)-1] {
				return nil, fmt.Errorf("%w: parents not sorted or repeated", errMalformed)
			}
			leg = append(leg, p)
		}
		for _, prev := range legs {
			if d.err == nil && slices.Equal(prev, leg) {
				return nil, fmt.Errorf("%w: identical legs", errMalformed)
			}
		}
		legs = append(legs, leg)
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return legs, nil
}

func encodeRecall(target ID, m Mode, r Reason) []byte {
	b := binary.AppendUvarint(nil, uint64(target))
	return append(b, byte(m), byte(r))
}

func decodeRecall(b []byte, self ID) (ID, Mode, Reason, error) {
	d := decoder{b: b}
	target := ID(d.uvarint())
	m := Mode(d.byte())
	r := Reason(d.byte())
	if err := d.finish(); err != nil {
		return 0, 0, 0, err
	}
	if target == 0 || target >= self {
		return 0, 0, 0, fmt.Errorf("%w: recall %d targets id %d", errMalformed, self, target)
	}
	if !m.valid() || !r.valid() {
		return 0, 0, 0, fmt.Errorf("%w: mode %d, reason %d", errMalformed, m, r)
	}
	return target, m, r, nil
}

// decoder reads uvarints and bytes from b and remembers the first error, so
// the decode functions above read like a list of fields instead of an error
// check after every one.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = fmt.Errorf("%w: bad uvarint", errMalformed)
		return 0
	}
	d.b = d.b[n:]
	return v
}

// count reads a number of items that follow. Each item takes at least one
// byte, so a count larger than the bytes left is damage. Checking it before
// allocating stops a damaged count from asking for gigabytes.
func (d *decoder) count() int {
	n := d.uvarint()
	if d.err == nil && n > uint64(len(d.b)) {
		d.err = fmt.Errorf("%w: count %d with only %d bytes left", errMalformed, n, len(d.b))
	}
	if d.err != nil {
		return 0
	}
	return int(n)
}

func (d *decoder) byte() byte {
	if d.err != nil {
		return 0
	}
	if len(d.b) == 0 {
		d.err = fmt.Errorf("%w: ended early", errMalformed)
		return 0
	}
	c := d.b[0]
	d.b = d.b[1:]
	return c
}

// finish reports the first error, or trailing bytes nobody asked for.
func (d *decoder) finish() error {
	if d.err == nil && len(d.b) > 0 {
		d.err = fmt.Errorf("%w: %d trailing bytes", errMalformed, len(d.b))
	}
	return d.err
}
