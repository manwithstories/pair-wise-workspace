package analyze

import (
	"errors"
	"hash/crc32"
	"testing"

	"bcastqc/internal/ingest"
	"bcastqc/internal/model"
)

func TestAnalyzeStructureFrameMismatch(t *testing.T) {
	info := &ingest.WAVInfo{
		SampleRate: 48000, NumChans: 1, BitsPerSample: 16,
		BlockAlign: 2, TotalFrames: 1000, DataBytes: 2000,
		Chunks: []ingest.ChunkInfo{{ID: "fmt "}, {ID: "data"}},
	}
	rep, err := AnalyzeStructure(0, info, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.FrameConsistent || rep.DeltaFrames != 0 {
		t.Fatalf("等长应一致: %+v", rep)
	}
	rep2, err := AnalyzeStructure(0, info, 900)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.FrameConsistent || rep2.DeltaFrames != 100 {
		t.Fatalf("帧数差异应被标记: %+v", rep2)
	}
}

func TestAnalyzeStructureMissingChunks(t *testing.T) {
	info := &ingest.WAVInfo{Chunks: []ingest.ChunkInfo{{ID: "fmt "}}}
	if _, err := AnalyzeStructure(0, info, 10); !errors.Is(err, model.ErrRiffStructure) {
		t.Fatalf("缺 data 块应报 ErrRiffStructure: %v", err)
	}
}

func mkRec(seq, start, frames int64, crc uint32, anchor bool, payload []byte) ingest.PacketRecord {
	return ingest.PacketRecord{
		Packet: ingest.Packet{
			TrackID: 0, Seq: seq, StartSample: start, Frames: frames,
			CRC: crc, Anchor: anchor, Payload: payload,
		},
	}
}

func TestLocateDamageLossGap(t *testing.T) {
	clock, _ := model.NewClock(48000)
	tl, _ := model.NewTrackTimeline(0, clock, 1000)
	// 包 0 覆盖 [0,100)，包 1 直接覆盖 [200,300)，中间 100 帧丢失。
	recs := []ingest.PacketRecord{
		mkRec(0, 0, 100, 0, true, make([]byte, 200)),
		mkRec(1, 200, 100, 0, false, make([]byte, 200)),
	}
	rep, err := LocateDamage(0, recs, tl, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Segments) != 2 {
		t.Fatalf("应有丢失缺口 + 尾截断共 2 段，实际 %d: %+v", len(rep.Segments), rep.Segments)
	}
	if rep.Segments[0].Start != 100 || rep.Segments[0].End != 200 || rep.Segments[0].Kind != model.GapKindLoss {
		t.Fatalf("丢失缺口定位错误: %+v", rep.Segments[0])
	}
	if rep.MaxLocError > 1 {
		t.Fatalf("定位误差必须 ≤1，实际 %d", rep.MaxLocError)
	}
}

func TestLocateDamageCRCFailure(t *testing.T) {
	clock, _ := model.NewClock(48000)
	tl, _ := model.NewTrackTimeline(0, clock, 300)
	payload := []byte{1, 2, 3, 4}
	goodCRC := crc32.ChecksumIEEE(payload)
	badPayload := []byte{9, 2, 3, 4}
	recs := []ingest.PacketRecord{
		mkRec(0, 0, 100, goodCRC, true, payload),
		mkRec(1, 100, 100, goodCRC, false, badPayload), // 载荷被破坏但 CRC 仍旧
		mkRec(2, 200, 100, goodCRC, false, payload),
	}
	rep, err := LocateDamage(0, recs, tl, 300)
	if err != nil {
		t.Fatal(err)
	}
	foundCRC := false
	for _, seg := range rep.Segments {
		if seg.Kind == model.GapKindCRC && seg.Start == 100 && seg.End == 200 {
			foundCRC = true
		}
	}
	if !foundCRC {
		t.Fatalf("未定位到 CRC 损坏区段: %+v", rep.Segments)
	}
	if len(tl.Gaps) == 0 {
		t.Fatal("CRC 缺口应同步登记到时间线")
	}
}

func TestLocateDamageTruncation(t *testing.T) {
	clock, _ := model.NewClock(48000)
	tl, _ := model.NewTrackTimeline(0, clock, 1000)
	recs := []ingest.PacketRecord{
		mkRec(0, 0, 999, 0, true, make([]byte, 1998)),
	}
	rep, err := LocateDamage(0, recs, tl, 1000)
	if err != nil {
		t.Fatal(err)
	}
	last := rep.Segments[len(rep.Segments)-1]
	if last.Kind != model.GapKindTruncation || last.Start != 999 || last.End != 1000 {
		t.Fatalf("尾截断定位错误: %+v", last)
	}
	if last.LocError != 1 {
		t.Fatalf("单帧截断定位误差应为 1，实际 %d", last.LocError)
	}
	if rep.MaxLocError > 1 {
		t.Fatalf("截断定位误差应 ≤1，实际 %d", rep.MaxLocError)
	}
}

func TestLocateDamageNoGapWhenFullyCovered(t *testing.T) {
	clock, _ := model.NewClock(48000)
	tl, _ := model.NewTrackTimeline(0, clock, 200)
	recs := []ingest.PacketRecord{
		mkRec(0, 0, 100, 0, true, make([]byte, 200)),
		mkRec(1, 100, 100, 0, false, make([]byte, 200)),
	}
	rep, err := LocateDamage(0, recs, tl, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Segments) != 0 || rep.DamagedSamples != 0 {
		t.Fatalf("完整覆盖不应有损坏区段，实际 %+v", rep.Segments)
	}
}

func TestAnchorReportNA(t *testing.T) {
	clock, _ := model.NewClock(48000)
	tl, _ := model.NewTrackTimeline(0, clock, 1000)
	rep := AnalyzeAnchors(tl)
	if rep.Available {
		t.Fatal("无锚点应 N/A")
	}
	tl.AddAnchor(model.Anchor{Sample: 0})
	tl.AddAnchor(model.Anchor{Sample: 480})
	rep = AnalyzeAnchors(tl)
	if !rep.Available || len(rep.Intervals) != 1 {
		t.Fatalf("双锚点应产出 1 个间隔: %+v", rep)
	}
	// 480 采样 / 48000Hz = 10ms。
	if rep.Intervals[0].Ms != 10 {
		t.Fatalf("间隔毫秒数错误: %+v", rep.Intervals[0])
	}
}
