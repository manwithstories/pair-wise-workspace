// Package render streams repaired timeline frames into offscreen,
// auditable ISO BMFF (fragmented MP4) segments. No decoder or display is
// involved: each frame is a deterministic record with a payload hash.
// Segment boundaries and per-segment SHA-256 let an auditor re-verify the
// bytes against the source packet ranges. The renderer never holds more
// than one segment's frames, so memory is bounded by segment size, not the
// total packet count.
package render

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"

	mp4 "github.com/abema/go-mp4"

	"archive/internal/model"
)

// FrameRecord is the offscreen, serializable form of one timeline frame.
type FrameRecord struct {
	Index       int64
	Track       model.TrackKind
	TimeNanos   int64
	SourceStart int64
	SourceEnd   int64
	SSRC        uint32
	Keyframe    bool
	PayloadHash [32]byte
	ClockBasis  string
}

// SegmentWriter accumulates frames for one segment and flushes it as a
// fragmented MP4 file plus a deterministic byte hash.
type SegmentWriter struct {
	dir        string
	frames     []FrameRecord
	packetBase int64
	timeStart  int64
	index      int
}

// NewSegmentWriter prepares a writer that emits segment files under dir.
func NewSegmentWriter(dir string, index int, packetBase, timeStart int64) *SegmentWriter {
	return &SegmentWriter{dir: dir, index: index, packetBase: packetBase, timeStart: timeStart}
}

// Add appends a frame to the in-flight segment.
func (s *SegmentWriter) Add(f FrameRecord) {
	s.frames = append(s.frames, f)
}

// Len is the number of buffered frames.
func (s *SegmentWriter) Len() int { return len(s.frames) }

// Flush writes the buffered frames to segment_XXXX.m4s and returns the
// committed segment descriptor with its whole-file SHA-256.
func (s *SegmentWriter) Flush() (model.Segment, error) {
	if len(s.frames) == 0 {
		return model.Segment{}, model.Newf(model.KindRender, "segment", "segment %d has no frames", s.index)
	}
	name := filepath.Join(s.dir, segmentName(s.index))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return model.Segment{}, model.New(model.KindIO, name, "cannot create segment", err)
	}
	// go-mp4 seeks back to patch box-size fields after writing, so hashing
	// the in-flight byte stream would capture stale size headers. Write the
	// whole file first, then hash the final on-disk bytes so the digest
	// always matches what an independent verifier reads back.
	w := mp4.NewWriter(f)
	if err := writeHeader(w, s.index); err != nil {
		f.Close()
		return model.Segment{}, err
	}
	if err := writeFragment(w, s); err != nil {
		f.Close()
		return model.Segment{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return model.Segment{}, model.New(model.KindIO, name, "cannot sync segment", err)
	}
	if err := f.Close(); err != nil {
		return model.Segment{}, model.New(model.KindIO, name, "cannot close segment", err)
	}
	sum, err := hashFile(name)
	if err != nil {
		return model.Segment{}, err
	}
	seg := model.Segment{
		Index:       s.index,
		TimeStart:   s.timeStart,
		TimeEnd:     s.frames[len(s.frames)-1].TimeNanos,
		Frames:      len(s.frames),
		SHA256:      sum,
		ContentSHA:  sha256.Sum256(encodeFrames(s.frames)),
		PacketStart: s.packetBase,
		PacketEnd:   s.frames[len(s.frames)-1].SourceEnd,
	}
	s.frames = nil
	return seg, nil
}

func writeHeader(w *mp4.Writer, index int) error {
	styp := &mp4.Styp{
		MajorBrand:       brand("msdh"),
		MinorVersion:     0,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: brand("msdh")}, {CompatibleBrand: brand("msix")}},
	}
	if _, err := mp4.Marshal(w, styp, mp4.Context{}); err != nil {
		return model.New(model.KindRender, "styp", "cannot write segment header", err)
	}
	return nil
}

func writeFragment(w *mp4.Writer, s *SegmentWriter) error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return model.New(model.KindRender, "moof", "cannot open moof", err)
	}
	if _, err := mp4.Marshal(w, &mp4.Mfhd{SequenceNumber: uint32(s.index + 1)}, mp4.Context{}); err != nil {
		return model.New(model.KindRender, "mfhd", "cannot write mfhd", err)
	}
	if _, err := w.EndBox(); err != nil {
		return model.New(model.KindRender, "moof", "cannot close moof", err)
	}
	// Frame records live in mdat as a length-prefixed deterministic blob.
	data := encodeFrames(s.frames)
	if _, err := mp4.Marshal(w, &mp4.Mdat{Data: data}, mp4.Context{}); err != nil {
		return model.New(model.KindRender, "mdat", "cannot write frames", err)
	}
	return nil
}

// encodeFrames serializes frames deterministically: fixed-width fields
// followed by the payload hash and the clock-basis string.
func encodeFrames(frames []FrameRecord) []byte {
	buf := make([]byte, 0, len(frames)*96)
	tmp := make([]byte, 8)
	for _, fr := range frames {
		binary.BigEndian.PutUint64(tmp, uint64(fr.Index))
		buf = append(buf, tmp...)
		binary.BigEndian.PutUint64(tmp, uint64(fr.TimeNanos))
		buf = append(buf, tmp...)
		binary.BigEndian.PutUint64(tmp, uint64(fr.SourceStart))
		buf = append(buf, tmp...)
		binary.BigEndian.PutUint64(tmp, uint64(fr.SourceEnd))
		buf = append(buf, tmp...)
		binary.BigEndian.PutUint32(tmp[:4], fr.SSRC)
		buf = append(buf, tmp[:4]...)
		buf = append(buf, fr.PayloadHash[:]...)
		buf = append(buf, boolByte(fr.Keyframe), trackByte(fr.Track))
		buf = append(buf, []byte(fr.ClockBasis)...)
		buf = append(buf, 0)
	}
	return buf
}

func segmentName(index int) string {
	return "segment_" + pad4(index) + ".m4s"
}

func pad4(n int) string {
	d := []byte{byte('0' + n/1000), byte('0' + (n/100)%10), byte('0' + (n/10)%10), byte('0' + n%10)}
	return string(d)
}

func brand(s string) [4]byte {
	var b [4]byte
	copy(b[:], s)
	return b
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func trackByte(t model.TrackKind) byte {
	if t == model.TrackAudio {
		return 'a'
	}
	return 'v'
}

// hashFile returns the SHA-256 of a fully written segment file.
func hashFile(path string) ([32]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}, model.New(model.KindIO, path, "cannot read segment for hashing", err)
	}
	return sha256.Sum256(data), nil
}
