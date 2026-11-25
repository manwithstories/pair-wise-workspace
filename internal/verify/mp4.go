package verify

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediaproc/internal/model"
)

// SupportedMP4Codec reports whether a source codec has a validated sidecar.
func SupportedMP4Codec(codecID string) bool {
	switch codecID {
	case "V_MPEG4/ISO/AVC", "A_OPUS", "V_VP9", "V_VP8":
		return true
	default:
		return false
	}
}

type seekBuffer struct {
	b   []byte
	pos int
}

func (s *seekBuffer) Write(p []byte) (int, error) {
	end := s.pos + len(p)
	if end > len(s.b) {
		grown := make([]byte, end)
		copy(grown, s.b)
		s.b = grown
	}
	copy(s.b[s.pos:end], p)
	s.pos = end
	return len(p), nil
}

func (s *seekBuffer) Seek(offset int64, whence int) (int64, error) {
	var target int
	switch whence {
	case io.SeekStart:
		target = int(offset)
	case io.SeekCurrent:
		target = s.pos + int(offset)
	case io.SeekEnd:
		target = len(s.b) + int(offset)
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if target < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	s.pos = target
	return int64(target), nil
}

// BuildMP4Sidecar creates a small fragmented MP4 box tree. It is deliberately
// metadata-only plus a copy of the segment hash/payload reference; the media
// payload itself remains in the deterministic .bin artifact.
func BuildMP4Sidecar(sequence uint32, decodeTimeNS model.Nanoseconds, payload []byte) ([]byte, error) {
	var sb seekBuffer
	w := mp4.NewWriter(&sb)

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return nil, err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 0x200,
	}
	ftyp.AddCompatibleBrand([4]byte{'i', 's', 'o', 'm'})
	ftyp.AddCompatibleBrand([4]byte{'m', 'p', '4', '1'})
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoof()}); err != nil {
		return nil, err
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMfhd()}); err != nil {
		return nil, err
	}
	if _, err := mp4.Marshal(w, &mp4.Mfhd{SequenceNumber: sequence + 1}, mp4.Context{}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTraf()}); err != nil {
		return nil, err
	}
	tfhd := &mp4.Tfhd{TrackID: 1, DefaultSampleDuration: 20 * 48}
	tfhd.SetFlags(mp4.TfhdDefaultSampleDurationPresent)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTfhd()}); err != nil {
		return nil, err
	}
	if _, err := mp4.Marshal(w, tfhd, mp4.Context{}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}

	tfdt := &mp4.Tfdt{BaseMediaDecodeTimeV0: uint32(decodeTimeNS / model.Millisecond)}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeTfdt()}); err != nil {
		return nil, err
	}
	if _, err := mp4.Marshal(w, tfdt, mp4.Context{}); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // traf
		return nil, err
	}
	if _, err := w.EndBox(); err != nil { // moof
		return nil, err
	}

	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return nil, err
	}
	if _, err := w.Write(payload); err != nil {
		return nil, err
	}
	if _, err := w.EndBox(); err != nil {
		return nil, err
	}
	return append([]byte(nil), sb.b...), nil
}

// ValidateMP4Sidecar uses Abema go-mp4 to probe the generated box structure.
func ValidateMP4Sidecar(data []byte) error {
	info, err := mp4.Probe(bytes.NewReader(data))
	if err != nil {
		return model.InvariantError(model.PhaseMP4, 0, "go-mp4 probe failed", err)
	}
	if len(info.Segments) != 1 {
		return model.InvariantError(model.PhaseMP4, 0,
			fmt.Sprintf("expected one MP4 fragment, got %d", len(info.Segments)), nil)
	}
	return nil
}
