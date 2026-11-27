package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"

	"archive/internal/media"
)

// digestBytes returns lowercase hex SHA-256 of b.
func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func digestHasher() hash.Hash { return sha256.New() }

func digestHex(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// extractAVCC finds the first avcC box inside the moov/stsd hierarchy and
// returns its payload (the AVCDecoderConfigurationRecord body).
func extractAVCC(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data := make([]byte, st.Size())
	if _, err := f.Read(data); err != nil {
		return nil, err
	}
	// Locate moov, then walk children without full validation: this is an
	// encoder-side helper, and the sample entry is small.
	moovOff, moovSize, ok := topBox(data, "moov")
	if !ok {
		return nil, fmt.Errorf("%w: moov missing", media.ErrUnrecoverable)
	}
	avcc, err := findAVCC(data[moovOff:moovOff+moovSize], 0)
	if err != nil {
		return nil, err
	}
	if len(avcc) == 0 {
		return nil, fmt.Errorf("%w: avcC missing from stsd", media.ErrUnrecoverable)
	}
	return avcc, nil
}

func topBox(data []byte, typ string) (int64, int64, bool) {
	pos := int64(0)
	for pos+8 <= int64(len(data)) {
		size := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		hdr := int64(8)
		if size == 1 {
			size = int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
			hdr = 16
		} else if size == 0 {
			size = int64(len(data)) - pos
		}
		if size < hdr || pos+size > int64(len(data)) {
			return 0, 0, false
		}
		if string(data[pos+4:pos+8]) == typ {
			return pos, size, true
		}
		pos += size
	}
	return 0, 0, false
}

// findAVCC recursively searches for an avcC inside stsd entries.
func findAVCC(b []byte, depth int) ([]byte, error) {
	if depth > 16 {
		return nil, errors.New("stsd nesting too deep")
	}
	pos := 0
	// Allow searching a payload-only slice (stsd) or a container header.
	for pos+8 <= len(b) {
		size := int(binary.BigEndian.Uint32(b[pos : pos+4]))
		hdr := 8
		if size == 1 {
			if pos+16 > len(b) {
				return nil, errors.New("largesize truncated in stsd tree")
			}
			size = int(binary.BigEndian.Uint64(b[pos+8 : pos+16]))
			hdr = 16
		} else if size == 0 {
			size = len(b) - pos
		}
		if size < hdr || pos+size > len(b) {
			return nil, errors.New("box overruns in stsd tree")
		}
		typ := string(b[pos+4 : pos+8])
		if typ == "avcC" {
			return b[pos+hdr : pos+size], nil
		}
		body := b[pos+hdr : pos+size]
		switch typ {
		case "moov", "trak", "mdia", "minf", "stbl":
			if found, err := findAVCC(body, depth+1); found != nil || err != nil {
				return found, err
			}
		case "stsd":
			// stsd payload: version/flags + entry_count, then sample entries.
			if len(body) >= 8 {
				if found, err := findAVCC(body[8:], depth+1); found != nil || err != nil {
					return found, err
				}
			}
		case "avc1", "avc3":
			// VisualSampleEntry fields occupy 78 bytes after the box header;
			// decoder configuration child boxes start there.
			if len(body) >= 78 {
				if found, err := findAVCC(body[78:], depth+1); found != nil || err != nil {
					return found, err
				}
			}
		}
		pos += size
	}
	return nil, nil
}
