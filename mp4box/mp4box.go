// Package mp4box wraps segment payloads in a valid fragmented ISO-BMFF
// container using go-mp4 v1.3.0.
//
// Each emitted segment file is a self-contained fragment: a FileType box
// (ftyp, major brand iso5) followed by a MediaData box (mdat) carrying the
// deterministic segment payload. A reader round-trips the structure back, so
// corruption of the container is detectable independently of payload hashing.
package mp4box

import (
	"bytes"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediad/diag"
)

// majorBrandISO5 is the four-character "iso5" fragmented-MP4 brand.
var majorBrandISO5 = [4]byte{'i', 's', 'o', '5'}

// Encode wraps payload in a minimal valid fMP4 fragment: ftyp then mdat.
func Encode(payload []byte) ([]byte, error) {
	mem := newSeekableBuffer()
	w := mp4.NewWriter(mem)

	// ftyp box declares the fragmented-MP4 compatible brand.
	ftyp := &mp4.Ftyp{MajorBrand: majorBrandISO5, MinorVersion: 1}
	ftyp.AddCompatibleBrand(majorBrandISO5)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return nil, diag.New(diag.EOutput, "", "start ftyp: %v", err)
	}
	if _, err := mp4.Marshal(w, ftyp, mp4.Context{}); err != nil {
		return nil, diag.New(diag.EOutput, "", "marshal ftyp: %v", err)
	}
	if _, err := w.EndBox(); err != nil {
		return nil, diag.New(diag.EOutput, "", "end ftyp: %v", err)
	}

	// mdat box carries the payload verbatim.
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return nil, diag.New(diag.EOutput, "", "start mdat: %v", err)
	}
	if _, err := mp4.Marshal(w, &mp4.Mdat{Data: append([]byte(nil), payload...)}, mp4.Context{}); err != nil {
		return nil, diag.New(diag.EOutput, "", "marshal mdat: %v", err)
	}
	if _, err := w.EndBox(); err != nil {
		return nil, diag.New(diag.EOutput, "", "end mdat: %v", err)
	}
	return mem.bytes(), nil
}

// seekableBuffer is an in-memory io.WriteSeeker supporting true random access.
// go-mp4's EndBox seeks backward to overwrite the box size, which an
// append-only bytes.Buffer cannot do. Writes extend or overwrite at the current
// position; the slice grows to cover the highest byte written.
type seekableBuffer struct {
	data []byte
	pos  int64
}

func newSeekableBuffer() *seekableBuffer { return &seekableBuffer{} }

func (s *seekableBuffer) Write(p []byte) (int, error) {
	end := int(s.pos) + len(p)
	if end > len(s.data) {
		grown := make([]byte, end)
		copy(grown, s.data)
		s.data = grown
	}
	copy(s.data[s.pos:end], p)
	s.pos = int64(end)
	return len(p), nil
}

func (s *seekableBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	default:
		s.pos = int64(len(s.data)) + offset
	}
	if s.pos < 0 {
		return 0, diag.New(diag.EOutput, "", "negative seek")
	}
	return s.pos, nil
}

func (s *seekableBuffer) bytes() []byte { return append([]byte(nil), s.data...) }

// Decode reads a fragment back and returns the mdat payload plus the ftyp
// major brand. A structurally invalid container returns E_INPUT_CORRUPT.
func Decode(raw []byte) (payload []byte, brand string, err error) {
	r := bytes.NewReader(raw)
	var gotBrand string
	var gotPayload []byte
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "ftyp":
			box, _, lerr := h.ReadPayload()
			if lerr != nil {
				return nil, lerr
			}
			if f, ok := box.(*mp4.Ftyp); ok {
				gotBrand = string(f.MajorBrand[:])
			}
		case "mdat":
			box, _, lerr := h.ReadPayload()
			if lerr != nil {
				return nil, lerr
			}
			if m, ok := box.(*mp4.Mdat); ok {
				gotPayload = m.Data
			}
		}
		return nil, nil
	}
	if _, perr := mp4.ReadBoxStructure(r, handler); perr != nil {
		return nil, "", diag.New(diag.EInputCorrupt, "", "invalid fMP4 container: %v", perr)
	}
	if gotBrand == "" || gotPayload == nil {
		return nil, "", diag.New(diag.EInputCorrupt, "", "fragment missing ftyp or mdat box")
	}
	return gotPayload, gotBrand, nil
}
