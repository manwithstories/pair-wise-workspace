package fixture

import (
	"encoding/binary"
	"fmt"

	"mediamerge/internal/model"
)

// Kind enumerates the four fault classes required by the acceptance tests.
type Kind string

const (
	KindTruncatedBox    Kind = "truncated-box"
	KindCorruptStbl     Kind = "corrupt-sample-table"
	KindTailTruncation  Kind = "tail-truncation"
	KindJournalMismatch Kind = "journal-mismatch"
	KindPayloadFlip     Kind = "corrupt-sample"
)

// Damage describes a one-point mutation. Offset is the first byte that is
// invalid; LocatorTarget is where a correct validator must report it.
type Damage struct {
	Kind          Kind
	Offset        int64
	LocatorTarget int64
	Detail        string
}

// Corrupt applies kind to a valid MP4 and returns the damaged bytes plus the
// expected byte locator. Every mutation is a single point so the locator
// tolerance can be one byte.
func Corrupt(data []byte, kind Kind) ([]byte, Damage, error) {
	out := append([]byte(nil), data...)
	switch kind {
	case KindTruncatedBox:
		// Shrink the declared moov size so the moov box overruns its parent
		// or leaves an unparsable trailing box. We pick the mdat header and
		// claim a size larger than the file.
		off, err := findBox(out, "mdat")
		if err != nil {
			return nil, Damage{}, err
		}
		if len(out) < off+16 {
			return nil, Damage{}, fmt.Errorf("fixture: mdat header too short")
		}
		declared := uint64(len(out)-off) + 4096
		if out[off+4-4] == 1 || binary.BigEndian.Uint32(out[off:off+4]) == 1 {
			binary.BigEndian.PutUint64(out[off+8:off+16], declared)
		} else {
			binary.BigEndian.PutUint32(out[off:off+4], uint32(len(out)-off+4096))
		}
		return out, Damage{Kind: kind, Offset: int64(len(out)), LocatorTarget: int64(len(out)),
			Detail: "mdat size extends past EOF"}, nil
	case KindCorruptStbl:
		// Point one stsz sample size at a payload region far outside mdat.
		off, err := findBox(out, "stsz")
		if err != nil {
			return nil, Damage{}, err
		}
		sizeOff := off + 8 + 4 + 4 + 4 // header, version/flags, sample_size, sample_count, entry 0
		if len(out) < sizeOff+4 {
			return nil, Damage{}, fmt.Errorf("fixture: stsz entry out of range")
		}
		binary.BigEndian.PutUint32(out[sizeOff:sizeOff+4], 0x7fffffff)
		return out, Damage{Kind: kind, Offset: int64(sizeOff), LocatorTarget: int64(sizeOff),
			Detail: "stsz sample size exceeds mdat"}, nil
	case KindTailTruncation:
		cut := len(out) * 3 / 4
		return out[:cut], Damage{Kind: kind, Offset: int64(cut), LocatorTarget: int64(cut),
			Detail: "file truncated mid-mdat"}, nil
	case KindPayloadFlip:
		off, err := findBox(out, "mdat")
		if err != nil {
			return nil, Damage{}, err
		}
		at := off + 16 + 64
		if at >= len(out) {
			return nil, Damage{}, fmt.Errorf("fixture: payload offset out of range")
		}
		out[at] ^= 0x5A
		return out, Damage{Kind: kind, Offset: int64(at), LocatorTarget: int64(at),
			Detail: "single payload byte flipped"}, nil
	}
	return nil, Damage{}, model.NewError(model.StageVerify, model.CodeUnsupported, -1, 0, false,
		"unknown corruption kind %q", kind)
}

var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"dinf": true, "edts": true, "udta": true, "mvex": true, "moof": true, "traf": true,
}

// findBox locates the first box of the given type at any nesting depth.
func findBox(data []byte, typ string) (int, error) {
	if off, ok := scanBoxes(data, 0, len(data), typ); ok {
		return off, nil
	}
	return 0, model.NewError(model.StageVerify, model.CodeMissingBox, 0, 0, false,
		"fixture: box %q not found", typ)
}

func scanBoxes(data []byte, start, end int, typ string) (int, bool) {
	off := start
	for off+8 <= end {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		hdr := 8
		if size == 1 {
			if off+16 > end {
				return 0, false
			}
			size = int(binary.BigEndian.Uint64(data[off+8 : off+16]))
			hdr = 16
		} else if size == 0 {
			size = end - off
		}
		if size < hdr || off+size > end {
			return 0, false
		}
		name := string(data[off+4 : off+8])
		if name == typ {
			return off, true
		}
		if containers[name] {
			if found, ok := scanBoxes(data, off+hdr, off+size, typ); ok {
				return found, true
			}
		}
		off += size
	}
	return 0, false
}
