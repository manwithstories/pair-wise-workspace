package fixture

import (
	"strings"
	"testing"

	"archive/internal/manifest"
	"archive/internal/model"
	"archive/internal/rtp"
)

func TestGenerateDeterministic(t *testing.T) {
	p := DefaultParams(41)
	p.DuplicateEvery = 7
	p.ReorderWindow = 9
	p.LossBurst = 4
	p.LossEvery = 30
	s1 := Generate(p)
	s2 := Generate(p)
	if len(s1.Packets) != len(s2.Packets) {
		t.Fatal("same seed produced different packet counts")
	}
	for i := range s1.Packets {
		a, b := s1.Packets[i], s2.Packets[i]
		if a.ExtSeq != b.ExtSeq || a.TS != b.TS || !rtp.EqualPayload(a.Payload, b.Payload) {
			t.Fatalf("same seed diverged at packet %d", i)
		}
	}
}

func TestFaultInjectionCounts(t *testing.T) {
	p := DefaultParams(41)
	p.LossBurst = 4
	p.LossEvery = 24
	p.DuplicateEvery = 5
	p.ReorderWindow = 8
	s := Generate(p)

	if len(s.LostExtSeqs) == 0 {
		t.Fatal("expected injected burst losses")
	}
	// No lost sequence may appear in the arrival stream.
	arrived := map[int64]bool{}
	dupCount := 0
	seen := map[int64]int{}
	for _, pk := range s.Packets {
		arrived[pk.ExtSeq] = true
		seen[pk.ExtSeq]++
		if seen[pk.ExtSeq] == 2 {
			dupCount++
		}
	}
	for _, lost := range s.LostExtSeqs {
		if arrived[lost] {
			t.Fatalf("lost seq %d leaked into arrival stream", lost)
		}
	}
	if dupCount == 0 {
		t.Fatal("expected duplicated packets in arrival stream")
	}
	// Arrival order must differ from ideal order somewhere (reordering).
	inOrder := true
	prev := int64(-1)
	for _, pk := range s.Packets {
		if pk.IsParity {
			continue
		}
		if pk.ExtSeq < prev {
			inOrder = false
			break
		}
		prev = pk.ExtSeq
	}
	if inOrder {
		t.Fatal("reorder window did not perturb arrival order")
	}
}

func TestParityRecoversSingleLoss(t *testing.T) {
	p := DefaultParams(41)
	p.LossBurst = 1
	p.LossEvery = 4 // one lost member per group of four
	s := Generate(p)

	log := model.NewFaultLog()
	repair := rtp.NewRepairer(log)
	jb := rtp.NewJitterBuffer(64, log)
	present := map[int64]*rtp.MediaPacket{}
	parser := rtp.NewParser(p.ClockRate, log)

	for _, pk := range s.Packets {
		if pk.IsParity {
			repair.AddGroup(rtp.NewParityGroup(pk.Group, pk.ExtSeq, pk.Payload))
			continue
		}
		mp, err := parser.Parse(pk.Datagram)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		jb.Put(mp)
		present[mp.ExtSeq] = mp
	}
	for _, lost := range s.LostExtSeqs {
		got := repair.Try(lost, present)
		if got == nil || !got.Parity {
			t.Fatalf("single-loss seq %d was not parity-recovered", lost)
		}
		want := s.IdealPayload(lost)
		if !rtp.EqualPayload(got.Packet.Payload, want) {
			t.Fatalf("rebuilt payload for %d does not match oracle", lost)
		}
		if !rtp.CheckCRC(got.Packet.Payload) {
			t.Fatal("rebuilt payload fails CRC")
		}
	}
}

func TestPlaylistRoundtrip(t *testing.T) {
	p := DefaultParams(41)
	for _, f := range []manifest.Format{manifest.FormatHLS, manifest.FormatDASH} {
		pl := BuildPlaylist(f, p, 10, 1)
		log := model.NewFaultLog()
		var tl *manifest.Timeline
		var err error
		if f == manifest.FormatHLS {
			tl, err = manifest.ParseHLS(strings.NewReader(pl[f]), "/archive/play", log)
		} else {
			tl, err = manifest.ParseDASH(strings.NewReader(pl[f]), "/archive/mpd", log)
		}
		if err != nil {
			t.Fatalf("%s roundtrip: %v", f, err)
		}
		if len(tl.Segments) != p.Segments {
			t.Fatalf("%s segment count = %d, want %d", f, len(tl.Segments), p.Segments)
		}
		// The discontinuity marker is an HLS playlist construct; the DASH
		// SegmentTemplate fixture has no equivalent attribute.
		if f == manifest.FormatHLS && !tl.Segments[2].Discontinuity {
			t.Fatalf("%s discontinuity marker lost", f)
		}
	}
}
