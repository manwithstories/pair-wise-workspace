package transport

import (
	"testing"

	"bq/internal/manifest"
	"bq/internal/model"
)

// buildManifest constructs one stream with groupSize-1 media packets plus one
// parity packet per XOR group. Explicit check bytes make the XOR identity
// verifiable: media XOR holds its own byte, parity holds the group XOR.
func buildManifest(groups int, groupSize int) *manifest.Manifest {
	m := &manifest.Manifest{Version: "bq/1", Seed: 1, DurationS: 10, MasterHz: 90000}
	m.Streams = []manifest.Stream{{
		ID: 0, Kind: manifest.KindVideo, Codec: "h264", Container: "mp4", File: "v.mp4",
		Width: 16, Height: 16, TimebaseNum: 90000, TimebaseDen: 1, KeyframeEvery: 1,
	}}
	for g := 0; g < groups; g++ {
		groupMedia := make([]manifest.Packet, 0, groupSize-1)
		for i := 0; i < groupSize-1; i++ {
			seq := int64(g*(groupSize-1) + i)
			mp := manifest.Packet{
				StreamID: 0, Seq: seq, Kind: manifest.KindMedia, GroupID: int64(g),
				SourceOffset: seq * 10, RawPTS: seq * 90000, DurationTick: 90000,
			}
			mp.XOR = payloadByte(mp)
			groupMedia = append(groupMedia, mp)
			m.Packets = append(m.Packets, mp)
		}
		m.Packets = append(m.Packets, manifest.Packet{
			StreamID: 0, Seq: -1, Kind: manifest.KindParity, GroupID: int64(g),
			SourceOffset: int64(g)*100 + 99, XOR: parityByte(groupMedia),
		})
	}
	return m
}

func TestFullDeliveryNoErrors(t *testing.T) {
	m := buildManifest(4, 5)
	delivered := SimulateDelivery(m)
	res := Recover(m, delivered)
	st := res.Stats[0]
	if st.DeclaredMedia != 16 || st.ReceivedMedia != 16 ||
		st.RecoveredPackets != 0 || len(res.Errors) != 0 {
		t.Fatalf("clean delivery must have no errors: %+v %v", st, res.Errors)
	}
}

func TestSingleLossPerGroupRecovered(t *testing.T) {
	m := buildManifest(8, 5) // 32 media packets, 4 media per group
	// Drop the first media packet of every group: exactly one loss per group.
	var delivered []manifest.Packet
	for _, p := range m.Packets {
		if p.Kind == manifest.KindMedia && p.Seq%4 == 0 {
			continue
		}
		delivered = append(delivered, p)
	}
	res := Recover(m, delivered)
	st := res.Stats[0]
	if st.RecoveredPackets != 8 {
		t.Fatalf("want 8 recovered, got %d (stats %+v)", st.RecoveredPackets, st)
	}
	if st.UnrecoveredPackets != 0 {
		t.Fatalf("all single-loss groups recover, got %d unrecovered", st.UnrecoveredPackets)
	}
	for _, e := range res.Errors {
		if e.Code != model.ErrPacketLoss || e.Severity != model.SeverityRecovered {
			t.Fatalf("recovered loss mislabelled: %v", e)
		}
	}
	// Recovered packet keeps declared provenance.
	if recovered, ok := manifest.FindPacket(res.Packets, 0, 0); !ok || recovered.SourceOffset != 0 {
		t.Fatal("recovered packet provenance lost")
	}
}

func TestDoubleLossUnrecoverable(t *testing.T) {
	m := buildManifest(2, 5) // group 0: seq 0..3
	var delivered []manifest.Packet
	for _, p := range m.Packets {
		if p.Kind == manifest.KindMedia && (p.Seq == 0 || p.Seq == 1) {
			continue
		}
		delivered = append(delivered, p)
	}
	res := Recover(m, delivered)
	st := res.Stats[0]
	if st.UnrecoveredPackets != 2 || st.RecoveredPackets != 0 {
		t.Fatalf("double loss must stay unrecoverable: %+v", st)
	}
	if len(res.Errors) != 2 || res.Errors[0].Code != model.ErrPacketUnrecovered {
		t.Fatalf("want unrecovered errors, got %v", res.Errors)
	}
}

