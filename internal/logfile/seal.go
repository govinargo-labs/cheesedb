package logfile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
)

// A seal is a small file written next to a segment when it is sealed
// (000001.seal for 000001.seg). It records what the segment holds, so a
// sealed segment can be checked on its own: its first id, its last id, and
// the chain hash of its last record.
//
// It is a separate file rather than a footer inside the segment, so that
// segments stay "records only" and replay and the torn-tail rule do not
// need to know about it.
//
// Layout (little-endian), 53 bytes:
//
//	0   1   version   currently 1
//	1   8   first id
//	9   8   last id
//	17  32  chain hash of the last record
//	49  4   CRC-32C over bytes 0-48
type seal struct {
	first, last uint64
	hash        [32]byte
}

const (
	sealVersion = 1
	sealSize    = 53
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func sealName(n int) string { return fmt.Sprintf("%06d.seal", n) }

func encodeSeal(s seal) []byte {
	b := make([]byte, sealSize)
	b[0] = sealVersion
	binary.LittleEndian.PutUint64(b[1:], s.first)
	binary.LittleEndian.PutUint64(b[9:], s.last)
	copy(b[17:], s.hash[:])
	binary.LittleEndian.PutUint32(b[49:], crc32.Checksum(b[:49], castagnoli))
	return b
}

// readSeal reads and checks the seal of segment n. A missing or damaged
// seal is corruption: seals are written atomically (see writeSeal), so a
// crash leaves either the whole file or none.
func readSeal(dir string, n int) (seal, error) {
	b, err := os.ReadFile(filepath.Join(dir, sealName(n)))
	if errors.Is(err, fs.ErrNotExist) {
		return seal{}, fmt.Errorf("%w: sealed segment %d has no seal", ErrCorrupt, n)
	}
	if err != nil {
		return seal{}, err
	}
	if len(b) != sealSize || b[0] != sealVersion ||
		crc32.Checksum(b[:49], castagnoli) != binary.LittleEndian.Uint32(b[49:]) {
		return seal{}, fmt.Errorf("%w: seal of segment %d is damaged", ErrCorrupt, n)
	}
	s := seal{first: binary.LittleEndian.Uint64(b[1:]), last: binary.LittleEndian.Uint64(b[9:])}
	copy(s.hash[:], b[17:49])
	return s, nil
}

// checkSeal compares segment n's seal with what reading the segment found.
func checkSeal(dir string, n int, got seal) error {
	want, err := readSeal(dir, n)
	if err != nil {
		return err
	}
	if want != got {
		return fmt.Errorf("%w: segment %d does not match its seal (seal says ids %d-%d, segment holds %d-%d, or the last hash differs)",
			ErrCorrupt, n, want.first, want.last, got.first, got.last)
	}
	return nil
}

// writeSeal writes segment n's seal atomically: a temporary file, fsync,
// rename over the final name, fsync the directory. A rename is atomic, so a
// crash leaves either no seal or a complete one, never half of one.
func writeSeal(dir string, n int, s seal) error {
	final := filepath.Join(dir, sealName(n))
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(encodeSeal(s))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err == nil {
		err = syncDir(dir)
	}
	return err
}
