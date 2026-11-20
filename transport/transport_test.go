package transport

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/pion/rtp"

	"mediamux/media"
)

func rtpBytes(seq uint16, ts uint32, payload []byte) []byte {
	p := rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 7},
		Payload: payload,
	}
	b, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

func frame(pkts ...[]byte) []byte {
	var buf bytes.Buffer
	for _, p := range pkts {
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))
		buf.Write(hdr[:])
		buf.Write(p)
	}
	return buf.Bytes()
}

func TestReaderValidAndOffsets(t *testing.T) {
	data := frame(rtpBytes(1, 900, []byte("aa")), rtpBytes(2, 1800, []byte("bbb")))
	rd := NewReader(bytes.NewReader(data), 1500)
	p1, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	if p1.Seq != 1 || p1.Timestamp != 900 || p1.Offset != 0 || string(p1.Payload) != "aa" {
		t.Fatalf("p1 = %+v", p1)
	}
	p2, err := rd.Next()
	if err != nil {
		t.Fatal(err)
	}
	wantOff := int64(2 + len(rtpBytes(1, 900, []byte("aa"))))
	if p2.Offset != wantOff {
		t.Fatalf("p2 offset = %d want %d", p2.Offset, wantOff)
	}
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("expect EOF, got %v", err)
	}
}

