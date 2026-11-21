package ingest

import (
	"bytes"
	"os"
	"testing"

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
	if video.Kind != model.KindVideo || len(video.Samples) != 48 {
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
	if !video.ContainerCrop.Valid || video.ContainerCrop.W != 632 {
		t.Fatalf("crop not preserved: %+v", video.ContainerCrop)
	}
	if video.ContainerColor.Primaries != "bt709" {
		t.Fatalf("colour=%+v", video.ContainerColor)
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
	path := b.Dir + "/camera_corrupt.mxf"
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
