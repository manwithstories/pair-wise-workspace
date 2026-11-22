package transport

import (
	"testing"

	"github.com/pion/rtp"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

func pkt(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts},
		Payload: []byte{byte(seq), byte(seq >> 8)}}
}

func feedAll(r *Reorderer, pkts ...*rtp.Packet) ([]Unit, error) {
	var out []Unit
	for _, p := range pkts {
		us, err := r.Feed(p)
		if err != nil {
			return out, err
		}
		out = append(out, us...)
	}
	us, err := r.Flush()
	out = append(out, us...)
	return out, err
}

func TestOrderedPassthrough(t *testing.T) {
	r := New(8, 48000, 960, PolicyFillSilence)
	out, err := feedAll(r, pkt(0, 0), pkt(1, 960), pkt(2, 1920))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].Silence || out[2].Silence {
		t.Fatalf("out=%v", out)
	}
	if out[1].Start != clock.SamplesToTicks(960, 48000) {
		t.Fatalf("start=%d", out[1].Start)
	}
	if r.Stats != (Stats{}) {
		t.Fatalf("stats=%+v", r.Stats)
	}
}

func TestDuplicatesAndReorder(t *testing.T) {
	r := New(8, 48000, 960, PolicyFillSilence)
	// 乱序：0,2,1,3；重复：1 再发一次
	out, err := feedAll(r, pkt(0, 0), pkt(2, 1920), pkt(1, 960), pkt(1, 960), pkt(3, 2880))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("units=%d", len(out))
	}
	for i, u := range out {
		if u.Silence || u.Packet.SequenceNumber != uint16(i) {
			t.Fatalf("unit %d out of order: %+v", i, u)
		}
	}
	if r.Stats.DuplicatePackets != 1 || r.Stats.ReorderedPackets != 2 {
		t.Fatalf("stats=%+v", r.Stats)
	}
}

func TestLossBurstFillSilence(t *testing.T) {
	r := New(2, 48000, 960, PolicyFillSilence)
	// 丢失 1,2,3,4（突发 4 包），随后到 5
	out, err := feedAll(r, pkt(0, 0), pkt(5, 5*960), pkt(6, 6*960))
	if err != nil {
		t.Fatal(err)
	}
	var sil, media int
	for _, u := range out {
		if u.Silence {
			sil++
		} else {
			media++
		}
	}
	if sil != 4 || media != 3 {
		t.Fatalf("sil=%d media=%d", sil, media)
	}
	if r.Stats.GapPackets != 4 || r.Stats.GapEvents != 4 {
		t.Fatalf("stats=%+v", r.Stats)
	}
	// 时间线不压缩：总时长 = 7 个包时长
	var total clock.Ticks
	for _, u := range out {
		total += u.Duration
	}
	if total != 7*clock.SamplesToTicks(960, 48000) {
		t.Fatalf("total=%d", total)
	}
	// 静音位置连续无重叠
	for i := 1; i < len(out); i++ {
		if out[i].Start < out[i-1].Start+out[i-1].Duration {
			t.Fatalf("overlap at %d", i)
		}
	}
}

func TestLossBurstStop(t *testing.T) {
	r := New(2, 48000, 960, PolicyStop)
	_, err := feedAll(r, pkt(0, 0), pkt(5, 5*960))
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindTransport || fe.Recovery != fault.Fail {
		t.Fatalf("err=%v", err)
	}
}

func TestTimestampRollback(t *testing.T) {
	r := New(8, 48000, 960, PolicyFillSilence)
	_, err := feedAll(r, pkt(0, 960), pkt(1, 0)) // 时间戳回退
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindTransport {
		t.Fatalf("err=%v", err)
	}
}
