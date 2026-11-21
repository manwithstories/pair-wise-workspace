package remux

import (
	"bytes"
	"errors"
	"io"

	mp4 "github.com/abema/go-mp4"
)

// qualityBoxType is the custom four-character code carrying the JSON quality
// evidence inside the MP4 sidecar. It is registered as an any-type box so
// go-mp4 can parse it back without a typed payload model.
var qualityBoxType = mp4.StrToBoxType("arqv")

func init() {
	mp4.AddAnyTypeBoxDef(&mp4.AnyTypeBox{}, qualityBoxType)
}

// writeSeeker is an in-memory io.WriteSeeker; go-mp4's Writer rewinds to
// patch box sizes, which a plain bytes.Buffer cannot do.
type writeSeeker struct {
	buf []byte
	pos int64
}

func (w *writeSeeker) Write(p []byte) (int, error) {
	end := int(w.pos) + len(p)
	if end > len(w.buf) {
		grown := make([]byte, end)
		copy(grown, w.buf)
		w.buf = grown
	}
	copy(w.buf[w.pos:end], p)
	w.pos = int64(end)
	return len(p), nil
}

func (w *writeSeeker) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = w.pos + offset
	case io.SeekEnd:
		target = int64(len(w.buf)) + offset
	}
	if target < 0 {
		return 0, errNegativeSeek
	}
	w.pos = target
	return target, nil
}

func (w *writeSeeker) Bytes() []byte { return w.buf }

var errNegativeSeek = errors.New("negative seek position")

// WriteQualitySidecar emits a minimal standards-shaped MP4 container: an
// ftyp box plus a custom "arqv" box whose payload is the JSON quality
// evidence. go-mp4 performs the box framing and size accounting.
func WriteQualitySidecar(w io.Writer, evidenceJSON []byte) error {
	buf := &writeSeeker{}
	mw := mp4.NewWriter(buf)
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		return err
	}
	ftyp := &mp4.Ftyp{
		MajorBrand:   mp4.StrToBoxType("isom"),
		MinorVersion: 0x200,
		CompatibleBrands: []mp4.CompatibleBrandElem{
			{CompatibleBrand: mp4.StrToBoxType("isom")},
			{CompatibleBrand: mp4.StrToBoxType("mp42")},
		},
	}
	if _, err := mp4.Marshal(mw, ftyp, mp4.Context{}); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	if _, err := mw.StartBox(&mp4.BoxInfo{Type: qualityBoxType}); err != nil {
		return err
	}
	if _, err := mw.Write(append([]byte(nil), evidenceJSON...)); err != nil {
		return err
	}
	if _, err := mw.EndBox(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// ReadQualitySidecar parses the sidecar and returns the ftyp brand and the
// raw evidence payload, proving the MP4 framing round-trips.
func ReadQualitySidecar(r io.ReadSeeker) (majorBrand string, evidenceJSON []byte, err error) {
	found := struct {
		brand string
		data  []byte
	}{}
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "ftyp":
			payload, _, perr := h.ReadPayload()
			if perr != nil {
				return nil, perr
			}
			if f, ok := payload.(*mp4.Ftyp); ok {
				found.brand = string(f.MajorBrand[:])
			}
		case "arqv":
			var buf bytes.Buffer
			if _, perr := h.ReadData(&buf); perr != nil {
				return nil, perr
			}
			found.data = buf.Bytes()
			// arqv carries opaque JSON, not nested boxes: do not expand.
			return []interface{}{}, nil
		}
		return h.Expand()
	}
	if _, err = mp4.ReadBoxStructure(r, handler); err != nil {
		return "", nil, err
	}
	return found.brand, found.data, nil
}

// RoundTripQualitySidecar writes then reads the sidecar in memory.
func RoundTripQualitySidecar(evidenceJSON []byte) (string, []byte, error) {
	var buf bytes.Buffer
	if err := WriteQualitySidecar(&buf, evidenceJSON); err != nil {
		return "", nil, err
	}
	return ReadQualitySidecar(bytes.NewReader(buf.Bytes()))
}
