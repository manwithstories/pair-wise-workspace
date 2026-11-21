package ingest

import (
	"bytes"
	"crypto/sha256"
	"os"
	"testing"

	"mediameld/internal/fixture"
	"mediameld/internal/model"
)

func TestDemuxMXFCamera(t *testing.T) {
	b := generateInto(t, t.TempDir())
	f := openFile(t, b.CameraMXF)
	res, err := DemuxMXF(f, b.CameraMXF)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("clean fixture reported %v", res.Err)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks=%d want 3", len(res.Tracks))
	}
	video := res.Tracks[0]
	if video.Kind != model.KindVideo || len(video.Samples) != 72 {
		t.Fatalf("video track kind=%v samples=%d", video.Kind, len(video.Samples))
	}
	// VFR: consecutive frame durations must not all be equal.
	varied := false
	for i := 1; i < len(video.Samples); i++ {
		if video.Samples[i].Duration != video.Samples[0].Duration {
			varied = true
			break
		}
	}
	if !varied {
		t.Fatal("VFR camera samples are all constant duration")
	}
	// Crop and rotation metadata preserved.
	if !video.TrackCrop.Valid || video.TrackCrop.W != 632 {
		t.Fatalf("crop not preserved: %+v", video.TrackCrop)
	}
	if video.TrackColor.Primaries != "bt709" {
		t.Fatalf("colour=%+v", video.TrackColor)
	}
	// Sample hashes must be non-zero and stable across two demuxes.
	f2 := openFile(t, b.CameraMXF)
	res2, _ := DemuxMXF(f2, b.CameraMXF)
	for i := range video.Samples {
		if video.Samples[i].Hash != res2.Tracks[0].Samples[i].Hash {
			t.Fatalf("hash instability at %d", i)
		}
		if video.Samples[i].Hash == ([32]byte{}) {
			t.Fatalf("sample %d not hashed", i)
		}
	}
}

func TestDemuxMXFCorruptTailIsolates(t *testing.T) {
	b := generateInto(t, t.TempDir())
	if err := fixture.WriteCorrupt(b); err != nil {
		t.Fatal(err)
	}
	path := fixture.CorruptPath(b)
	f := openFile(t, path)
	res, err := DemuxMXF(f, path)
	if err != nil {
		t.Fatalf("corrupt tail must not be fatal: %v", err)
	}
	if res.Err == nil {
		t.Fatal("corrupt tail not reported")
	}
	code, ok := model.CodeOf(res.Err)
	if !ok || code != model.CodeKLVLength {
		t.Fatalf("code=%v ok=%v err=%v", code, ok, res.Err)
	}
	if model.ExitCode(res.Err) != 2 {
		t.Fatalf("exit=%d want 2", model.ExitCode(res.Err))
	}
	if res.Recovered != 2 {
		t.Fatalf("recovered_tracks=%d want 2", res.Recovered)
	}
	if len(res.Tracks) != 2 {
		t.Fatalf("tracks=%d want 2 intact", len(res.Tracks))
	}
	if res.TailOffset == 0 {
		t.Fatal("bad KLV offset not reported")
	}
}

func TestKLVBadBER(t *testing.T) {
	// Key + indefinite-length marker (0x80) must be rejected.
	raw := append(make([]byte, 16), 0x80)
	_, err := DemuxMXF(bytes.NewReader(raw), "bad.mxf")
	if err == nil {
		t.Fatal("indefinite BER accepted")
	}
}

func TestKLVAbsoluteOffset(t *testing.T) {
	data := []byte{}
	data = append(data, fixtureTestKey(0x01)...)
	data = append(data, 0x02, 'h', 'i')
	data = append(data, fixtureTestKey(0x02)...)
	data = append(data, 0x05, 0x00, 0x00) // length 5 but only 2 bytes remain
	f, _ := os.CreateTemp(t.TempDir(), "klv-*.mxf")
	f.Write(data)
	f.Seek(0, 0)
	res, err := DemuxMXF(f, "offset.mxf")
	if err != nil {
		t.Fatalf("fatal: %v", err)
	}
	if res.Err == nil {
		t.Fatal("overrun accepted")
	}
	e, ok := res.Err.(*model.Error)
	if !ok || e.Code != model.CodeKLVLength || !e.HasOffset {
		t.Fatalf("want ERR_KLV_LENGTH with offset, got %v", res.Err)
	}
	if e.Offset != 19 {
		t.Fatalf("offset=%d want 19", e.Offset)
	}
}

func fixtureTestKey(b byte) []byte {
	k := make([]byte, 16)
	k[15] = b
	return k
}

func TestKLVPreservesEverySampleByte(t *testing.T) {
	b := generateInto(t, t.TempDir())
	f := openFile(t, b.CameraMXF)
	res, err := DemuxMXF(f, b.CameraMXF)
	if err != nil {
		t.Fatal(err)
	}
	// Re-open and hash the exact recorded byte range; it must match.
	src := openFile(t, b.CameraMXF)
	for _, tr := range res.Tracks {
		for i, s := range tr.Samples {
			var buf [32]byte
			raw := make([]byte, s.Size)
			if _, err := src.ReadAt(raw, s.SourceOffset); err != nil {
				t.Fatalf("track %s sample %d: %v", tr.ID, i, err)
			}
			buf = sha256Sum(raw)
			if buf != s.Hash {
				t.Fatalf("track %s sample %d offset %d hash mismatch", tr.ID, i, s.SourceOffset)
			}
		}
	}
}

func TestKLVZeroLengthValueAccepted(t *testing.T) {
	// A zero-length KLV value is structurally legal and must not be rejected.
	raw := append(fixtureTestKey(0x01), 0x00)
	res, err := DemuxMXF(bytes.NewReader(raw), "empty.mxf")
	if err != nil {
		t.Fatalf("zero-length value rejected: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
}

func TestKLVLongFormBER(t *testing.T) {
	// Length 200 requires the 0x81 long form and must decode exactly.
	val := make([]byte, 200)
	raw := append(fixtureTestKey(0x01), 0x81, 200)
	raw = append(raw, val...)
	res, err := DemuxMXF(bytes.NewReader(raw), "ber.mxf")
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatalf("long-form BER rejected: %v", res.Err)
	}
}

func TestKLVRejectsOversizedBER(t *testing.T) {
	// Nine length bytes exceed the SMPTE limit of eight.
	raw := append(fixtureTestKey(0x01), 0x89)
	raw = append(raw, make([]byte, 9)...)
	_, err := DemuxMXF(bytes.NewReader(raw), "ber9.mxf")
	if err == nil {
		t.Fatal("9-byte BER length accepted")
	}
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
