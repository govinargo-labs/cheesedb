// Package record converts one log record to bytes and back.
//
// It knows nothing about files: the logfile package decides where the bytes
// go. Keeping encoding separate means every byte of the format can be tested
// without touching the disk.
//
// On-disk layout (integers little-endian; see ARCHITECTURE.md section 4):
//
//	offset size field
//	0      4    length         bytes in the body (everything after crc)
//	4      4    crc32          CRC-32C over length + body
//	--- fixed header (kept by redaction) ---
//	8      1    version
//	9      1    type
//	10     8    id
//	18     32   prevHash       zeros for record 1
//	50     4    keyID          reserved, 0 in v1
//	54     4    signerKeyID    reserved, 0 in v1
//	58     64   signature      reserved, zeros in v1
//	122    32   contentDigest  SHA-256(content)
//	154    4    structLen      size of the structural section
//	--- structural section (kept by redaction) ---
//	158    s    structure      legs, recall target, ...; meaning set per record type
//	--- content section (removed by redaction) ---
//	158+s  n    content        16-byte salt + data; empty means redacted
package record

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	// Version is the format version written into every record. A future
	// layout change bumps it, so old files still say how to read them.
	Version = 1

	PrefixSize = 8   // length + crc: what you must read before anything else
	HeaderSize = 150 // fixed part of the body, before the structural section
	SaltSize   = 16

	// MaxBodySize caps one record. Without a cap, a damaged length field
	// could claim gigabytes and make us allocate them. It also bounds how
	// large a torn tail can be (see the logfile package).
	MaxBodySize = 1 << 20
)

// Byte offsets of each field, counted from the start of the record.
const (
	offLength      = 0
	offCRC         = 4
	offVersion     = 8
	offType        = 9
	offID          = 10
	offPrevHash    = 18
	offKeyID       = 50
	offSignerKeyID = 54
	offSignature   = 58
	offDigest      = 122
	offStructLen   = 154 // added last, so every earlier offset stayed put
	offStructure   = 158 // = PrefixSize + HeaderSize
)

// Type says what a record means.
type Type uint8

const (
	TypeLot    Type = 1
	TypeMemory Type = 2
	TypeRecall Type = 3
)

func (t Type) valid() bool { return t >= TypeLot && t <= TypeRecall }

var (
	// ErrTruncated means the bytes end before the record does. At the end of
	// a log, this is what a crash in the middle of a write looks like.
	ErrTruncated = errors.New("record: truncated")

	// ErrBadLength means the length field is impossible (shorter than the
	// fixed header or longer than MaxBodySize), so it must be damaged.
	ErrBadLength = errors.New("record: bad length")

	// ErrChecksum means the crc does not match: some byte changed by accident.
	ErrChecksum = errors.New("record: checksum mismatch")

	// ErrDigest means the crc is fine but the content does not match its
	// digest, i.e. the content was rewritten along with a fresh crc. Editing
	// the digest too is caught later by the chain hash.
	ErrDigest = errors.New("record: content digest mismatch")

	// ErrInvalid means the bytes are intact but describe something not
	// allowed: unknown version or type, a structLen that runs past the body,
	// or content shorter than a salt.
	ErrInvalid = errors.New("record: invalid")

	// ErrTooLarge means the caller tried to write more than MaxBodySize.
	ErrTooLarge = errors.New("record: too large")
)

// CRC-32C (Castagnoli) detects more error patterns than the classic IEEE
// polynomial and is computed in hardware on modern CPUs.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Record is one entry of the log, decoded into Go values.
//
// Structure and Content are split so that redaction can erase what a record
// says (Content) without erasing where it came from (Structure): replay still
// rebuilds the family tree from a redacted memory.
type Record struct {
	Version       uint8
	Type          Type
	ID            uint64
	PrevHash      [32]byte
	KeyID         uint32
	SignerKeyID   uint32
	Signature     [64]byte
	ContentDigest [32]byte
	Structure     []byte // what replay needs; never redacted
	Content       []byte // salt + data, or empty if redacted
}

