package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

func mustNew(t *testing.T, typ Type, id uint64, structure, data string) *Record {
	t.Helper()
	r, err := New(typ, id, [32]byte{}, []byte(structure), []byte(data))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func mustEncode(t *testing.T, r *Record) []byte {
	t.Helper()
	buf, err := Encode(r)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf
}

// fixCRC recomputes the crc after a test edits bytes on purpose, so the test
// can reach the checks that run after the crc.
func fixCRC(buf []byte) {
	binary.LittleEndian.PutUint32(buf[offCRC:], checksum(buf))
}

// TestRoundTrip: what we write is exactly what we read back, with and without
// a structural section.
func TestRoundTrip(t *testing.T) {
	for _, typ := range []Type{TypeLot, TypeMemory, TypeRecall} {
		for _, structure := range []string{"", "legs: [1] or [2]"} {
			r := mustNew(t, typ, 42, structure, "user lives in Pune")
			// Non-zero values in every header field, so a field that is
			// silently dropped or swapped shows up.
			r.PrevHash[0] = 0xAB
			r.KeyID = 7
			r.SignerKeyID = 9
			r.Signature[63] = 1

			buf := mustEncode(t, r)
			got, n, err := Decode(buf)
			if err != nil {
				t.Fatalf("type %d: Decode: %v", typ, err)
			}
			if n != len(buf) || n != r.Size() {
				t.Errorf("type %d: Decode used %d bytes, encoded %d, Size() %d", typ, n, len(buf), r.Size())
			}
			if !reflect.DeepEqual(got, r) {
				t.Errorf("type %d: round trip changed the record:\n got %+v\nwant %+v", typ, got, r)
			}
			if string(got.Data()) != "user lives in Pune" {
				t.Errorf("type %d: Data() = %q", typ, got.Data())
			}
		}
	}
}

// TestLayout pins every field to the offset documented in ARCHITECTURE.md,
// using a 4-byte structure "LEGS" and the content "hello" (16-byte salt plus
// 5 bytes of data).
func TestLayout(t *testing.T) {
	r := mustNew(t, TypeLot, 1, "LEGS", "hello")
	buf := mustEncode(t, r)
	le := binary.LittleEndian

	if len(buf) != 183 {
		t.Fatalf("record is %d bytes, want 183 (8 prefix + 150 header + 4 structure + 21 content)", len(buf))
	}
	if got := le.Uint32(buf[0:4]); got != 175 {
		t.Errorf("length = %d, want 175", got)
	}
	if buf[8] != Version {
		t.Errorf("version = %d", buf[8])
	}
	if Type(buf[9]) != TypeLot {
		t.Errorf("type = %d", buf[9])
	}
	if got := le.Uint64(buf[10:18]); got != 1 {
		t.Errorf("id = %d", got)
	}
	if !bytes.Equal(buf[18:50], make([]byte, 32)) {
		t.Error("prevHash should be zero when none is set")
	}
	if !bytes.Equal(buf[50:122], make([]byte, 72)) {
		t.Error("reserved fields (keyID, signerKeyID, signature) should be zero in v1")
	}
	if !bytes.Equal(buf[122:154], r.ContentDigest[:]) {
		t.Error("contentDigest not at offset 122")
	}
	if got := le.Uint32(buf[154:158]); got != 4 {
		t.Errorf("structLen = %d, want 4", got)
	}
	if string(buf[158:162]) != "LEGS" {
		t.Errorf("structure = %q, want \"LEGS\"", buf[158:162])
	}
	if !bytes.Equal(buf[162:178], r.Content[:SaltSize]) {
		t.Error("salt not at offset 162")
	}
	if string(buf[178:]) != "hello" {
		t.Errorf("data = %q, want \"hello\"", buf[178:])
	}
}

// TestSaltIsRandom: the same data gets a different salt, and therefore a
// different digest, every time. That is what stops guessing attacks.
func TestSaltIsRandom(t *testing.T) {
	a := mustNew(t, TypeLot, 1, "", "user lives in Pune")
	b := mustNew(t, TypeLot, 1, "", "user lives in Pune")
	if a.ContentDigest == b.ContentDigest {
		t.Fatal("two records with the same data have the same digest; salt is not random")
	}
}

// TestNewCopiesStructure: a caller reusing its slice after New must not be
// able to change a record that is already built (and maybe already queued).
func TestNewCopiesStructure(t *testing.T) {
	s := []byte("legs")
	r, err := New(TypeMemory, 1, [32]byte{}, s, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	s[0] = 'X'
	if string(r.Structure) != "legs" {
		t.Fatalf("structure changed to %q through the caller's slice", r.Structure)
	}
}

// TestEveryTruncationDetected cuts a record at every possible length, the way
// a crash in the middle of a write could. Every cut must be reported as
// truncated, never misread as a smaller valid record.
func TestEveryTruncationDetected(t *testing.T) {
	buf := mustEncode(t, mustNew(t, TypeMemory, 3, "legs: [1]", "carry an umbrella"))
	for cut := 0; cut < len(buf); cut++ {
		if _, _, err := Decode(buf[:cut]); !errors.Is(err, ErrTruncated) {
			t.Fatalf("cut at %d of %d bytes: got %v, want ErrTruncated", cut, len(buf), err)
		}
	}
}

// TestEveryByteFlipDetected damages each byte in turn. Whatever byte is hit
// (length, crc, header, structure or content), Decode must refuse the record.
func TestEveryByteFlipDetected(t *testing.T) {
	orig := mustEncode(t, mustNew(t, TypeMemory, 3, "legs: [1]", "carry an umbrella"))
	for i := range orig {
		buf := bytes.Clone(orig)
		buf[i] ^= 0xFF
		if _, _, err := Decode(buf); err == nil {
			t.Fatalf("flipping byte %d went unnoticed", i)
		}
	}
}

// TestBadLength: a length field that cannot be real is reported as damage
// instead of being trusted.
func TestBadLength(t *testing.T) {
	for _, n := range []uint32{0, HeaderSize - 1, MaxBodySize + 1, 0xFFFFFFFF} {
		prefix := make([]byte, PrefixSize)
		binary.LittleEndian.PutUint32(prefix, n)
		if _, _, err := Decode(prefix); !errors.Is(err, ErrBadLength) {
			t.Errorf("length %d: got %v, want ErrBadLength", n, err)
		}
	}
}

// TestDigestCatchesRewrittenContent shows the difference between the crc and
// the digest: someone changes the content AND recomputes the crc. The crc is
// now valid, but the digest no longer matches.
func TestDigestCatchesRewrittenContent(t *testing.T) {
	buf := mustEncode(t, mustNew(t, TypeMemory, 1, "", "user lives in Pune"))
	copy(buf[len(buf)-4:], "Goa!")
	fixCRC(buf)
	if _, _, err := Decode(buf); !errors.Is(err, ErrDigest) {
		t.Fatalf("got %v, want ErrDigest", err)
	}
}

// TestInvalidFields: bytes that are intact (good crc) but mean something not
// allowed are still refused.
func TestInvalidFields(t *testing.T) {
	good := mustEncode(t, mustNew(t, TypeLot, 1, "", "hello"))
	le := binary.LittleEndian

	cases := map[string]func([]byte) []byte{
		"unknown version": func(b []byte) []byte { b[offVersion] = 2; return b },
		"type 0":          func(b []byte) []byte { b[offType] = 0; return b },
		"type 4":          func(b []byte) []byte { b[offType] = 4; return b },
		"content shorter than salt": func(b []byte) []byte {
			b = append(b[:offStructure], 1, 2, 3, 4, 5)
			le.PutUint32(b[offLength:], HeaderSize+5)
			return b
		},
		"structLen one past the body": func(b []byte) []byte {
			le.PutUint32(b[offStructLen:], uint32(len(b)-offStructure+1))
			return b
		},
		"structLen huge": func(b []byte) []byte {
			le.PutUint32(b[offStructLen:], 0xFFFFFFFF)
			return b
		},
	}
	for name, edit := range cases {
		buf := edit(bytes.Clone(good))
		fixCRC(buf)
		if _, _, err := Decode(buf); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

// TestRedactionKeepsStructure: a redacted record (empty content) keeps its
// header, digest AND structural section, and reads back as redacted. This is
// the point of the split: erasing what a memory says must not erase where it
// came from.
func TestRedactionKeepsStructure(t *testing.T) {
	r := mustNew(t, TypeMemory, 5, "legs: [1] or [2]", "user lives in Pune")
	digest := r.ContentDigest
	r.Content = nil // what redaction does

	buf := mustEncode(t, r)
	if want := PrefixSize + HeaderSize + len("legs: [1] or [2]"); len(buf) != want {
		t.Fatalf("redacted record is %d bytes, want %d", len(buf), want)
	}
	got, _, err := Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !got.Redacted() || got.Data() != nil {
		t.Error("record should read back as redacted with no data")
	}
	if string(got.Structure) != "legs: [1] or [2]" {
		t.Errorf("structure = %q; redaction must keep it", got.Structure)
	}
	if got.ContentDigest != digest {
		t.Error("redaction must keep the digest, or the chain hash would break")
	}
}

// TestContentSizeBoundaries walks the edges of both decode rules, with a
// structural section present: structLen may reach exactly the end of the body
// (content empty, i.e. redacted), and content must be empty or at least the
// 16-byte salt.
func TestContentSizeBoundaries(t *testing.T) {
	full := mustEncode(t, mustNew(t, TypeMemory, 1, "LEGS", "hello"))
	offContent := offStructure + 4

	for _, k := range []int{0, 1, 15} {
		buf := bytes.Clone(full[:offContent+k])
		binary.LittleEndian.PutUint32(buf[offLength:], uint32(len(buf)-PrefixSize))
		fixCRC(buf)
		r, _, err := Decode(buf)
		switch {
		case k == 0 && (err != nil || !r.Redacted()):
			t.Errorf("content of 0 bytes (structLen == length - 150): got %v, want a redacted record", err)
		case k > 0 && !errors.Is(err, ErrInvalid):
			t.Errorf("content of %d bytes: got %v, want ErrInvalid", k, err)
		}
	}

	// Exactly the salt and no data is the smallest live record.
	saltOnly := mustEncode(t, mustNew(t, TypeRecall, 1, "LEGS", ""))
	if r, _, err := Decode(saltOnly); err != nil || len(r.Content) != SaltSize || r.Redacted() {
		t.Errorf("content of exactly %d bytes: got %v, want a live record", SaltSize, err)
	}
}

// TestDigestExcludesStructure: contentDigest is SHA-256(salt || data) and
// nothing else. The structure can change without the digest noticing, which
// is exactly why the chain hash must cover the structure itself.
func TestDigestExcludesStructure(t *testing.T) {
	r := mustNew(t, TypeMemory, 1, "legs: [1]", "user lives in Pune")
	if r.ContentDigest != sha256.Sum256(r.Content) {
		t.Fatal("contentDigest is not SHA-256 of the content section")
	}
	r.Structure = []byte("legs: [3]")
	if _, err := Encode(r); err != nil {
		t.Fatalf("changing the structure should not affect the digest check, got %v", err)
	}
}

// TestChainHashCatchesEditedParents is the attack the chain hash exists for:
// someone rewrites a memory's parents on disk and recomputes the crc. Decode
// accepts it (the crc matches, the digest only covers content), but the
// chain hash changes, so Verify will see that the next record's
// prevHash no longer matches.
func TestChainHashCatchesEditedParents(t *testing.T) {
	orig := mustNew(t, TypeMemory, 4, "legs: [1] or [2]", "carry an umbrella")
	buf := mustEncode(t, orig)

	i := offStructure + bytes.Index(orig.Structure, []byte("2"))
	buf[i] = '3' // leg [2] becomes [3]
	fixCRC(buf)

	forged, _, err := Decode(buf)
	if err != nil {
		t.Fatalf("Decode refused the forgery (%v); this test expects it to pass so the chain hash is what catches it", err)
	}
	if forged.ChainHash() == orig.ChainHash() {
		t.Fatal("editing a memory's parents did not change its chain hash")
	}
}

// TestChainHashCoverage: changing any covered field changes the hash;
// redacting the content or setting the signature does not.
func TestChainHashCoverage(t *testing.T) {
	base := mustNew(t, TypeMemory, 4, "legs: [1]", "hello")
	want := base.ChainHash()

	covered := map[string]func(*Record){
		"version":       func(r *Record) { r.Version++ },
		"type":          func(r *Record) { r.Type = TypeLot },
		"id":            func(r *Record) { r.ID++ },
		"prevHash":      func(r *Record) { r.PrevHash[31] ^= 1 },
		"keyID":         func(r *Record) { r.KeyID++ },
		"signerKeyID":   func(r *Record) { r.SignerKeyID++ },
		"contentDigest": func(r *Record) { r.ContentDigest[0] ^= 1 },
		"structure":     func(r *Record) { r.Structure[len(r.Structure)-2] = '2' },
		"structLen":     func(r *Record) { r.Structure = append(r.Structure, 0) },
	}
	for name, edit := range covered {
		r := *base
		r.Structure = bytes.Clone(base.Structure)
		edit(&r)
		if r.ChainHash() == want {
			t.Errorf("changing %s did not change the chain hash", name)
		}
	}

	ignored := map[string]func(*Record){
		"redaction": func(r *Record) { r.Content = nil },
		"signature": func(r *Record) { r.Signature[0] = 1 },
	}
	for name, edit := range ignored {
		r := *base
		edit(&r)
		if r.ChainHash() != want {
			t.Errorf("%s changed the chain hash; it must not", name)
		}
	}
}

// TestEncodeRefusesBadRecords: invariants are enforced on the way in too, so
// a record that could not be read back is never written.
func TestEncodeRefusesBadRecords(t *testing.T) {
	if _, err := New(TypeLot, 1, [32]byte{}, nil, make([]byte, MaxBodySize)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized data: got %v, want ErrTooLarge", err)
	}
	// The structural section counts towards the limit too.
	half := make([]byte, MaxBodySize/2)
	if _, err := New(TypeLot, 1, [32]byte{}, half, half); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized structure + data: got %v, want ErrTooLarge", err)
	}

	r := mustNew(t, TypeLot, 1, "", "hello")
	r.Content[SaltSize] = 'j' // data changed after the digest was computed
	if _, err := Encode(r); !errors.Is(err, ErrDigest) {
		t.Errorf("stale digest: got %v, want ErrDigest", err)
	}

	r = mustNew(t, TypeLot, 1, "", "hello")
	r.Content = r.Content[:5]
	if _, err := Encode(r); !errors.Is(err, ErrInvalid) {
		t.Errorf("content shorter than salt: got %v, want ErrInvalid", err)
	}

	r = mustNew(t, TypeLot, 1, "", "hello")
	r.Type = 9
	if _, err := Encode(r); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown type: got %v, want ErrInvalid", err)
	}
}
