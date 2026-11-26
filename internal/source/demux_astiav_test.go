package source

import (
	"path/filepath"
	"testing"
)

func TestGenerateProbeAndDemuxSyntheticMP4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.mp4")
	err := GenerateSyntheticMP4(SyntheticConfig{
		Path: path, Seconds: 1, Width: 160, Height: 90, FPS: 10, BitrateKbps: 300, Seed: 17,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	info, err := Probe(path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(info.Streams) != 1 || info.Streams[0].Width != 160 || info.Streams[0].Height != 90 {
		t.Fatalf("unexpected streams: %+v", info.Streams)
	}
	if info.Boxes == nil || !info.Boxes.MoovPresent || !info.Boxes.MoovComplete {
		t.Fatalf("box report missing complete moov: %+v", info.Boxes)
	}
	res, err := Demux(path)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if len(res.Packets) < 5 {
		t.Fatalf("too few packets: %d", len(res.Packets))
	}
	var key int
	for _, p := range res.Packets {
		if p.Key {
			key++
		}
		if p.TimeBase.D == 0 {
			t.Fatal("packet missing time base")
		}
	}
	if key == 0 {
		t.Fatal("expected generated keyframes")
	}
}
