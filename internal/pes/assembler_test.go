package pes

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeDecodePTS(t *testing.T) {
	values := []int64{0, 1, 90000, 1 << 32, (1 << 33) - 2}
	for _, v := range values {
		raw := Encode(StreamIDJPEG, v, []byte("frame-bytes"))
		pkt, _, err := parseHeader(raw)
		if err != nil {
			t.Fatalf("pts=%d parse: %v", v, err)
		}
		if pkt.PTS90k != v {
			t.Fatalf("PTS round trip: got=%d want=%d", pkt.PTS90k, v)
		}
		if !bytes.Equal(pkt.Data, []byte("frame-bytes")) {
			t.Fatalf("data mismatch: %q", pkt.Data)
		}
		if pkt.StreamID != StreamIDJPEG {
			t.Fatalf("stream id=%x", pkt.StreamID)
		}
	}
}

func TestNoPTS(t *testing.T) {
	raw := Encode(StreamIDPCM, -1, []byte{1, 2, 3})
	pkt, _, err := parseHeader(raw)
	if err != nil || pkt.PTS90k != -1 {
		t.Fatalf("no-PTS: pts=%d err=%v", pkt.PTS90k, err)
	}
}

func TestStartCodeRejected(t *testing.T) {
	raw := Encode(StreamIDPCM, 100, []byte("x"))
	raw[2] = 0x02
	if _, _, err := parseHeader(raw); !errors.Is(err, ErrStartCode) {
		t.Fatalf("want ErrStartCode, got %v", err)
	}
}

// chunk 模拟把 PES 负载切成若干 TS 包大小的块。
func chunk(pes []byte, size int) [][]byte {
	var out [][]byte
	for len(pes) > 0 {
		n := min(size, len(pes))
		out = append(out, pes[:n])
		pes = pes[n:]
	}
	return out
}

func TestAssemblerMultiPacket(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 500)
	raw := Encode(StreamIDJPEG, 270_000, data)
	a := NewAssembler()
	pieces := chunk(raw, 100)
	var got *Packet
	for i, p := range pieces {
		pkt, err := a.Push(0x100, p, i == 0, 0, false)
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		if pkt != nil {
			got = pkt
		}
	}
	if got == nil {
		t.Fatal("PES never completed")
	}
	if !bytes.Equal(got.Data, data) || got.PTS90k != 270_000 {
		t.Fatalf("reassembled PES wrong: pts=%d dataLen=%d", got.PTS90k, len(got.Data))
	}
}

func TestGapMarksUntrusted(t *testing.T) {
	data := bytes.Repeat([]byte{0x01}, 400)
	raw := Encode(StreamIDJPEG, 90_000, data)
	a := NewAssembler()
	first, rest := raw[:120], raw[120:]
	if _, err := a.Push(0x100, first, true, 0, false); err != nil {
		t.Fatal(err)
	}
	// 中间丢了 2 个包：gap=2。
	pkt, err := a.Push(0x100, rest, false, 2, false)
	if err != nil {
		t.Fatalf("gap push returned err=%v", err)
	}
	if pkt == nil || !pkt.Untrusted {
		t.Fatalf("gap must propagate Untrusted: %+v", pkt)
	}
}

func TestDuplicateIgnored(t *testing.T) {
	raw := Encode(StreamIDPCM, 10, []byte("audio"))
	a := NewAssembler()
	pkt, _ := a.Push(0x101, raw, true, 0, false)
	if pkt == nil {
		t.Fatal("single-packet PES should complete immediately")
	}
	// 重复推送同一包，不应产生第二个 PES，也不应污染状态。
	pkt2, err := a.Push(0x101, raw, true, 0, true)
	if err != nil || pkt2 != nil {
		t.Fatalf("duplicate must be ignored: pkt=%v err=%v", pkt2, err)
	}
}

func TestTruncatedFlush(t *testing.T) {
	raw := Encode(StreamIDJPEG, 100, bytes.Repeat([]byte{0x02}, 300))
	a := NewAssembler()
	if _, err := a.Push(0x100, raw[:120], true, 0, false); err != nil {
		t.Fatal(err)
	}
	pkt := a.Flush(0x100)
	if pkt == nil || !pkt.Truncated {
		t.Fatalf("flush must report truncated: %+v", pkt)
	}
	if a.Flush(0x100) != nil {
		t.Fatal("second flush must be empty after reset")
	}
}

func TestOversizedPESRejected(t *testing.T) {
	a := NewAssembler()
	// 声明一个较大但 16 位可表达的长度，实际送入字节先冲破 MaxPESLength。
	huge := make([]byte, MaxPESLength+10)
	huge[0], huge[1], huge[2] = 0, 0, 1
	huge[4], huge[5] = 0xFF, 0xFF // 声明 65535，缓冲实际更大
	if _, err := a.Push(0x100, huge, true, 0, false); !errors.Is(err, ErrPESLength) {
		t.Fatalf("want ErrPESLength, got %v", err)
	}
}
