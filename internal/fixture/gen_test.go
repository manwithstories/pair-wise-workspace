package fixture

import (
	"os"
	"testing"

	"review/internal/fault"
	"review/internal/fragment"
	"review/internal/manifest"
)

func TestGenerate(t *testing.T) {
	l, err := Generate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Cameras) != 3 {
		t.Fatalf("cameras=%d", len(l.Cameras))
	}
	for _, cam := range l.Cameras {
		d, errs := manifest.ParseDASH(cam.MPDPath)
		if len(errs) != 0 {
			t.Fatalf("%s mpd faults: %v", cam.ID, errs)
		}
		if len(d.Representations) != 4 { // 3 视频 + 1 音频
			t.Fatalf("%s reps=%d", cam.ID, len(d.Representations))
		}
		// 分片可解析、样本数正确
		seg, errs := fragment.ParseSegment(cam.VideoSegs[0][0])
		if len(errs) != 0 {
			t.Fatalf("seg faults: %v", errs)
		}
		if len(seg.Samples) != FramesPerSeg {
			t.Fatalf("samples=%d", len(seg.Samples))
		}
		if _, ferr := seg.ReadData(0); ferr != nil {
			t.Fatal(ferr)
		}
	}
	h, errs := manifest.ParseHLS(l.HLS.M3U8Path)
	if len(errs) != 0 {
		t.Fatalf("hls faults: %v", errs)
	}
	if h.DiscontinuityCount != 2 || len(h.Segments) != 4 {
		t.Fatalf("hls disco=%d segs=%d", h.DiscontinuityCount, len(h.Segments))
	}
}

func TestFaultFixtures(t *testing.T) {
	l, err := Generate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// CRC 损坏夹具
	seg, errs := fragment.ParseSegment(l.CorruptPath)
	if len(errs) != 0 {
		t.Fatalf("parse faults: %v", errs)
	}
	var crcErr *fault.Error
	for i := range seg.Samples {
		if _, ferr := seg.ReadData(i); ferr != nil {
			crcErr = ferr
			break
		}
	}
	if crcErr == nil || crcErr.Kind != fault.KindCRC {
		t.Fatalf("want crc fault, got %v", crcErr)
	}
	// 尾部截断
	p, err := l.ApplyTruncateTail()
	if err != nil {
		t.Fatal(err)
	}
	if _, errs := fragment.ParseSegment(p); fault.First(errs) == nil ||
		fault.First(errs).Kind != fault.KindTruncated {
		t.Fatalf("want truncated, got %v", errs)
	}
	// 索引缺项
	p2, err := l.ApplyDropIndex(2)
	if err != nil {
		t.Fatal(err)
	}
	seg2, errs := fragment.ParseSegment(p2)
	if f := fault.First(errs); f == nil || f.Kind != fault.KindMissingIndex {
		t.Fatalf("want missing_index, got %v", errs)
	}
	if seg2.IndexCount != FramesPerSeg-2 {
		t.Fatalf("index=%d", seg2.IndexCount)
	}
	// 故障应用后文件仍可读（可恢复）
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}
