package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/adapter"
	mp4adapter "mediacheck/internal/adapter/mp4"
	"mediacheck/internal/model"
)

func TestPackRoundTripAndBitrate(t *testing.T) {
	dir := t.TempDir()
	// 2 秒 48k 立体声 16-bit。
	pcm := make([]byte, 48000*2*2*2)
	in := PackInput{
		OutDir: dir, BaseName: "prog", Timescale: 48000,
		TargetKBPS: 6000, FramesPerFrag: 48000, // 1 秒/片
		PCM16: pcm, Channels: 2,
		Color: model.ColorMeta{ColorPrimaries: 1, Transfer: 1, Matrix: 1},
	}
	res, err := Pack(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FragmentSeqs) != 2 {
		t.Fatalf("fragments = %d", len(res.FragmentSeqs))
	}
	// 实际码率必须落在目标 ±1%（free box 填充校准）。
	deltaPct := absInt(res.ActualKBPS-6000) * 100 / 6000
	if deltaPct > 1 {
		t.Fatalf("actual kbps=%d delta=%d%% exceeds 1%%", res.ActualKBPS, deltaPct)
	}
	// 输出分片必须能被 mp4 适配器重新解析（自包含可复核）。
	p := mp4adapter.New()
	initData, err := os.ReadFile(res.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	var frags [][]byte
	for _, f := range res.Files[1:] {
		if filepath.Ext(f) != ".m4s" {
			continue
		}
		d, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		frags = append(frags, d)
	}
	parsed, err := p.Parse(context.Background(), adapter.ParseInput{InitSegment: initData, Fragments: frags})
	if err != nil {
		t.Fatalf("reparse failed: %v", err)
	}
	if len(parsed.Fragments) != 2 {
		t.Fatalf("reparsed fragments = %d", len(parsed.Fragments))
	}
}

func TestPackCleansUpOnBadPath(t *testing.T) {
	// 路径逃逸必须被拒绝，且目录内不应有产物。
	dir := t.TempDir()
	in := PackInput{
		OutDir: dir, BaseName: "../escape", Timescale: 48000,
		TargetKBPS: 6000, FramesPerFrag: 100,
		PCM16: make([]byte, 400), Channels: 2,
	}
	if _, err := Pack(in); err == nil {
		t.Fatal("path escape must be rejected")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("no output may remain on rejection, got %d", len(entries))
	}
}

func TestPackRejectsInvalidParams(t *testing.T) {
	_, err := Pack(PackInput{TargetKBPS: 0, FramesPerFrag: 10, PCM16: []byte{1}, Channels: 2})
	if err == nil {
		t.Fatal("invalid bitrate must fail")
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
