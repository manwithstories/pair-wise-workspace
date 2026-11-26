package sample

import (
	"bytes"
	"testing"

	"github.com/archive/tsrestore/internal/rtp"
)

func TestBuildRenditionDeterministic(t *testing.T) {
	r := DefaultRenditions()[0]
	cfg := DamageConfig{Loss: 0.05, ReorderDist: 5, Duplicates: 0.02}
	a, sa, err := BuildRendition(r, ProgramDuration(30), 41, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, sb, err := BuildRendition(r, ProgramDuration(30), 41, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same seed must produce byte-identical streams")
	}
	if sa != sb {
		t.Fatalf("stats must be deterministic: %+v vs %+v", sa, sb)
	}
	if sa.RTPDatagrams == 0 || sa.SRReports == 0 {
		t.Fatalf("expected RTP datagrams and SR reports, got %+v", sa)
	}
	if sa.Lost == 0 {
		t.Fatal("loss>0 should drop some packets")
	}
}

func TestGeneratedStreamParsesAndIDRsSurvive(t *testing.T) {
	r := DefaultRenditions()[1]
	cfg := DamageConfig{Loss: 0.10, ReorderDist: 9, Duplicates: 0.05}
	stream, st, err := BuildRendition(r, ProgramDuration(60), 7, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reader := rtp.NewOrderedReader(bytes.NewReader(stream))
	var srs int
	reader.OnSR = func(rtp.SenderReport) { srs++ }

	seenIDR := 0
	rtpCount := 0
	for {
		p, err := reader.Next()
		if err != nil {
			break
		}
		rtpCount++
		// Every RTP payload must be whole 188-byte TS packets.
		if len(p.Payload)%TSPacketSize != 0 {
			t.Fatalf("payload not TS-aligned: %d bytes", len(p.Payload))
		}
		if payloadHasIDR(p.Payload) {
			seenIDR++
		}
	}
	if srs != st.SRReports {
		t.Fatalf("parsed SR count %d != generated %d", srs, st.SRReports)
	}
	if rtpCount == 0 {
		t.Fatal("no RTP packets parsed")
	}
	// 60 s = 1500 frames; an IDR every 50 frames at indices 0..1450 yields 30
	// IDR frames. Loss never targets IDR-bearing datagrams, so one PES packet
	// per IDR frame must survive and be observed.
	wantIDRFrames := 60 / 2
	if seenIDR < wantIDRFrames {
		t.Fatalf("surviving IDR packets %d < IDR frames %d", seenIDR, wantIDRFrames)
	}
	// Reorder/dup accounting: reader must observe duplicates and gaps, proving
	// the injected damage actually reached the wire.
	rs := reader.Stats()
	if rs.Duplicates == 0 {
		t.Fatal("injected duplicates were not observed")
	}
	if rs.Gaps == 0 && st.Lost > 0 {
		t.Fatalf("loss (%d packets) produced no gaps: %+v", st.Lost, rs)
	}
}

func TestNoDamageYieldsPerfectOrder(t *testing.T) {
	r := DefaultRenditions()[0]
	stream, _, err := BuildRendition(r, ProgramDuration(12), 1, DamageConfig{})
	if err != nil {
		t.Fatal(err)
	}
	reader := rtp.NewOrderedReader(bytes.NewReader(stream))
	var prev uint16
	first := true
	for {
		p, err := reader.Next()
		if err != nil {
			break
		}
		if !first {
			if p.Sequence != prev+1 {
				t.Fatalf("clean stream out of order at %d", p.Sequence)
			}
		}
		prev, first = p.Sequence, false
	}
	if reader.Stats().Gaps != 0 || reader.Stats().Duplicates != 0 {
		t.Fatalf("clean stream must have no gaps/dups: %+v", reader.Stats())
	}
}

// payloadHasIDR scans TS packets for a video-PID PES carrying an IDR NAL type.
func payloadHasIDR(payload []byte) bool {
	for off := 0; off+TSPacketSize <= len(payload); off += TSPacketSize {
		tp := payload[off : off+TSPacketSize]
		if tp[0] != SyncByte {
			continue
		}
		pid := uint16(tp[1]&0x1F)<<8 | uint16(tp[2])
		if pid != PIDVideo || tp[1]&0x40 == 0 {
			continue
		}
		start := 4
		if tp[3]&0x20 != 0 { // adaptation field precedes the payload
			start = 5 + int(tp[4])
		}
		if start+17 < TSPacketSize && tp[start] == 0 && tp[start+1] == 0 &&
			tp[start+2] == 1 && tp[start+3] == 0xE0 && tp[start+17]&0x1F == NALIDR {
			return true
		}
	}
	return false
}
