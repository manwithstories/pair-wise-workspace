package sample

import (
	"bytes"
	"testing"

	mp4insp "mediad/internal/mp4"
	"mediad/internal/wav"
)

func TestDeterministicSameSeed(t *testing.T) {
	o := Options{Seed: 42, Packets: 40, LossBurst: 5, LossAt: 12, Redundancy: 4, DupConflict: true}
	a := Generate(o)
	b := Generate(o)
	if !bytes.Equal(a.WAV, b.WAV) {
		t.Fatal("WAV output must be deterministic for the same seed")
	}
	if len(a.Packets) != len(b.Packets) {
		t.Fatalf("packet count differs: %d vs %d", len(a.Packets), len(b.Packets))
	}
	for i := range a.Packets {
		pa, pb := a.Packets[i], b.Packets[i]
		if pa.SequenceNumber != pb.SequenceNumber || pa.Timestamp != pb.Timestamp ||
			!bytes.Equal(pa.Payload, pb.Payload) {
			t.Fatalf("packet %d differs across runs", i)
		}
	}
	if !bytes.Equal(a.Redundant[a.LostSeqs[0]], b.Redundant[b.LostSeqs[0]]) {
		t.Fatal("redundancy must be deterministic")
	}
	// 损坏区段摘要也应稳定。
	if summary(a) != summary(b) {
		t.Fatalf("corrupt summary differs: %q vs %q", summary(a), summary(b))
	}
}

func TestDifferentSeedDiffers(t *testing.T) {
	a := Generate(Options{Seed: 1, Packets: 20})
	b := Generate(Options{Seed: 2, Packets: 20})
	if bytes.Equal(a.WAV, b.WAV) {
		t.Fatal("different seeds should produce different PCM")
	}
}

// summary 生成与具体载荷无关的结构性摘要（序号+时间戳+丢失集合）。
func summary(b *Bundle) string {
	var buf bytes.Buffer
	for _, p := range b.Packets {
		buf.WriteByte(byte(p.SequenceNumber >> 8))
		buf.WriteByte(byte(p.SequenceNumber))
		buf.WriteByte(byte(p.Timestamp >> 24))
		buf.WriteByte(byte(p.Timestamp))
	}
	for _, s := range b.LostSeqs {
		buf.WriteByte(byte(s))
	}
	return buf.String()
}

func TestExpectedFrameCount(t *testing.T) {
	const packets = 25
	b := Generate(Options{Seed: 7, Packets: packets})
	if b.ExpectFrames != packets*FramesPerPacket {
		t.Fatalf("expect frames=%d", b.ExpectFrames)
	}
	// 无丢失时 WAV 应能被流式解析出同样数量的完整帧。
	p := wav.NewParser(bytes.NewReader(b.WAV))
	if err := p.ParseHeader(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NextChunk(); err != nil { // fmt
		t.Fatal(err)
	}
	if _, err := p.NextChunk(); err != nil { // data
		t.Fatal(err)
	}
	var frames uint64
	for {
		blk, err := p.ReadData(1 << 20)
		frames += blk.FrameCount
		if err != nil {
			break
		}
	}
	if frames != uint64(b.ExpectFrames) {
		t.Fatalf("parsed frames=%d want %d", frames, b.ExpectFrames)
	}
}

func TestSequenceCoverage(t *testing.T) {
	const packets = 30
	o := Options{Seed: 3, Packets: packets, LossBurst: 4, LossAt: 10, Redundancy: 2}
	b := Generate(o)
	// 期望序号覆盖连续区间 [0, packets)。
	if len(b.ExpectSeqs) != packets {
		t.Fatalf("expect seqs=%d", len(b.ExpectSeqs))
	}
	for i, s := range b.ExpectSeqs {
		if s != uint16(i) {
			t.Fatalf("expect seq gap at %d: got %d", i, s)
		}
	}
	// 实际到达序号（去掉冲突副本）应恰好缺少 LostSeqs。
	seen := map[uint16]bool{}
	for _, p := range b.Packets {
		seen[p.SequenceNumber] = true
	}
	wantArrived := packets - len(b.LostSeqs)
	if len(seen) != wantArrived {
		t.Fatalf("distinct arrived=%d want %d", len(seen), wantArrived)
	}
	for _, s := range b.LostSeqs {
		if seen[s] {
			t.Fatalf("lost seq %d unexpectedly arrived", s)
		}
	}
	// 冗余只覆盖前 Redundancy 个丢失包。
	if len(b.Redundant) != o.Redundancy {
		t.Fatalf("redundancy count=%d want %d", len(b.Redundant), o.Redundancy)
	}
}

func TestTruncatedWAVRejected(t *testing.T) {
	b := Generate(Options{Seed: 5, Packets: 12, TruncateWAV: true})
	p := wav.NewParser(bytes.NewReader(b.WAV))
	if err := p.ParseHeader(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NextChunk(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NextChunk(); err != nil {
		t.Fatal(err)
	}
	var failed bool
	for {
		_, err := p.ReadData(1 << 20)
		if err != nil {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("truncated WAV must surface a parse error")
	}
}

func TestTimestampRollbackAndConflict(t *testing.T) {
	b := Generate(Options{
		Seed: 9, Packets: 16, TimestampBack: true, DupConflict: true,
	})
	// 至少存在一个回退时间戳（小于首包时间戳）。
	var sawRollback bool
	firstTS := b.Packets[0].Timestamp
	for _, p := range b.Packets[1:] {
		if p.Timestamp+PacketSamples <= firstTS {
			sawRollback = true
		}
	}
	if !sawRollback {
		t.Fatal("expected a timestamp rollback packet")
	}
	// 冲突副本：同一序号出现两种不同载荷。
	payloads := map[uint16][][]byte{}
	for _, p := range b.Packets {
		payloads[p.SequenceNumber] = append(payloads[p.SequenceNumber], p.Payload)
	}
	var conflict bool
	for _, pls := range payloads {
		if len(pls) > 1 && !bytes.Equal(pls[0], pls[1]) {
			conflict = true
		}
	}
	if !conflict {
		t.Fatal("expected a same-seq payload conflict")
	}
}

func TestGeneratedFMP4Inspectable(t *testing.T) {
	frags := DefaultVideoFragments(40)
	data := BuildVideoFMP4(frags)
	rep, err := (&mp4insp.Inspector{Timescale: VideoRate}).Inspect(Reader(data))
	if err != nil {
		t.Fatalf("inspect generated fMP4: %v", err)
	}
	if len(rep.Fragments) != 40 {
		t.Fatalf("fragments=%d", len(rep.Fragments))
	}
	if len(rep.KeyframeTimes) == 0 {
		t.Fatal("expected keyframes")
	}
	if len(rep.Discontinuities) != 0 {
		t.Fatalf("continuous timeline should have no discontinuities: %+v", rep.Discontinuities)
	}
}
