package mux_test

import (
	"bytes"
	"io"
	"testing"

	"mediapack/internal/codec"
	"mediapack/internal/fixture"
	"mediapack/internal/mux"
)

// memWriteSeeker adapts bytes.Buffer to io.WriteSeeker for in-memory muxing.
type memWriteSeeker struct{ b *bytes.Buffer; p int64 }

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	for int64(m.b.Len()) < m.p+int64(len(p)) {
		m.b.WriteByte(0)
	}
	copy(m.b.Bytes()[m.p:], p)
	m.p += int64(len(p))
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.p = offset
	case io.SeekCurrent:
		m.p += offset
	case io.SeekEnd:
		m.p = int64(m.b.Len()) + offset
	}
	return m.p, nil
}

func TestManifestAndChunking(t *testing.T) {
	data, _, _, err := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H265, Seed: 4, Frames: 40, KeyEvery: 10, ExtraSize: 4096,
	}, fixture.Fault{})
	if err != nil {
		t.Fatal(err)
	}
	aus, meta, err := codec.ParseBytes(data, 2)
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	man, err := mux.Write(&memWriteSeeker{b: buf}, aus, meta)
	if err != nil {
		t.Fatal(err)
	}
	if man.Samples != 40 || man.Keyframes != 4 || man.TrackID != 2 {
		t.Fatalf("manifest counts wrong: %+v", man)
	}
	if man.Timescale != 90000 || man.Duration == 0 || !man.PayloadOnly {
		t.Fatalf("manifest timing/flags wrong: %+v", man)
	}
}
