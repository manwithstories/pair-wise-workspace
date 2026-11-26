package ts

import (
	"bytes"
	"errors"
	"testing"
)

func TestDetectAndSplit188And204(t *testing.T) {
	raw := make([]byte, PacketSize188*3)
	for i := range raw {
		if i%PacketSize188 == 0 {
			raw[i] = SyncByte
		}
	}
	size, err := DetectSize(raw)
	if err != nil || size != PacketSize188 {
		t.Fatalf("detect 188: size=%d err=%v", size, err)
	}
	pkts, off, err := SplitPackets(raw)
	if err != nil || off != 0 || len(pkts) != 3 {
		t.Fatalf("split 188: n=%d off=%d err=%v", len(pkts), off, err)
	}
	// 204 字节：每包追加 16 字节 RS 尾。
	raw204 := make([]byte, PacketSize204*2)
	for i := 0; i < 2; i++ {
		raw204[i*PacketSize204] = SyncByte
	}
	size, err = DetectSize(raw204)
	if err != nil || size != PacketSize204 {
		t.Fatalf("detect 204: size=%d err=%v", size, err)
	}
	pkts, _, err = SplitPackets(raw204)
	if err != nil || len(pkts) != 2 || len(pkts[0]) != PacketSize188 {
		t.Fatalf("split 204: n=%d err=%v", len(pkts), err)
	}
	// 非整数倍长度必须拒绝（ErrSize 或后续同步失败的 ErrSync 均属畸形）。
	if _, _, err := SplitPackets(make([]byte, PacketSize188+7)); err == nil {
		t.Fatal("malformed length must be rejected")
	}
}

func TestSyncRecovery(t *testing.T) {
	garbage := []byte{0x00, 0x11, 0x22}
	raw := make([]byte, PacketSize188)
	raw[0] = SyncByte
	buf := append(garbage, raw...)
	pkts, off, err := SplitPackets(buf)
	if err != nil || off != 3 || len(pkts) != 1 {
		t.Fatalf("resync: off=%d n=%d err=%v", off, len(pkts), err)
	}
	if _, _, err := SplitPackets(garbage); !errors.Is(err, ErrSync) {
		t.Fatalf("want ErrSync, got %v", err)
	}
}

func TestPCRRoundTrip(t *testing.T) {
	values := []int64{0, 1, 299, 300, 8_589_934_590, 1 << 35}
	for _, v := range values {
		raw := make([]byte, PacketSize188)
		raw[0] = SyncByte
		raw[3] = 0x20 // adaptation only
		raw[4] = 7    // adaptation_field_length
		raw[5] = 0x10 // PCR_flag
		EncodePCR(raw, 5, v)
		p, err := ParsePacket(raw, 0)
		if err != nil {
			t.Fatalf("parse pcr=%d: %v", v, err)
		}
		if p.PCR != v {
			t.Fatalf("PCR round trip: got=%d want=%d", p.PCR, v)
		}
	}
}

func TestCCTracker(t *testing.T) {
	c := NewCCTracker()
	mk := func(pid uint16, cc uint8, order int) *Packet {
		return &Packet{PID: pid, CC: cc, HasPayload: true, Order: order}
	}
	// 首包建立基线。
	if ev := c.Observe(mk(0x100, 3, 0)); ev.Gap != 0 {
		t.Fatalf("baseline gap=%d", ev.Gap)
	}
	// 正常递增。
	if ev := c.Observe(mk(0x100, 4, 1)); ev.Gap != 0 || ev.Duplicate {
		t.Fatalf("sequential event wrong: %+v", ev)
	}
	// 重复包（CC 停在 4）。
	if ev := c.Observe(mk(0x100, 4, 2)); !ev.Duplicate {
		t.Fatalf("duplicate not detected: %+v", ev)
	}
	// 跳变 4 -> 8，缺口 3（缺 5/6/7 三个包）。
	if ev := c.Observe(mk(0x100, 8, 3)); ev.Gap != 3 {
		t.Fatalf("gap=%d want 3", ev.Gap)
	}
	// 回绕 15 -> 0。
	c.Observe(mk(0x100, 15, 4))
	if ev := c.Observe(mk(0x100, 0, 5)); ev.Gap != 0 {
		t.Fatalf("wrap gap=%d want 0", ev.Gap)
	}
	// 乱序。
	if ev := c.Observe(mk(0x100, 1, 2)); !ev.Reversed {
		t.Fatalf("reorder not detected: %+v", ev)
	}
	// 空包不影响状态。
	before := c.Observe(mk(NullPID, 9, 6))
	if before.Duplicate || before.Gap != 0 {
		t.Fatalf("null packet must be ignored: %+v", before)
	}
}

