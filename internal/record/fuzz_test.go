package record

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// FuzzDecode feeds Decode arbitrary bytes. Decode reads files that may be
// damaged or hand-edited, so the one thing it must never do is panic, and
// whatever it accepts must be exactly what Encode would write back.
//
// Run it for a while with:
//
//	go test -fuzz=FuzzDecode -fuzztime=60s ./internal/record
//
// A plain `go test` runs only the seeds below (and any failing inputs saved
// under testdata/fuzz), so the test is quick and runs in CI.
func FuzzDecode(f *testing.F) {
	for _, seed := range decodeSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		checkDecode(t, b)

		// The crc covers every byte, so a mutated input almost always stops at
		// ErrChecksum and the checks after it (structLen, version, type,
		// digest) would never run. Fix the length and the crc as well, so the
		// fuzzer also reaches those checks.
		if len(b) >= PrefixSize {
			sealed := bytes.Clone(b)
			binary.LittleEndian.PutUint32(sealed[offLength:], uint32(len(sealed)-PrefixSize))
			fixCRC(sealed)
			checkDecode(t, sealed)
		}
	})
}

// checkDecode states what must hold for any input.
func checkDecode(t *testing.T, b []byte) {
	t.Helper()
	r, n, err := Decode(b)
	if err != nil {
		if r != nil || n != 0 {
			t.Fatalf("Decode failed (%v) but returned a record or a length: %v, %d", err, r, n)
		}
		// Callers tell damage from a torn tail by these errors, so every
		// failure must be one of them.
		for _, known := range []error{ErrTruncated, ErrBadLength, ErrChecksum, ErrDigest, ErrInvalid} {
			if errors.Is(err, known) {
				return
			}
		}
		t.Fatalf("Decode returned an error outside the documented set: %v", err)
	}

	if n < PrefixSize+HeaderSize || n > len(b) {
		t.Fatalf("Decode used %d bytes of %d", n, len(b))
	}
	if bl, err := BodyLen(b); err != nil || PrefixSize+bl != n {
		t.Fatalf("BodyLen = %d, %v, but Decode used %d bytes", bl, err, n)
	}

	// Anything Decode accepts must encode back to the very same bytes: no
	// field is lost, changed or reinterpreted along the way.
	enc, err := Encode(r)
	if err != nil {
		t.Fatalf("Encode refused a record that Decode accepted: %v", err)
	}
	if !bytes.Equal(enc, b[:n]) {
		t.Fatalf("re-encoding changed the bytes:\n got %x\nwant %x", enc, b[:n])
	}
	again, n2, err := Decode(enc)
	if err != nil || n2 != n || !reflect.DeepEqual(again, r) {
		t.Fatalf("decoding the re-encoded bytes gave %+v, %d, %v; want %+v, %d", again, n2, err, r, n)
	}
}

// decodeSeeds returns valid records of every type, with and without a
// structural section, one redacted, one with bytes after it, and a few
// damaged ones. The fuzzer starts from these and mutates them.
func decodeSeeds(f *testing.F) [][]byte {
	f.Helper()
	var seeds [][]byte
	encode := func(r *Record) []byte {
		buf, err := Encode(r)
		if err != nil {
			f.Fatalf("Encode: %v", err)
		}
		return buf
	}
	for _, typ := range []Type{TypeLot, TypeMemory, TypeRecall} {
		for _, structure := range []string{"", "legs: [1] or [2]"} {
			r, err := New(typ, 42, [32]byte{0xAB}, []byte(structure), []byte("user lives in Pune"))
			if err != nil {
				f.Fatalf("New: %v", err)
			}
			seeds = append(seeds, encode(r))
		}
	}

	r, err := New(TypeMemory, 7, [32]byte{}, []byte("legs: [1]"), []byte("carry an umbrella"))
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	valid := encode(r)

	redacted := *r
	redacted.Content = nil // what redaction does: the digest stays, the content goes
	seeds = append(seeds, encode(&redacted))

	seeds = append(seeds, append(bytes.Clone(valid), "trailing bytes of the next record"...))
	seeds = append(seeds, valid[:len(valid)/2])              // torn
	seeds = append(seeds, nil, []byte{0}, make([]byte, 200)) // nothing, tiny, all zeros
	flipped := bytes.Clone(valid)
	flipped[len(flipped)-1] ^= 0xFF
	seeds = append(seeds, flipped)
	return seeds
}
