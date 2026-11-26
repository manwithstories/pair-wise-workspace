package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"mediascan/internal/media"
)

// MaxFileSize is the permitted size of one local asset: 8 GiB.
const MaxFileSize int64 = 8 << 30

// MaxBoxDepth is the maximum accepted MP4/MOV box nesting depth.
const MaxBoxDepth = 32

// Source is a size-bounded, content-hashed random-access input.
type Source struct {
	ReaderAt io.ReaderAt
	Closer   io.Closer
	Name     string
	Size     int64
	SHA256   string
}

// Close releases an OS file when the source owns one.
func (s *Source) Close() error {
	if s.Closer == nil {
		return nil
	}
	return s.Closer.Close()
}

// HashBytes returns the lowercase SHA-256 of in-memory fixture data.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// OpenSource hashes and opens a local file without following path components
// outside the caller's workspace. Callers are responsible for validating the
// cleaned path before calling this function.
func OpenSource(path string) (*Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: not a regular file", media.Fault{Code: media.CodeInputPathUnsafe})
	}
	if st.Size() < 0 || st.Size() > MaxFileSize {
		_ = f.Close()
		return nil, media.Fault{Code: media.CodeFileTooLarge, ByteOffset: 0, Message: fmt.Sprintf("size=%d", st.Size())}
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Source{
		ReaderAt: f,
		Closer:   f,
		Name:     path,
		Size:     st.Size(),
		SHA256:   hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// Box is one parsed MP4/MOV box header and its resolved byte range.
type Box struct {
	Type          string
	Offset        int64
	HeaderSize    int64
	DeclaredSize  uint64
	Size          int64
	PayloadOffset int64
	PayloadSize   int64
	End           int64
	Depth         int
	TrackID       uint32
	Children      []*Box
}

// BoxIndex contains every header in parse order and the top-level roots.
type BoxIndex struct {
	Root []*Box
	All  []*Box
}

// FindAll returns all boxes of the requested four-character type.
func (idx *BoxIndex) FindAll(typ string) []*Box {
	var out []*Box
	for _, b := range idx.All {
		if b.Type == typ {
			out = append(out, b)
		}
	}
	return out
}

var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"edts": true, "dinf": true, "mvex": true, "moof": true, "traf": true,
	"udta": true, "meta": true, "ipro": true, "sinf": true, "schi": true,
}

func readExactAt(r io.ReaderAt, p []byte, off int64) error {
	if off < 0 {
		return fmt.Errorf("negative read offset %d", off)
	}
	n, err := r.ReadAt(p, off)
	if errors.Is(err, io.EOF) && n == len(p) {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(p) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// ParseBoxes validates every box header and builds a bounded structural index.
// Malformed boxes produce Faults; parsing continues whenever a safe boundary is
// known.
func ParseBoxes(src *Source) (*BoxIndex, []media.Fault) {
	idx := &BoxIndex{}
	var faults []media.Fault
	if src == nil || src.Size < 0 {
		return idx, []media.Fault{{Code: media.CodeBoxSizeInvalid, Severity: media.SeverityError, Message: "nil or invalid source"}}
	}
	faults = parseRange(src, 0, src.Size, 0, 0, idx, &faults)
	return idx, faults
}

func parseRange(src *Source, start, limit int64, depth int, trackID uint32, idx *BoxIndex, faults *[]media.Fault) []media.Fault {
	off := start
	for off < limit {
		remaining := limit - off
		if remaining < 8 {
			*faults = append(*faults, faultAt(src, media.CodeTruncatedBox, media.SeverityError, trackID, off, media.Time{},
				"short box header: %d bytes remain", remaining))
			return *faults
		}

		header := make([]byte, 16)
		if err := readExactAt(src.ReaderAt, header[:8], off); err != nil {
			*faults = append(*faults, faultAt(src, media.CodeTruncatedBox, media.SeverityError, trackID, off, media.Time{},
				"read box header: %v", err))
			return *faults
		}
		declared := uint64(header[0])<<24 | uint64(header[1])<<16 | uint64(header[2])<<8 | uint64(header[3])
		typ := string(header[4:8])
		headerSize := int64(8)

		if declared == 1 {
			if err := readExactAt(src.ReaderAt, header[8:16], off+8); err != nil {
				*faults = append(*faults, faultAt(src, media.CodeTruncatedBox, media.SeverityError, trackID, off, media.Time{},
					"short 64-bit box size: %v", err))
				return *faults
			}
			declared = uint64(header[8])<<56 | uint64(header[9])<<48 | uint64(header[10])<<40 | uint64(header[11])<<32 |
				uint64(header[12])<<24 | uint64(header[13])<<16 | uint64(header[14])<<8 | uint64(header[15])
			headerSize = 16
		}

		b := &Box{Type: typ, Offset: off, HeaderSize: headerSize, DeclaredSize: declared, Depth: depth, TrackID: trackID}
		if declared == 0 {
			b.Size = limit - off
			b.End = limit
		} else {
			if declared < uint64(headerSize) {
				*faults = append(*faults, faultAt(src, media.CodeBoxSizeInvalid, media.SeverityError, trackID, off, media.Time{},
					"declared size %d < header %d", declared, headerSize))
				return *faults
			}
			if uint64(off) > math.MaxInt64-declared {
				*faults = append(*faults, faultAt(src, media.CodeIntegerOverflow, media.SeverityError, trackID, off, media.Time{},
					"box end overflows int64: offset=%d size=%d", off, declared))
				return *faults
			}
			b.Size = int64(declared)
			b.End = off + b.Size
		}
		b.PayloadOffset = off + headerSize
		b.PayloadSize = b.Size - headerSize

		if b.End > src.Size {
			code := media.CodeTruncatedBox
			if typ == "mdat" {
				code = media.CodeMDATPastEOF
			}
			*faults = append(*faults, faultAt(src, code, media.SeverityError, trackID, off, media.Time{},
				"box %s declared end %d exceeds file end %d by %d", typ, b.End, src.Size, b.End-src.Size))
			return *faults
		}
		if b.End > limit {
			*faults = append(*faults, faultAt(src, media.CodeBoxOutsideParent, media.SeverityError, trackID, off, media.Time{},
				"box %s ends at %d beyond parent limit %d", typ, b.End, limit))
			b.End = limit
			b.Size = b.End - off
			b.PayloadSize = b.Size - headerSize
		}

		if depth >= MaxBoxDepth {
			*faults = append(*faults, faultAt(src, media.CodeBoxDepthExceeded, media.SeverityError, trackID, off, media.Time{},
				"box %s exceeds maximum depth %d", typ, MaxBoxDepth))
			return *faults
		}

		if typ == "tkhd" {
			if id, ok := readTKHDTrackID(src, b); ok {
				b.TrackID = id
				trackID = id
			}
		}
		if typ == "tfhd" {
			if id, ok := readTFHDTrackID(src, b); ok {
				b.TrackID = id
				trackID = id
			}
		}
		idx.All = append(idx.All, b)
		if depth == 0 {
			idx.Root = append(idx.Root, b)
		}

		if !isKnownBoxType(typ) {
			*faults = append(*faults, faultAt(src, media.CodeUnknownBoxSkipped, media.SeverityInfo, trackID, off, media.Time{},
				"unknown box %q skipped", typ))
		}

		if containerTypes[typ] {
			childStart := b.PayloadOffset
			if typ == "meta" && b.PayloadSize >= 4 {
				childStart += 4 // meta is a FullBox before its children.
			}
			if childStart <= b.End {
				parseRange(src, childStart, b.End, depth+1, trackID, idx, faults)
			}
		}
		off = b.End
		if b.Size <= 0 {
			break
		}
	}
	return *faults
}

func readTKHDTrackID(src *Source, b *Box) (uint32, bool) {
	// Payload begins with version+flags. v0 track_ID is at payload offset 12;
	// v1 uses 64-bit creation/modification times, moving it to offset 20.
	ver := make([]byte, 1)
	if err := readExactAt(src.ReaderAt, ver, b.PayloadOffset); err != nil {
		return 0, false
	}
	pos := b.PayloadOffset + 12
	if ver[0] == 1 {
		pos = b.PayloadOffset + 20
	}
	buf := make([]byte, 4)
	if err := readExactAt(src.ReaderAt, buf, pos); err != nil {
		return 0, false
	}
	return uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]), true
}

func readTFHDTrackID(src *Source, b *Box) (uint32, bool) {
	pos := b.PayloadOffset + 4 // skip version+flags
	buf := make([]byte, 4)
	if err := readExactAt(src.ReaderAt, buf, pos); err != nil {
		return 0, false
	}
	return uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]), true
}

