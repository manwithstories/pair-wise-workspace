package demux

import (
	"context"
	"io"
	"os"

	"mediamerge/internal/model"
)

// Reader owns an open input file and its validated index. All sample access
// goes through it; callers must Close it.
type Reader struct {
	*File
	file *os.File
}

// Open validates path and returns a reader positioned for sample access.
func Open(ctx context.Context, path string) (*Reader, error) {
	f, err := Read(ctx, path)
	if err != nil {
		return nil, err
	}
	h, err := os.Open(path)
	if err != nil {
		return nil, derr(model.StageDemux, model.CodeTruncated, 0, 0, false, "open input: %v", err)
	}
	return &Reader{File: f, file: h}, nil
}

// Close releases the input handle.
func (r *Reader) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

// ReadSample copies one sample payload from the input file.
func (r *Reader) ReadSample(s model.SampleRef) ([]byte, error) {
	buf := make([]byte, s.Size)
	n, err := r.file.ReadAt(buf, s.Offset)
	if err != nil && n != int(s.Size) {
		return nil, derr(model.StageDemux, model.CodeTruncated, s.Offset+int64(n), s.TrackID, false,
			"short read for track %d sample %d at %d: %v", s.TrackID, s.Index, s.Offset, err)
	}
	return buf[:n], nil
}

// WriteSampleTo copies one sample payload to w without buffering it whole.
func (r *Reader) WriteSampleTo(w io.Writer, s model.SampleRef) (int64, error) {
	n, err := io.CopyN(w, io.NewSectionReader(r.file, s.Offset, int64(s.Size)), int64(s.Size))
	if err != nil {
		return n, derr(model.StageDemux, model.CodeTruncated, s.Offset+n, s.TrackID, false,
			"short read for track %d sample %d at %d: %v", s.TrackID, s.Index, s.Offset, err)
	}
	return n, nil
}

// buildSamples cross-checks every sample reference against the mdat extent and
// the file length. The reported offset is the first byte that is not valid.
func (f *File) buildSamples() error {
	if len(f.Tracks) == 0 {
		return derr(model.StageDemux, model.CodeMissingBox, 0, 0, false, "no supported tracks found")
	}
	total := 0
	seenIDs := map[uint32]bool{}
	for ti, t := range f.Tracks {
		if seenIDs[t.InputID] {
			off := int64(0)
			if len(t.Samples) > 0 {
				off = t.Samples[0].Offset
			}
			return derr(model.StageDemux, model.CodeMapping, off, t.InputID, false,
				"duplicate track id %d", t.InputID)
		}
		seenIDs[t.InputID] = true
		if t.Codec == "" || len(t.Codec) > 7 && t.Codec[:8] == "unknown:" {
			return derr(model.StageDemux, model.CodeUnsupported, 0, t.InputID, false,
				"track %d uses unsupported sample entry %q", t.InputID, t.Codec)
		}
		for i := range t.Samples {
			s := &t.Samples[i]
			s.TrackIndex = ti
			if s.Offset < 0 {
				return derr(model.StageDemux, model.CodeSampleTable, 0, t.InputID, false,
					"track %d has %d samples with no chunk mapping", t.InputID, t.SampleCount-i)
			}
			end := s.Offset + int64(s.Size)
			if s.Offset < f.Mdat.PayloadStart || end > f.Mdat.PayloadEnd {
				off := s.Offset
				if off < f.Mdat.PayloadStart {
					off = f.Mdat.PayloadStart
				}
				if end > f.Mdat.PayloadEnd {
					off = f.Mdat.PayloadEnd
				}
				tableOff := t.SizeTableOffset
				if tableOff > 0 {
					off = tableOff + int64(i)*4
					if t.SizeTableFixed {
						off = t.SizeTableOffset
					}
				}
				return derr(model.StageDemux, model.CodeSampleTable, off, t.InputID, false,
					"track %d sample %d size %d range [%d,%d) outside mdat payload [%d,%d)",
					t.InputID, i, s.Size, s.Offset, end, f.Mdat.PayloadStart, f.Mdat.PayloadEnd)
			}
			if end > f.Size {
				return derr(model.StageDemux, model.CodeTruncated, f.Size, t.InputID, false,
					"track %d sample %d ends at %d beyond file size %d", t.InputID, i, end, f.Size)
			}
		}
		if !monotonicDTS(t.Samples) {
			return derr(model.StageDemux, model.CodeNonMonotonic, t.Samples[0].Offset, t.InputID, false,
				"track %d sample timestamps are not monotonic", t.InputID)
		}
		total += len(t.Samples)
	}
	if total == 0 {
		return derr(model.StageDemux, model.CodeMissingBox, 0, 0, false, "input contains no samples")
	}
	return nil
}

func monotonicDTS(s []model.SampleRef) bool {
	var last uint64
	seen := false
	for _, x := range s {
		if seen && x.DTS < last {
			return false
		}
		last = x.DTS
		seen = true
	}
	return true
}