// New builds a record with the given structural section and data. A fresh
// random salt goes in front of the data. The salt is generated here, at write
// time, and stored in the content, so replaying the log never needs
// randomness (replay must be deterministic).
func New(t Type, id uint64, prevHash [32]byte, structure, data []byte) (*Record, error) {
	content := make([]byte, SaltSize+len(data))
	if _, err := rand.Read(content[:SaltSize]); err != nil {
		return nil, fmt.Errorf("record: generating salt: %w", err)
	}
	copy(content[SaltSize:], data)

	r := &Record{
		Version:       Version,
		Type:          t,
		ID:            id,
		PrevHash:      prevHash,
		ContentDigest: sha256.Sum256(content),
		// Copy so a caller reusing its slice cannot change the record later.
		// An empty structure becomes nil, the same as Decode returns.
		Structure: append([]byte(nil), structure...),
		Content:   content,
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	return r, nil
}

// Size returns how many bytes the record takes on disk.
func (r *Record) Size() int {
	return PrefixSize + HeaderSize + len(r.Structure) + len(r.Content)
}

// ChainHash returns the hash that links this record into the chain: the next
// record stores it as its prevHash.
//
// It covers, in on-disk order: version, type, id, prevHash, keyID,
// signerKeyID, contentDigest, structLen and the structural section. So
// editing a memory's parents changes it, even though the crc can be
// recomputed and the content digest never looks at the structure.
//
// It skips length, crc and content, which change when a record is redacted,
// so redaction does not break the chain. It skips the signature because v2
// signs this hash, and a signature cannot sign itself.
func (r *Record) ChainHash() [32]byte {
	le := binary.LittleEndian
	b := make([]byte, 0, 86+len(r.Structure))
	b = append(b, r.Version, byte(r.Type))
	b = le.AppendUint64(b, r.ID)
	b = append(b, r.PrevHash[:]...)
	b = le.AppendUint32(b, r.KeyID)
	b = le.AppendUint32(b, r.SignerKeyID)
	b = append(b, r.ContentDigest[:]...)
	// The length goes in before the bytes, so ("ab" + "c") and ("a" + "bc")
	// can never hash the same way.
	b = le.AppendUint32(b, uint32(len(r.Structure)))
	b = append(b, r.Structure...)
	return sha256.Sum256(b)
}

// Redacted reports whether the content has been erased. Live content always
// holds at least the salt, so empty content can only mean redacted. That is
// why there is no separate "redacted" flag that could contradict the content.
func (r *Record) Redacted() bool { return len(r.Content) == 0 }

// Data returns the caller's bytes (the content without its salt), or nil if
// the record is redacted.
func (r *Record) Data() []byte {
	if r.Redacted() {
		return nil
	}
	return r.Content[SaltSize:]
}

// check enforces the rules shared by New, Encode and Decode, so a record that
// could not be read back is never written.
func (r *Record) check() error {
	if r.Version != Version {
		return fmt.Errorf("%w: version %d", ErrInvalid, r.Version)
	}
	if !r.Type.valid() {
		return fmt.Errorf("%w: type %d", ErrInvalid, r.Type)
	}
	if body := r.Size() - PrefixSize; body > MaxBodySize {
		return fmt.Errorf("%w: body of %d bytes, max %d", ErrTooLarge, body, MaxBodySize)
	}
	if r.Redacted() {
		return nil // nothing left to compare the digest against
	}
	if len(r.Content) < SaltSize {
		return fmt.Errorf("%w: content of %d bytes is shorter than the salt", ErrInvalid, len(r.Content))
	}
	if sha256.Sum256(r.Content) != r.ContentDigest {
		return ErrDigest
	}
	return nil
}

// Encode returns the record's on-disk bytes: prefix, fixed header, structural
// section, content section.
func Encode(r *Record) ([]byte, error) {
	if err := r.check(); err != nil {
		return nil, err
	}

	buf := make([]byte, r.Size())
	offContent := offStructure + len(r.Structure)
	le := binary.LittleEndian
	le.PutUint32(buf[offLength:], uint32(len(buf)-PrefixSize))
	buf[offVersion] = r.Version
	buf[offType] = byte(r.Type)
	le.PutUint64(buf[offID:], r.ID)
	copy(buf[offPrevHash:], r.PrevHash[:])
	le.PutUint32(buf[offKeyID:], r.KeyID)
	le.PutUint32(buf[offSignerKeyID:], r.SignerKeyID)
	copy(buf[offSignature:], r.Signature[:])
	copy(buf[offDigest:], r.ContentDigest[:])
	le.PutUint32(buf[offStructLen:], uint32(len(r.Structure)))
	copy(buf[offStructure:], r.Structure)
	copy(buf[offContent:], r.Content)

	// The crc goes in last because it covers every other byte.
	le.PutUint32(buf[offCRC:], checksum(buf))
	return buf, nil
}

// checksum covers the length field and the body, skipping the crc field
// itself: a checksum cannot include its own value.
func checksum(rec []byte) uint32 {
	c := crc32.Update(0, castagnoli, rec[offLength:offCRC])
	return crc32.Update(c, castagnoli, rec[PrefixSize:])
}

// BodyLen reads the length field from the first PrefixSize bytes of a record
// and checks that it is possible. A log reader uses it to learn how many more
// bytes to read.
func BodyLen(prefix []byte) (int, error) {
	if len(prefix) < PrefixSize {
		return 0, ErrTruncated
	}
	n := int(binary.LittleEndian.Uint32(prefix[offLength:]))
	if n < HeaderSize || n > MaxBodySize {
		return 0, fmt.Errorf("%w: %d", ErrBadLength, n)
	}
	return n, nil
}

// Decode parses one record from the start of b and returns it together with
// the number of bytes it used. b may hold more data after the record.
//
// Checks run from the most basic to the most specific: are there enough
// bytes, is the length possible, does the crc match, and only then do the
// fields mean something valid.
func Decode(b []byte) (*Record, int, error) {
	bodyLen, err := BodyLen(b)
	if err != nil {
		return nil, 0, err
	}
	size := PrefixSize + bodyLen
	if len(b) < size {
		return nil, 0, ErrTruncated
	}
	rec := b[:size]

	le := binary.LittleEndian
	if checksum(rec) != le.Uint32(rec[offCRC:]) {
		return nil, 0, ErrChecksum
	}

	// The crc passed, so structLen is what the writer wrote. It can still be
	// impossible if the writer was buggy or someone forged a matching crc.
	// Compare as uint64 so a huge value cannot overflow int on 32-bit systems.
	structLen := le.Uint32(rec[offStructLen:])
	if uint64(structLen) > uint64(size-offStructure) {
		return nil, 0, fmt.Errorf("%w: structLen %d runs past the body", ErrInvalid, structLen)
	}
	offContent := offStructure + int(structLen)

	r := &Record{
		Version:     rec[offVersion],
		Type:        Type(rec[offType]),
		ID:          le.Uint64(rec[offID:]),
		KeyID:       le.Uint32(rec[offKeyID:]),
		SignerKeyID: le.Uint32(rec[offSignerKeyID:]),
		// Copy rather than slice, so the record does not keep the caller's
		// (possibly large) read buffer alive. Empty sections become nil.
		Structure: append([]byte(nil), rec[offStructure:offContent]...),
		Content:   append([]byte(nil), rec[offContent:]...),
	}
	copy(r.PrevHash[:], rec[offPrevHash:])
	copy(r.Signature[:], rec[offSignature:])
	copy(r.ContentDigest[:], rec[offDigest:])

	if err := r.check(); err != nil {
		return nil, 0, err
	}
	return r, size, nil
}
