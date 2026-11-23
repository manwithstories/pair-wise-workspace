package remux

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediasync/internal/model"
)

// memReader serves coded bytes from a flat buffer at sample offsets.
type memReader struct{ data []byte }

func (m *memReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(m.data)) {
		return 0, os.ErrInvalid
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, ioEOF
	}
	return n, nil
}

var ioEOF = &eofError{}

type eofError struct{}

func (*eofError) Error() string { return "EOF" }

func makeTrack(id uint32, payloads [][]byte) (*model.Track, *memReader) {
	var data []byte
	tr := &model.Track{ID: id, Kind: model.KindVideo, Codec: "avc1", Timescale: 30000,
		Width: 1920, Height: 1080,
		Meta: model.DisplayMeta{SourceWidth: 1920, SourceHeight: 1080, Rotation: 90,
			Crop:   model.Crop{Left: 8, Right: 8, Top: 0, Bottom: 0},
			Colour: &model.Colour{Primaries: 1, Transfer: 1, Matrix: 1, Known: true}}}
	for i, p := range payloads {
		tr.Samples = append(tr.Samples, model.Sample{Index: i, Offset: int64(len(data)), Size: len(p), Duration: 3003})
		data = append(data, p...)
	}
	return tr, &memReader{data: data}
}

func TestWriteTrackCopiesBytesVerbatim(t *testing.T) {
	payloads := [][]byte{{0x00, 0x00, 0x00, 0x01, 0xaa}, {0x00, 0x00, 0x00, 0x01, 0xbb, 0xcc}, {0x01}}
	tr, src := makeTrack(1, payloads)
	w, err := NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.WriteTrack(TrackInput{Name: "program-video", Track: tr, Reader: src})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, p := range payloads {
		want += len(p)
	}
	if out.Samples != 3 || out.Bytes != int64(want) {
		t.Fatalf("out = %d samples %d bytes", out.Samples, out.Bytes)
	}
	got, err := os.ReadFile(filepath.Join(w.Dir(), "program-video.essence"))
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, p := range payloads {
		joined = append(joined, p...)
	}
	if !bytes.Equal(got, joined) {
		t.Fatal("essence bytes were altered")
	}
	// Rotation swaps axes after crop.
	if out.DisplayW != 1080 || out.DisplayH != 1904 {
		t.Fatalf("display = %dx%d", out.DisplayW, out.DisplayH)
	}
}

func TestManifestHashStableAcrossRuns(t *testing.T) {
	payloads := [][]byte{{1, 2, 3}, {4, 5}}
	var shas []string
	for run := 0; run < 2; run++ {
		tr, src := makeTrack(1, payloads)
		w, err := NewWriter(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		out, err := w.WriteTrack(TrackInput{Name: "program-video", Track: tr, Reader: src})
		if err != nil {
			t.Fatal(err)
		}
		sha, err := w.WriteManifest(Result{Tracks: []TrackOutput{out}, Samples: out.Samples, Bytes: out.Bytes})
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	if shas[0] != shas[1] {
		t.Fatal("package hash differs between identical runs")
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	tr, src := makeTrack(1, [][]byte{{1, 2, 3}})
	w, err := NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.WriteTrack(TrackInput{Name: "program-video", Track: tr, Reader: src})
	if err != nil {
		t.Fatal(err)
	}
	res := Result{Tracks: []TrackOutput{out}}
	if err := w.Verify(res); err != nil {
		t.Fatalf("verify clean: %v", err)
	}
	// Corrupt one byte.
	path := filepath.Join(w.Dir(), "program-video.essence")
	data, _ := os.ReadFile(path)
	data[0] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Verify(res); err == nil {
		t.Fatal("verify must detect tampering")
	}
}

func TestPathEscapeAndBadNames(t *testing.T) {
	if _, err := NewWriter("../escape"); err == nil || model.CodeOf(err) != model.ErrPathEscape {
		t.Fatalf("err = %v", err)
	}
	w, err := NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr, src := makeTrack(1, [][]byte{{1}})
	for _, name := range []string{"", "../x", "a/b", ".", ".."} {
		if _, err := w.WriteTrack(TrackInput{Name: name, Track: tr, Reader: src}); err == nil {
			t.Fatalf("name %q should be rejected", name)
		}
	}
}

func TestShortReadRejected(t *testing.T) {
	tr := &model.Track{ID: 1, Kind: model.KindVideo, Samples: []model.Sample{{Index: 0, Offset: 0, Size: 16}}}
	src := &memReader{data: []byte{1, 2, 3}}
	w, err := NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteTrack(TrackInput{Name: "v", Track: tr, Reader: src}); err == nil {
		t.Fatal("short read should fail")
	}
}

func TestManifestJSONShape(t *testing.T) {
	tr, src := makeTrack(1, [][]byte{{9}})
	w, err := NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.WriteTrack(TrackInput{Name: "program-video", Track: tr, Reader: src})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteManifest(Result{Tracks: []TrackOutput{out}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(w.Dir(), "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Result
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Tracks) != 1 || decoded.Tracks[0].SHA256 == "" {
		t.Fatalf("manifest = %+v", decoded)
	}
	if decoded.PackageSHA == "" {
		t.Fatal("package hash missing")
	}
}
