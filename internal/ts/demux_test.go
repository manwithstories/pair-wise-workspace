package ts

import (
	"testing"

	"tsqc/internal/clock"
)

func TestDemuxExtractsIDRAndAudio(t *testing.T) {
	d := NewDemux()
	var ccPat, ccPmt, ccV, ccA uint8
	var stream [][]byte
	stream = append(stream, sectionPackets(PIDPAT, patSection(0x1000, 0), &ccPat)...)
	stream = append(stream, sectionPackets(0x1000, pmtSection(0x101, 0, [2]uint16{StreamH264, 0x101}, [2]uint16{StreamAAC, 0x102}), &ccPmt)...)

	video := append(pesHeader(0xE0, 90000, 90000, false), nalPayload(0x05, 40)...)
	stream = append(stream, tsPacket(0x101, ccV, true, video, 90000*300, true))
	ccV++
	audio := append(pesHeader(0xC0, 90000, 0, false), []byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x1F, 0xFC}...)
	stream = append(stream, tsPacket(0x102, ccA, true, audio, 0, false))

	aus, err := pump(d, stream)
	if err != nil {
		t.Fatal(err)
	}
	// IDR AU emits when the next PES starts; audio emits on flush.
	aus = append(aus, d.Flush()...)
	if len(aus) != 2 {
		t.Fatalf("aus=%d %+v", len(aus), aus)
	}
	var vid, aud *AccessUnit
	for i := range aus {
		switch aus[i].Kind {
		case KindVideo:
			vid = &aus[i]
		case KindAudio:
			aud = &aus[i]
		}
	}
	if vid == nil || !vid.Keyframe || !vid.HasPTS || vid.PTS != 90000*300 {
		t.Fatalf("video au %+v", vid)
	}
	if aud == nil || aud.HasDTS || aud.PTS != 90000*300 {
		t.Fatalf("audio au %+v", aud)
	}
	if d.Stats().IDRs != 1 || d.Stats().PSIErrors != 0 {
		t.Fatalf("stats %+v", d.Stats())
	}
}

func TestDemuxContinuityJumpMarksPES(t *testing.T) {
	d := NewDemux()
	var ccPat, ccPmt uint8
	d.PushPacket(sectionPackets(PIDPAT, patSection(0x1000, 0), &ccPat)[0])
	d.PushPacket(sectionPackets(0x1000, pmtSection(0x101, 0, [2]uint16{StreamH264, 0x101}), &ccPmt)[0])
	first := append(pesHeader(0xE0, 90000, 90000, false), nalPayload(0x05, 20)...)
	d.PushPacket(tsPacket(0x101, 0, true, first, 0, false))
	// Skip CC 1 -> 3; then a new PES triggers finishing the broken one.
	second := append(pesHeader(0xE0, 180000, 180000, false), nalPayload(0x01, 20)...)
	aus, err := d.PushPacket(tsPacket(0x101, 3, true, second, 0, false))
	if err == nil {
		t.Fatal("expected continuity error")
	}
	if len(aus) != 1 || !aus[0].Unrecoverable || aus[0].Reason != "continuity_error" {
		t.Fatalf("aus=%+v", aus)
	}
	if d.Stats().CCJumps != 1 {
		t.Fatalf("jumps=%d", d.Stats().CCJumps)
	}
}

func TestPCRWrapThroughDemux(t *testing.T) {
	d := NewDemux()
	var ccPat, ccPmt uint8
	d.PushPacket(sectionPackets(PIDPAT, patSection(0x1000, 0), &ccPat)[0])
	d.PushPacket(sectionPackets(0x1000, pmtSection(0x101, 0, [2]uint16{StreamH264, 0x101}), &ccPmt)[0])
	base := int64(clock.Wrap - 10)
	for i := int64(0); i < 4; i++ {
		pcr := (base + i) * 300
		p, err := ParsePacket(tsPacket(0x101, uint8(i), false, []byte{0xAA}, pcr, true))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.PushPacket(p.Raw); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	tl := d.TimelineFor(0x101)
	want := (int64(clock.Wrap) - 10 + 3) * 300
	if tl.LastPCR() != want {
		t.Fatalf("unwrapped=%d want %d", tl.LastPCR(), want)
	}
}

func TestSectionAssemblerPointerContinuation(t *testing.T) {
	sec := patSection(0x1000, 0)
	var a SectionAssembler
	// First packet: pointer 0, first 5 bytes of the section only.
	out, err := a.Feed(append([]byte{0}, sec[:5]...), true)
	if err != nil || len(out) != 0 {
		t.Fatalf("out=%d err=%v", len(out), err)
	}
	// Second packet: the pointer field finishes the partial section, then a
	// second complete section follows in the same payload.
	payload := []byte{byte(len(sec) - 5)}
	payload = append(payload, sec[5:]...)
	payload = append(payload, sec...)
	out, err = a.Feed(payload, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("sections=%d", len(out))
	}
}

func TestADTSAndNALHelpers(t *testing.T) {
	if !IsIDR(nalPayload(5, 10)) || IsIDR(nalPayload(1, 10)) {
		t.Fatal("IDR detection")
	}
	if NALUnitType([]byte{9, 9}) != -1 {
		t.Fatal("no start code should return -1")
	}
	if _, _, _, ok := ADTSHeader([]byte{0xFF, 0xF1, 0x50, 0x80, 0, 0, 0}); !ok {
		t.Fatal("ADTS header rejected")
	}
}

// pump feeds raw packets into a demux.
func pump(d *Demux, pkts [][]byte) ([]AccessUnit, error) {
	var out []AccessUnit
	for _, raw := range pkts {
		aus, err := d.PushPacket(raw)
		out = append(out, aus...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
