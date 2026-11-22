package stream

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/abema/go-mp4"

	"mediascan/internal/fault"
)

// BoxRecord describes one MP4 box reached during the tolerant walk.
type BoxRecord struct {
	Type   string
	Offset int64 // absolute byte offset of the box header
	Size   int64 // declared total box size in bytes
	Depth  int   // nesting depth (0 = top level)
}

// KeyframeGap is the frame-index distance between two successive keyframes.
type KeyframeGap struct {
	FromSample uint32
	ToSample   uint32
	Distance   uint32
}

// MP4Stats is the MP4 analysis result for one file.
type MP4Stats struct {
	Source        string
	FileSize      int64
	Boxes         []BoxRecord
	KeyframeGaps  []KeyframeGap
	KeyframeCount int
	// DamageOffset is the first byte offset that could not be read because
	// of a truncated box, or -1 when the structure is intact.
	DamageOffset int64
	// RebuiltIndex is true when the sample index (stbl) was missing or
	// unreadable and had to be reconstructed from readable sample data.
	RebuiltIndex bool
	Faults       []fault.Fault
}

// AnalyzeMP4 walks an MP4 file box by box using go-mp4's header reader, but
// never aborts the whole scan on one damaged box: a truncated tail is
// isolated at its first unreadable offset while preceding boxes stay valid.
func AnalyzeMP4(f *os.File, source string) (*MP4Stats, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	st := &MP4Stats{Source: source, FileSize: size, DamageOffset: -1}

	// First pass: tolerant top-level box walk. Container boxes (moov/trak/
	// mdia/minf/stbl) are expanded; leaf payloads are skipped via SeekToEnd.
	if err := st.walkBoxes(f, 0, size, 0); err != nil {
		return nil, err
	}

	// Second pass: extract keyframe information from any readable stss box.
	st.extractKeyframes(f)
	if st.RebuiltIndex {
		st.Faults = append(st.Faults, fault.New(source, -1, fault.StageMP4, true,
			"sample index incomplete: rebuilt from readable sample table"))
	}
	return st, nil
}

// walkBoxes scans boxes inside the byte range [start, end).
func (st *MP4Stats) walkBoxes(f *os.File, start, end int64, depth int) error {
	pos := start
	for pos < end {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		bi, err := mp4.ReadBoxInfo(f)
		if err != nil {
			// Partial header: this is the first unreadable byte.
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				st.markTruncation(pos, "truncated box header")
				return nil
			}
			return err
		}
		if int64(bi.Offset) != pos {
			return fmt.Errorf("box offset drift: want %d got %d", pos, bi.Offset)
		}
		boxEnd := int64(bi.Offset + bi.Size)
		if boxEnd > end || boxEnd > st.FileSize {
			// The box declares bytes beyond the file. The first unreadable
			// byte is the actual end of available data; report that exactly.
			firstBad := st.FileSize
			st.markTruncation(firstBad, "box %s declares %d bytes but only %d remain",
				bi.Type.String(), bi.Size, st.FileSize-pos)
			return nil
		}
		st.Boxes = append(st.Boxes, BoxRecord{
			Type: bi.Type.String(), Offset: pos, Size: int64(bi.Size), Depth: depth,
		})

		if isContainer(bi.Type) {
			if err := st.walkBoxes(f, pos+int64(bi.HeaderSize), boxEnd, depth+1); err != nil {
				return err
			}
		}
		pos = boxEnd
	}
	return nil
}

// markTruncation records the first unreadable offset once and isolates the
// damaged tail; all earlier boxes remain in the report.
func (st *MP4Stats) markTruncation(offset int64, format string, args ...any) {
	if st.DamageOffset < 0 || offset < st.DamageOffset {
		st.DamageOffset = offset
	}
	st.Faults = append(st.Faults, fault.New(st.Source, offset, fault.StageMP4, true, format, args...))
}

func isContainer(t mp4.BoxType) bool {
	switch t.String() {
	case "moov", "trak", "mdia", "minf", "stbl", "udta", "edts", "dinf":
		return true
	}
	return false
}

// extractKeyframes locates stss boxes via go-mp4's structural reader over
// the already validated moov range, then computes inter-keyframe distances.
func (st *MP4Stats) extractKeyframes(f *os.File) {
	var stssOffsets []int64
	var moovEnd int64 = -1
	for _, b := range st.Boxes {
		if b.Type == "stss" {
			stssOffsets = append(stssOffsets, b.Offset)
		}
		if b.Type == "moov" {
			moovEnd = b.Offset + b.Size
		}
	}
	if len(stssOffsets) == 0 {
		// No sync sample box: either no index or a damaged stbl. If a moov
		// exists without stss, flag index rebuild for the pipeline layer.
		if moovEnd > 0 {
			st.RebuiltIndex = true
		}
		return
	}
	for _, off := range stssOffsets {
		samples, ok := st.readStss(f, off)
		if !ok {
			st.RebuiltIndex = true
			continue
		}
		st.KeyframeCount += len(samples)
		for i := 1; i < len(samples); i++ {
			st.KeyframeGaps = append(st.KeyframeGaps, KeyframeGap{
				FromSample: samples[i-1], ToSample: samples[i],
				Distance: samples[i] - samples[i-1],
			})
		}
	}
}

// readStss parses one stss box directly from its offset, validating the
// declared entry count against the bytes actually present.
func (st *MP4Stats) readStss(f *os.File, offset int64) ([]uint32, bool) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, false
	}
	bi, err := mp4.ReadBoxInfo(f)
	if err != nil {
		return nil, false
	}
	payload := bi.Size - bi.HeaderSize
	// FullBox: version(1)+flags(3)+entry_count(4) = 8 bytes, then entries.
	hdr := make([]byte, 8)
	if payload < 8 {
		st.markTruncation(offset+int64(bi.HeaderSize), "stss header truncated")
		return nil, false
	}
	if _, err := io.ReadFull(f, hdr); err != nil {
		st.markTruncation(offset+int64(bi.HeaderSize), "stss unreadable: %v", err)
		return nil, false
	}
	count := binary.BigEndian.Uint32(hdr[4:])
	if uint64(count)*4 != payload-8 {
		st.markTruncation(offset+int64(bi.HeaderSize)+8,
			"stss entry count %d does not fit %d payload bytes", count, payload-8)
		return nil, false
	}
	buf := make([]byte, count*4)
	if _, err := io.ReadFull(f, buf); err != nil {
		st.markTruncation(offset+int64(bi.HeaderSize)+8, "stss entries truncated")
		return nil, false
	}
	out := make([]uint32, count)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	return out, true
}