func TestReaderRecoverableAndTruncated(t *testing.T) {
	good := rtpBytes(1, 900, []byte("ok"))
	badLen := []byte{0x00, 0x05, 1, 2, 3, 4, 5} // 长度 5 < 12，可恢复跳过
	notRTP := append([]byte{0x00, 0x0C}, bytes.Repeat([]byte{0xFF}, 12)...) // 非法 RTP
	trunc := []byte{0x00, 0x40, 1, 2}                                        // 包体截断，不可恢复
	data := concat(badLen, frame(good), notRTP, frame(good), trunc)
	rd := NewReader(bytes.NewReader(data), 1500)

	if _, err := rd.Next(); err == nil || !media.IsRecoverable(err) {
		t.Fatalf("bad length should be recoverable: %v", err)
	}
	if _, err := rd.Next(); err != nil {
		t.Fatalf("good packet after skip: %v", err)
	}
	if _, err := rd.Next(); err == nil || !media.IsRecoverable(err) {
		t.Fatalf("bad RTP should be recoverable: %v", err)
	}
	if _, err := rd.Next(); err != nil {
		t.Fatalf("second good packet: %v", err)
	}
	if _, err := rd.Next(); err == nil || media.IsRecoverable(err) {
		t.Fatalf("truncated body should be unrecoverable: %v", err)
	}
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func mkPkt(seq uint16, ts uint32) *Packet {
	return &Packet{Seq: seq, Timestamp: ts, SSRC: 7, Payload: []byte("x")}
}

func drainSeqs(ds []Delivery) (pkts []uint16, gaps int, recovered int) {
	for _, d := range ds {
		if d.Gap {
			gaps++
			continue
		}
		pkts = append(pkts, d.Pkt.Seq)
		if d.Recovered {
			recovered++
		}
	}
	return
}

func TestJitterReorderDupLoss(t *testing.T) {
	jb := NewJitterBuffer(64, 90000, media.MillisToTicks(50), media.MillisToTicks(400))
	at := func(ms int64) media.Ticks { return media.MillisToTicks(ms) }

	// 0,2 先到，1 乱序后至；3 丢失；4,5 到达；4 重复
	jb.Push(mkPkt(0, 0), at(0))
	jb.Push(mkPkt(2, 1800), at(5))
	if got := jb.Push(mkPkt(1, 900), at(10)); got != ClassOK {
		t.Fatalf("reorder class = %v", got)
	}
	jb.Push(mkPkt(4, 3600), at(15))
	jb.Push(mkPkt(5, 4500), at(20))
	if got := jb.Push(mkPkt(4, 3600), at(21)); got != ClassDup {
		t.Fatalf("dup class = %v", got)
	}
	// 窗口内：0,1,2 按序交付，1、2 均乱序到达标记恢复；3 未超时暂不判失
	pkts, gaps, rec := drainSeqs(jb.Drain(at(30)))
	if len(pkts) != 3 || pkts[0] != 0 || pkts[1] != 1 || pkts[2] != 2 {
		t.Fatalf("pkts = %v", pkts)
	}
	if gaps != 0 || rec != 2 {
		t.Fatalf("gaps=%d rec=%d", gaps, rec)
	}
	// 超时后 3 判失，4、5 交付
	pkts, gaps, _ = drainSeqs(jb.Drain(at(500)))
	if gaps != 1 || len(pkts) != 2 || pkts[0] != 4 || pkts[1] != 5 {
		t.Fatalf("after timeout pkts=%v gaps=%d", pkts, gaps)
	}
	// 迟到的 3：判失后到达为 ClassLate
	if got := jb.Push(mkPkt(3, 2700), at(600)); got != ClassLate {
		t.Fatalf("late class = %v", got)
	}
	st := jb.Stats()
	if st.Received != 7 || st.Duplicate != 1 || st.Late != 1 || st.Lost != 1 || st.Recovered != 4 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestJitterWrapAndFlush(t *testing.T) {
	jb := NewJitterBuffer(64, 90000, media.MillisToTicks(50), media.MillisToTicks(400))
	at := func(ms int64) media.Ticks { return media.MillisToTicks(ms) }
	// 序号回绕：65534, 65535, 0, 1（缺 65535）
	jb.Push(mkPkt(65534, 0), at(0))
	jb.Push(mkPkt(0, 1800), at(5))
	jb.Push(mkPkt(1, 2700), at(6))
	pkts, gaps, _ := drainSeqs(jb.Drain(at(10)))
	if len(pkts) != 1 || pkts[0] != 65534 || gaps != 0 {
		t.Fatalf("first drain pkts=%v gaps=%d", pkts, gaps)
	}
	// 超时后 65535 判失，回绕后的 0、1 按序交付
	pkts, gaps, _ = drainSeqs(jb.Drain(at(500)))
	if len(pkts) != 2 || pkts[0] != 0 || pkts[1] != 1 || gaps != 1 {
		t.Fatalf("wrap pkts=%v gaps=%d", pkts, gaps)
	}
	// Flush 交付剩余缺口
	jb.Push(mkPkt(3, 4500), at(600)) // 缺 2
	_, gaps, _ = drainSeqs(jb.Flush())
	if gaps != 1 {
		t.Fatalf("flush gaps = %d", gaps)
	}
}

func TestJitterCapacityOverflow(t *testing.T) {
	jb := NewJitterBuffer(4, 90000, media.MillisToTicks(50), media.MillisToTicks(400))
	at := func(ms int64) media.Ticks { return media.MillisToTicks(ms) }
	// 1 先到，随后 3..7 到达（缺 2），容量 4：强制交付/判失而非越界缓存
	jb.Push(mkPkt(1, 900), at(0))
	for i := 3; i <= 7; i++ {
		jb.Push(mkPkt(uint16(i), uint32(i*900)), at(int64(i)))
	}
	if pkts, _ := jb.Buffered(); pkts > 4 {
		t.Fatalf("buffered = %d > capacity", pkts)
	}
	st := jb.Stats()
	if st.Lost != 1 {
		t.Fatalf("overflow should force exactly 1 loss, got %+v", st)
	}
	if len(jb.Decisions()) == 0 {
		t.Fatal("overflow decision should be logged")
	}
	// 强制交付随 Drain 返回：1、缺口(2)、3、4、5…
	pkts, gaps, _ := drainSeqs(jb.Drain(at(100)))
	if gaps != 1 || len(pkts) == 0 || pkts[0] != 1 {
		t.Fatalf("forced drain pkts=%v gaps=%d", pkts, gaps)
	}
}

func TestJitterDeterministic(t *testing.T) {
	run := func() Stats {
		jb := NewJitterBuffer(64, 90000, media.MillisToTicks(50), media.MillisToTicks(400))
		at := func(ms int64) media.Ticks { return media.MillisToTicks(ms) }
		for i := 0; i < 100; i++ {
			seq := uint16(i)
			if i%10 == 3 { // 每 10 个丢 1 个
				continue
			}
			jb.Push(mkPkt(seq, uint32(i*900)), at(int64(i*10+i%3)))
			jb.Drain(at(int64(i*10 + i%3)))
		}
		jb.Flush()
		return jb.Stats()
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("non-deterministic: %+v vs %+v", a, b)
	}
}