func TestOversizedAdaptationRejected(t *testing.T) {
	raw := make([]byte, PacketSize188)
	raw[0] = SyncByte
	raw[3] = 0x20
	raw[4] = 200 // 声明长度越过包尾
	if _, err := ParsePacket(raw, 0); !errors.Is(err, ErrLength) {
		t.Fatalf("want ErrLength, got %v", err)
	}
}

func TestPATPMTRoundTrip(t *testing.T) {
	progs := []PATProgram{{ProgramNumber: 1, PMTPID: 0x1000}}
	pat := BuildPAT(0x04D2, 7, progs)
	sec, err := parseSection(pat)
	if err != nil {
		t.Fatalf("PAT parse: %v", err)
	}
	got, err := ParsePAT(sec)
	if err != nil || len(got) != 1 || got[0] != progs[0] {
		t.Fatalf("PAT round trip: %+v err=%v", got, err)
	}
	if sec.Version != 7 || !sec.Current {
		t.Fatalf("PAT version/current: v=%d cur=%v", sec.Version, sec.Current)
	}
	streams := []PMTStream{
		{StreamType: StreamTypeJPEG, PID: 0x0100},
		{StreamType: StreamTypePCM, PID: 0x0101},
	}
	pmtBytes := BuildPMT(1, 9, 0x0100, streams)
	pmtSec, err := parseSection(pmtBytes)
	if err != nil {
		t.Fatalf("PMT parse: %v", err)
	}
	pmt, err := ParsePMT(pmtSec)
	if err != nil {
		t.Fatalf("PMT decode: %v", err)
	}
	if pmt.Version != 9 || pmt.PCRPID != 0x0100 || len(pmt.Streams) != 2 {
		t.Fatalf("PMT fields wrong: %+v", pmt)
	}
	if pid, ok := pmt.FindStream(StreamTypePCM); !ok || pid != 0x0101 {
		t.Fatalf("FindStream PCM: pid=%d ok=%v", pid, ok)
	}
}

func TestCRCCorruptionRejected(t *testing.T) {
	pat := BuildPAT(1, 0, []PATProgram{{1, 0x1000}})
	bad := bytes.Clone(pat)
	bad[10] ^= 0xFF // 破坏负载字节，CRC 必然失配
	if _, err := parseSection(bad); !errors.Is(err, ErrCRC) {
		t.Fatalf("want ErrCRC, got %v", err)
	}
}

func TestSectionAssemblerMultiPacket(t *testing.T) {
	// 构造足够长的 PAT（多条映射），使节必须跨 TS 包拼接。
	progs := make([]PATProgram, 40)
	for i := range progs {
		progs[i] = PATProgram{ProgramNumber: uint16(i + 1), PMTPID: uint16(0x1000 + i)}
	}
	pat := BuildPAT(1, 3, progs)
	if len(pat) <= 100 {
		t.Fatalf("test fixture too short: %d", len(pat))
	}
	a := NewSectionAssembler()
	// 首包：pointer_field=0 + 前 100 字节。
	first := append([]byte{0x00}, pat[:100]...)
	done, err := a.Push(first)
	if err != nil || len(done) != 0 {
		t.Fatalf("first push done=%d err=%v", len(done), err)
	}
	rest := pat[100:]
	// 续包分两次到达。
	done, err = a.Push(rest[:len(rest)/2])
	if err != nil || len(done) != 0 {
		t.Fatalf("continuation done=%d err=%v", len(done), err)
	}
	done, err = a.Push(rest[len(rest)/2:])
	if err != nil || len(done) != 1 {
		t.Fatalf("final push done=%d err=%v", len(done), err)
	}
	if !bytes.Equal(done[0], pat) {
		t.Fatal("reassembled section mismatch")
	}
	sec, err := parseSection(done[0])
	if err != nil || sec.Version != 3 {
		t.Fatalf("reassembled parse: v=%d err=%v", sec.Version, err)
	}
	got, err := ParsePAT(sec)
	if err != nil || len(got) != 40 {
		t.Fatalf("reassembled PAT: n=%d err=%v", len(got), err)
	}
}

func TestSectionLengthCap(t *testing.T) {
	a := NewSectionAssembler()
	huge := make([]byte, MaxSectionBuffer+10)
	huge[0] = 0x00
	huge[1] = 0x0F // section_length 高位拉满
	if _, err := a.Push(huge); !errors.Is(err, ErrLength) {
		t.Fatalf("want ErrLength, got %v", err)
	}
}
