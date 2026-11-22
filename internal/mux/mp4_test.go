package mux_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/abema/go-mp4"

	"mediapack/internal/codec"
	"mediapack/internal/fixture"
	"mediapack/internal/mux"
)

func muxAllCodecs(t *testing.T, c codec.Codec) {
	data, _, _, err := fixture.BuildStream(fixture.StreamSpec{
		Codec: c, Seed: 11, Frames: 24, KeyEvery: 8, ExtraSize: 64,
	}, fixture.Fault{})
	if err != nil {
		t.Fatal(err)
	}
	aus, meta, err := codec.ParseBytes(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ws := &writeSeekerBuffer{buf: &buf}
	man, err := mux.Write(ws, aus, meta)
	if err != nil {
		t.Fatalf("mux: %v", err)
	}
	if man.Samples != 24 || man.Codec != c.String() || !man.PayloadOnly {
		t.Fatalf("manifest wrong: %+v", man)
	}
	// The output must be independently parseable with go-mp4.
	found := map[string]bool{}
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		found[h.BoxInfo.Type.String()] = true
		// Container boxes must be expanded explicitly to visit children.
		if h.BoxInfo.Type == mp4.BoxTypeMoov() || h.BoxInfo.Type == mp4.BoxTypeTrak() ||
			h.BoxInfo.Type == mp4.BoxTypeMdia() || h.BoxInfo.Type == mp4.BoxTypeMinf() ||
			h.BoxInfo.Type == mp4.BoxTypeStbl() || h.BoxInfo.Type == mp4.BoxTypeStsd() {
			return h.Expand()
		}
		return nil, nil
	}
	if _, err = mp4.ReadBoxStructure(bytes.NewReader(buf.Bytes()), handler); err != nil {
		t.Fatalf("output MP4 not parseable: %v", err)
	}
	for _, box := range []string{"ftyp", "moov", "mdat", "stsd", "stsz", "co64", "stts"} {
		if !found[box] {
			t.Fatalf("missing box %s in output", box)
		}
	}
	verifyPayloadIntegrity(t, aus, buf.Bytes())
}

func TestMuxAllCodecs(t *testing.T) {
	for _, c := range []codec.Codec{codec.H264, codec.H265, codec.AV1} {
		c := c
		t.Run(c.String(), func(t *testing.T) { muxAllCodecs(t, c) })
	}
}

// verifyPayloadIntegrity walks mdat and confirms every length-prefixed NAL
// matches a unit in the source access units (lossless payload preservation).
func verifyPayloadIntegrity(t *testing.T, aus []codec.AccessUnit, mp4Bytes []byte) {
	t.Helper()
	r := bytes.NewReader(mp4Bytes)
	var mdatPayload []byte
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		inf := h.BoxInfo
		if inf.Type == mp4.BoxTypeMdat() {
			buf := make([]byte, inf.Size-8)
			if _, err := r.ReadAt(buf, int64(inf.Offset+8)); err != nil {
				return nil, err
			}
			mdatPayload = buf
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the expected sample bytes from source units.
	var expected []byte
	for _, au := range aus {
		for _, n := range au.NALs {
			body := n.Raw
			if len(body) >= 8 && bytes.HasPrefix(body[len(body)-8:], []byte("MPK1")) {
				body = body[:len(body)-8]
			}
			var lb [4]byte
			binary.BigEndian.PutUint32(lb[:], uint32(len(body)))
			expected = append(expected, lb[:]...)
			expected = append(expected, body...)
		}
	}
	if !bytes.Equal(expected, mdatPayload) {
		t.Fatalf("mdat payload diverges from source: %d vs %d bytes",
			len(expected), len(mdatPayload))
	}
}

func TestEmptyInputRejected(t *testing.T) {
	var buf bytes.Buffer
	if _, err := mux.Write(&writeSeekerBuffer{buf: &buf}, nil,
		&codec.StreamMeta{Codec: codec.H264}); err == nil {
		t.Fatal("empty sample set must be rejected")
	}
}

// writeSeekerBuffer adapts bytes.Buffer to io.WriteSeeker.
type writeSeekerBuffer struct {
	buf *bytes.Buffer
	pos int64
}

func (w *writeSeekerBuffer) Write(p []byte) (int, error) {
	end := int(w.pos) + len(p)
	if end > w.buf.Len() {
		grow := make([]byte, end-w.buf.Len())
		w.buf.Write(grow)
	}
	copy(w.buf.Bytes()[w.pos:end], p)
	w.pos = int64(end)
	return len(p), nil
}

func (w *writeSeekerBuffer) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = int64(w.buf.Len()) + offset
	}
	return w.pos, nil
}
