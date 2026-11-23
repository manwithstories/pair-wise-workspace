package checkpoint

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// writeIndex persists the header and entries atomically.
func (s *Store) writeIndex() error {
	buf := make([]byte, 0, 48+len(s.entries)*52)
	buf = append(buf, magic[:]...)
	buf = appendU32(buf, 1)
	buf = appendU32(buf, uint32(s.BlockSize))
	buf = appendU64(buf, s.samples)
	buf = appendU64(buf, s.bytes)
	buf = appendU32(buf, uint32(len(s.entries)))
	for _, e := range s.entries {
		buf = appendU32(buf, e.Index)
		buf = appendU32(buf, e.Samples)
		buf = appendU64(buf, e.Bytes)
		buf = appendU32(buf, e.CRC32)
		buf = append(buf, e.ChainHash[:]...)
	}
	return writeAtomic(s.indexPath(), buf)
}

// parseIndex decodes and validates the index framing.
func parseIndex(raw []byte) (Header, []Entry, error) {
	var h Header
	if len(raw) < 36 {
		return h, nil, fmt.Errorf("index shorter than header (%d bytes)", len(raw))
	}
	if len(raw) > maxIndexBytes {
		return h, nil, fmt.Errorf("index is %d bytes, over the %d limit", len(raw), maxIndexBytes)
	}
	copy(h.Magic[:], raw[0:8])
	if h.Magic != magic {
		return h, nil, fmt.Errorf("index magic mismatch")
	}
	h.Version = binary.BigEndian.Uint32(raw[8:12])
	if h.Version != 1 {
		return h, nil, fmt.Errorf("unsupported index version %d", h.Version)
	}
	h.BlockSize = binary.BigEndian.Uint32(raw[12:16])
	h.SampleCount = binary.BigEndian.Uint64(raw[16:24])
	h.ByteCount = binary.BigEndian.Uint64(raw[24:32])
	h.BlockCount = binary.BigEndian.Uint32(raw[32:36])
	if h.BlockCount > 1<<20 {
		return h, nil, fmt.Errorf("index declares %d blocks", h.BlockCount)
	}
	need := 36 + int(h.BlockCount)*52
	if len(raw) != need {
		return h, nil, fmt.Errorf("index length %d, want %d for %d blocks", len(raw), need, h.BlockCount)
	}
	entries := make([]Entry, 0, h.BlockCount)
	off := 36
	for i := uint32(0); i < h.BlockCount; i++ {
		e := Entry{
			Index:   binary.BigEndian.Uint32(raw[off : off+4]),
			Samples: binary.BigEndian.Uint32(raw[off+4 : off+8]),
			Bytes:   binary.BigEndian.Uint64(raw[off+8 : off+16]),
			CRC32:   binary.BigEndian.Uint32(raw[off+16 : off+20]),
		}
		copy(e.ChainHash[:], raw[off+20:off+52])
		if e.Index != i {
			return h, nil, fmt.Errorf("entry %d claims index %d", i, e.Index)
		}
		entries = append(entries, e)
		off += 52
	}
	return h, entries, nil
}

// writeAtomic writes data to path via a uniquely named temp file and rename.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(name)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func appendU32(b []byte, v uint32) []byte {
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], v)
	return append(b, t[:]...)
}
func appendU64(b []byte, v uint64) []byte {
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], v)
	return append(b, t[:]...)
}

// HashString renders a chain hash for terminal output.
func HashString(h [32]byte) string { return hex.EncodeToString(h[:]) }