func faultAt(src *Source, code, severity string, trackID uint32, off int64, t media.Time, format string, args ...any) media.Fault {
	hash, name := "", ""
	if src != nil {
		hash, name = src.SHA256, src.Name
	}
	return media.NewFault(code, severity, hash, name, trackID, off, t, format, args...)
}

func isKnownBoxType(typ string) bool {
	known := map[string]bool{
		"ftyp": true, "moov": true, "mvhd": true, "trak": true, "tkhd": true,
		"edts": true, "elst": true, "mdia": true, "mdhd": true, "hdlr": true,
		"minf": true, "vmhd": true, "smhd": true, "dinf": true, "dref": true,
		"stbl": true, "stsd": true, "stts": true, "ctts": true, "stsc": true,
		"stsz": true, "stz2": true, "stco": true, "co64": true, "stss": true,
		"mdat": true, "free": true, "skip": true, "wide": true, "pasp": true,
		"mvex": true, "trex": true, "moof": true, "mfhd": true, "traf": true,
		"tfhd": true, "tfdt": true, "trun": true, "udta": true, "meta": true,
		"url ": true, "urn ": true, "avc1": true, "avcC": true, "hvc1": true,
		"hvcC": true, "mp4a": true, "esds": true, "colr": true, "btrt": true,
	}
	return known[typ]
}