func TestParityAlsoLostSkipsGroup(t *testing.T) {
	m := buildManifest(1, 5)
	var delivered []manifest.Packet
	for _, p := range m.Packets {
		// media seq 0 missing and parity missing: group cannot reconstruct
		if (p.Kind == manifest.KindMedia && p.Seq == 0) || p.Kind == manifest.KindParity {
			continue
		}
		delivered = append(delivered, p)
	}
	res := Recover(m, delivered)
	if res.Stats[0].UnrecoveredPackets != 1 {
		t.Fatalf("missing parity must leave 1 unrecovered: %+v", res.Stats[0])
	}
}

func TestDuplicatePacket(t *testing.T) {
	m := buildManifest(2, 5)
	m.Injection.Duplicates = []manifest.Dup{{StreamID: 0, Seq: 2}}
	delivered := SimulateDelivery(m)
	res := Recover(m, delivered)
	st := res.Stats[0]
	if st.DuplicatePackets != 1 {
		t.Fatalf("want 1 duplicate, got %d", st.DuplicatePackets)
	}
	if st.ReceivedMedia != 8 {
		t.Fatalf("duplicate must not inflate received media: %d", st.ReceivedMedia)
	}
	if res.Errors[0].Code != model.ErrDuplicatePacket {
		t.Fatalf("duplicate not reported: %v", res.Errors)
	}
}

func TestTailTruncation(t *testing.T) {
	m := buildManifest(4, 5) // 16 media packets, seq 0..15
	m.Injection.TruncateAfterSeq = []manifest.Trunc{
		{StreamID: 0, AfterSeq: 11, DeclaredCount: 16},
	}
	delivered := SimulateDelivery(m)
	res := Recover(m, delivered)
	st := res.Stats[0]
	if st.TruncatedPackets != 4 {
		t.Fatalf("want 4 truncated, got %d", st.TruncatedPackets)
	}
	found := false
	for _, e := range res.Errors {
		if e.Code == model.ErrTailTruncation {
			found = true
			if e.Severity != model.SeverityError {
				t.Fatal("truncation severity wrong")
			}
		}
	}
	if !found {
		t.Fatal("truncation error missing")
	}
}

func TestDeterministicLossPattern(t *testing.T) {
	m := buildManifest(20, 5)
	m.Injection.LossRate = 0.02
	first := SimulateDelivery(m)
	second := SimulateDelivery(m)
	if len(first) != len(second) {
		t.Fatal("same seed produced different delivery lengths")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatal("same seed produced different delivery content")
		}
	}
	// With 2% loss over 80 media packets, recovery must stay >= 90%. Real
	// loss is recovered + unrecovered (received already excludes the drops).
	res := Recover(m, first)
	var recovered, unrecovered int64
	for _, st := range res.Stats {
		recovered += st.RecoveredPackets
		unrecovered += st.UnrecoveredPackets
	}
	lost := recovered + unrecovered
	if lost == 0 {
		t.Fatal("test fixture expected some loss at 2%")
	}
	pct := recovered * 100 / lost
	if pct < 90 {
		t.Fatalf("recovery %d%% below 90%% (lost=%d recovered=%d)", pct, lost, recovered)
	}
}

func TestPayloadByteStable(t *testing.T) {
	p := manifest.Packet{StreamID: 1, GroupID: 7, Seq: 3}
	if payloadByte(p) != payloadByte(p) {
		t.Fatal("payload byte not deterministic")
	}
	if payloadByte(p) == payloadByte(manifest.Packet{StreamID: 1, GroupID: 7, Seq: 4}) {
		t.Fatal("different seq should differ (collision unlikely for this pair)")
	}
}
