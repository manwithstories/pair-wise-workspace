package demux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"archivecheck/internal/media"
)

// ParseConfig bounds every demux run. ScanLimit caps the number of bytes
// inspected while rebuilding missing/corrupt indexes; MaxSampleBytes caps a
// single sample so a corrupt length field cannot trigger a huge allocation.
type ParseConfig struct {
	ScanLimit      int64
	MaxSampleBytes int64
}

// DefaultParseConfig mirrors the archive's per-fragment memory limits.
func DefaultParseConfig() ParseConfig {
	return ParseConfig{ScanLimit: 4 << 20, MaxSampleBytes: 1 << 20}
}

type rawBox struct {
	info    *mp4.BoxInfo
	payload int64 // absolute offset of the box payload
	end     int64 // absolute offset one past the box
}

// containerBoxes are the only box types whose payload holds child boxes.
// Everything else is a leaf and must never be walked as a container.
var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"dinf": true, "edts": true, "udta": true, "mvex": true, "moof": true,
	"traf": true, "mfra": true, "meco": true, "sinf": true, "schi": true,
}

// walkBoxes iterates boxes inside [start,end). Every reported box is fully
// contained in the file; a header claiming to run past EOF is a located fault.
// A truncated tail (short header at the end) is reported as non-fatal so the
// caller can keep everything parsed before it.
func walkBoxes(r io.ReadSeeker, start, end int64, seg string, faults *media.FaultSet, fn func(rawBox) error) error {
	pos := start
	for pos < end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		info, err := mp4.ReadBoxInfo(r)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				faults.Add(media.NewFault(media.KindTruncated, seg, 0, pos,
					"box header truncated at tail, keeping previously complete boxes"))
				return nil
			}
			return err
		}
		boxEnd := pos + int64(info.Size)
		if info.Size < info.HeaderSize || boxEnd > end {
			// A mdat running past EOF is a truncated media payload, not a
			// corrupt container: clamp it to EOF so sample assembly can retain
			// every fully-contained sample. Any other box overrun is fatal.
			if info.Type.String() == "mdat" && boxEnd > end {
				faults.Add(media.NewFault(media.KindTruncated, seg, 0, pos,
					"mdat declares %d bytes but file ends at %d; tail truncated", info.Size, end-pos))
				rb := rawBox{info: info, payload: pos + int64(info.HeaderSize), end: end}
				if err := fn(rb); err != nil {
					return err
				}
				return nil
			}
			f := media.NewFault(media.KindBadLength, seg, 0, pos,
				"box %q declares size %d exceeding container end %d", info.Type.String(), info.Size, end-pos)
			faults.Add(f)
			return f
		}
		rb := rawBox{info: info, payload: pos + int64(info.HeaderSize), end: boxEnd}
		if err := fn(rb); err != nil {
			return err
		}
		pos = boxEnd
	}
	return nil
}

// readPayload opens the payload of a box as a bounded byte slice. The cap
// protects against corrupt length fields forcing oversized allocations.
func readPayloadAt(f *os.File, off, size int64, cap int64) ([]byte, error) {
	if size < 0 || size > cap {
		return nil, fmt.Errorf("payload size %d exceeds cap %d at offset %d", size, cap, off)
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}

// tableBox is the shared layout of all full-box sample-table entries.
type fullBox struct{ verFlags uint32 }

func readFullBox(b []byte) fullBox {
	if len(b) < 4 {
		return fullBox{}
	}
	return fullBox{verFlags: binary.BigEndian.Uint32(b)}
}

// stts: version/flags(4) count(4) entries(count, {count,delta} u32).
func parseStts(b []byte) (deltas []uint32, err error) {
	if len(b) < 8 {
		return nil, errors.New("stts too short")
	}
	n := binary.BigEndian.Uint32(b[4:8])
	if uint64(n)*8+8 > uint64(len(b)) {
		return nil, fmt.Errorf("stts entry count %d exceeds box", n)
	}
	deltas = make([]uint32, 0, n)
	for i := uint32(0); i < n; i++ {
		cnt := binary.BigEndian.Uint32(b[8+8*i:])
		delta := binary.BigEndian.Uint32(b[12+8*i:])
		for j := uint32(0); j < cnt; j++ {
			deltas = append(deltas, delta)
		}
	}
	return deltas, nil
}

// stsz: flags(4) sample_size(4) sample_count(4) then sizes if sample_size==0.
func parseStsz(b []byte) (sizes []uint32, uniform uint32, err error) {
	if len(b) < 12 {
		return nil, 0, errors.New("stsz too short")
	}
	uniform = binary.BigEndian.Uint32(b[4:8])
	n := binary.BigEndian.Uint32(b[8:12])
	if uniform != 0 {
		return nil, uniform, nil
	}
	if uint64(n)*4+12 > uint64(len(b)) {
		return nil, 0, fmt.Errorf("stsz entry count %d exceeds box", n)
	}
	sizes = make([]uint32, n)
	for i := range sizes {
		sizes[i] = binary.BigEndian.Uint32(b[12+4*i:])
	}
	return sizes, 0, nil
}

// stss: sync sample table (1-based sample numbers).
func parseStss(b []byte) ([]uint32, error) {
	if len(b) < 8 {
		return nil, errors.New("stss too short")
	}
	n := binary.BigEndian.Uint32(b[4:8])
	if uint64(n)*4+8 > uint64(len(b)) {
		return nil, fmt.Errorf("stss entry count %d exceeds box", n)
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(b[8+4*i:])
	}
	return out, nil
}

// stsc: flags(4) count(4) entries {first_chunk, samples_per_chunk, desc_idx}.
type stscEntry struct {
	FirstChunk      uint32
	SamplesPerChunk uint32
}

func parseStsc(b []byte) ([]stscEntry, error) {
	if len(b) < 8 {
		return nil, errors.New("stsc too short")
	}
	n := binary.BigEndian.Uint32(b[4:8])
	if uint64(n)*12+8 > uint64(len(b)) {
		return nil, fmt.Errorf("stsc entry count %d exceeds box", n)
	}
	out := make([]stscEntry, n)
	for i := range out {
		out[i] = stscEntry{
			FirstChunk:      binary.BigEndian.Uint32(b[8+12*i:]),
			SamplesPerChunk: binary.BigEndian.Uint32(b[12+12*i:]),
		}
	}
	return out, nil
}

// parseOffsets reads stco (u32) or co64 (u64) chunk offsets.
func parseOffsets(b []byte, wide bool) ([]int64, error) {
	if len(b) < 8 {
		return nil, errors.New("offset table too short")
	}
	n := binary.BigEndian.Uint32(b[4:8])
	step := 4
	if wide {
		step = 8
	}
	if uint64(n)*uint64(step)+8 > uint64(len(b)) {
		return nil, fmt.Errorf("offset entry count %d exceeds box", n)
	}
	out := make([]int64, n)
	for i := range out {
		if wide {
			out[i] = int64(binary.BigEndian.Uint64(b[8+8*i:]))
		} else {
			out[i] = int64(binary.BigEndian.Uint32(b[8+4*i:]))
		}
	}
	return out, nil
}
