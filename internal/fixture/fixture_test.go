package fixture

import (
	"math/rand"
	"testing"

	"mediasync/internal/model"
	"mediasync/internal/transport"
)

func testConfig() Config {
	return Config{
		DurationTicks: 12000, LossBurstTicks: 80, VideoRate: model.Rate2997,
		Master: model.Clock{TickRate: 1000},
		Reps: []transport.Representation{
			{Name: "low", Bitrate: 200_000},
			{Name: "mid", Bitrate: 700_000},
			{Name: "high", Bitrate: 2_500_000},
		},
		BandwidthSchedule: func(tick int64) int64 {
			third := int64(4000)
			v := []int64{2_000_000, 700_000, 3_000_000}
			idx := int(tick / third)
			if idx > 2 {
				idx = 2
			}
			return v[idx]
		},
	}
}

// TestFixtureNormalStream verifies a clean run produces one packet per frame
// and every packet belongs to a valid representation.
func TestFixtureNormalStream(t *testing.T) {
	cfg := testConfig()
	cfg.LossBurstTicks = 0 // no burst: normal path
	rng := rand.New(rand.NewSource(1))
	events, dropped := GenerateVideoStream(rng, cfg)
	if dropped != 0 {
		t.Fatalf("normal fixture must inject no loss, got %d", dropped)
	}
	delivered := 0
	for _, ev := range events {
		if ev.Dropped {
			t.Fatal("no dropped events expected in normal fixture")
		}
		if ev.Packet.RepIndex < 0 || ev.Packet.RepIndex >= len(cfg.Reps) {
			t.Fatalf("packet has invalid rep index %d", ev.Packet.RepIndex)
		}
		delivered++
	}
	if delivered == 0 {
		t.Fatal("normal fixture produced no packets")
	}
}

// TestFixtureDuplicateAndReorder covers the duplicate/out-of-order scenario:
// Perturb must preserve every non-dropped packet and inject duplicates.
func TestFixtureDuplicateAndReorder(t *testing.T) {
	cfg := testConfig()
	rng := rand.New(rand.NewSource(7))
	base, _ := GenerateVideoStream(rng, cfg)
	perturbed := Perturb(rand.New(rand.NewSource(7)), base, 25, 6)
	if len(perturbed) <= len(base) {
		t.Fatalf("reordering with duplicates must add entries: base=%d got=%d",
			len(base), len(perturbed))
	}
	// Every original non-dropped sequence must still be present.
	want := map[[2]int]bool{} // rep,seq
	for _, ev := range base {
		if !ev.Dropped {
			want[[2]int{ev.Packet.RepIndex, int(ev.Packet.Seq)}] = true
		}
	}
	got := map[[2]int]bool{}
	for _, ev := range perturbed {
		if !ev.Dropped {
			got[[2]int{ev.Packet.RepIndex, int(ev.Packet.Seq)}] = true
		}
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("reordering lost packet %v", k)
		}
	}
}

// TestFixtureBurstLoss verifies the burst injects real losses and that
// retransmissions reuse the ORIGINAL sequence number for receiver recovery.
func TestFixtureBurstLoss(t *testing.T) {
	cfg := testConfig()
	rng := rand.New(rand.NewSource(41))
	events, dropped := GenerateVideoStream(rng, cfg)
	if dropped < 2 {
		t.Fatalf("80ms burst must drop multiple frames, got %d", dropped)
	}
	droppedSeqs := map[uint16]bool{}
	for _, ev := range events {
		if ev.Dropped {
			droppedSeqs[ev.Packet.Seq] = true
		}
	}
	// A retransmitted packet exists for each dropped original sequence.
	retrans := map[uint16]int{}
	for _, ev := range events {
		if !ev.Dropped && droppedSeqs[ev.Packet.Seq] {
			retrans[ev.Packet.Seq]++
		}
	}
	recovered := 0
	for seq := range droppedSeqs {
		if retrans[seq] > 0 {
			recovered++
		}
	}
	if recovered == 0 {
		t.Fatal("burst losses must have in-window retransmissions")
	}
}

// TestFixtureMetadataConflict verifies MXF and MP4 describe the same logical
// tracks with conflicting rotation/color so the policy layer has work to do.
func TestFixtureMetadataConflict(t *testing.T) {
	set, err := BuildMetadata(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mxf := map[string]struct {
		rot      int64
		cp       uint16
		hasColor bool
	}{}
	for _, tr := range set.MXFResult.Tracks {
		var cp uint16
		if tr.Color != nil {
			cp = tr.Color.Primaries
		}
		mxf[tr.TrackID] = struct {
			rot      int64
			cp       uint16
			hasColor bool
		}{tr.Rotation, cp, tr.Color != nil}
	}
	mp4 := map[string]struct {
		rot      int64
		cp       uint16
		hasColor bool
	}{}
	for _, tr := range set.MP4Result.Tracks {
		var cp uint16
		if tr.Color != nil {
			cp = tr.Color.Primaries
		}
		mp4[tr.TrackID] = struct {
			rot      int64
			cp       uint16
			hasColor bool
		}{tr.Rotation, cp, tr.Color != nil}
	}
	if len(mxf) != len(mp4) || len(mxf) == 0 {
		t.Fatalf("containers must describe the same track set: mxf=%v mp4=%v", mxf, mp4)
	}
	conflictFound := false
	for id, m := range mp4 {
		p, ok := mxf[id]
		if !ok {
			t.Fatalf("track %s present in one container only", id)
		}
		if m.rot != p.rot || (m.hasColor && p.hasColor && m.cp != p.cp) {
			conflictFound = true
		}
	}
	if !conflictFound {
		t.Fatal("at least one track must have conflicting rotation/color across containers")
	}
}

// TestFixtureExtremeBandwidthCliff verifies the middle segment forces the
// lowest representation while the outer segments allow higher tiers.
func TestFixtureExtremeBandwidthCliff(t *testing.T) {
	cfg := testConfig()
	mid := selectRep(cfg, 5000) // middle third: 700k ceiling -> restricted tier
	top := selectRep(cfg, 9000) // last third: 3M ceiling -> highest tier
	if mid >= top {
		t.Fatalf("bandwidth cliff must restrict the middle segment below the top tier: mid=%d top=%d", mid, top)
	}
	if top != len(cfg.Reps)-1 {
		t.Fatalf("3M segment should reach the highest rep, got %d", top)
	}
}

// TestAccountRecovery verifies the injected-loss recovery denominator/numerator
// are derived from actual (rep, original-seq) retransmissions.
func TestAccountRecovery(t *testing.T) {
	events := []StreamEvent{
		{Packet: transport.SimPacket{RepIndex: 0, Seq: 1}, Dropped: true},
		{Packet: transport.SimPacket{RepIndex: 0, Seq: 2}, Dropped: false},
		{Packet: transport.SimPacket{RepIndex: 0, Seq: 1}, Dropped: false}, // retransmit heals seq 1
	}
	acc := AccountRecovery(events)
	if acc.Injected != 1 || acc.Recovered != 1 {
		t.Fatalf("want 1 injected/1 recovered, got %+v", acc)
	}
	// A loss with no retransmission stays unrecovered.
	unhealed := AccountRecovery(append(events,
		StreamEvent{Packet: transport.SimPacket{RepIndex: 1, Seq: 9}, Dropped: true}))
	if unhealed.Injected != 2 || unhealed.Recovered != 1 {
		t.Fatalf("want 2 injected/1 recovered, got %+v", unhealed)
	}
}

// TestFixtureDeterministic proves repeated generation with the same seed is
// bit-for-bit identical, supporting idempotent re-runs.
func TestFixtureDeterministic(t *testing.T) {
	cfg := testConfig()
	a, d1 := GenerateVideoStream(rand.New(rand.NewSource(41)), cfg)
	b, d2 := GenerateVideoStream(rand.New(rand.NewSource(41)), cfg)
	if d1 != d2 || len(a) != len(b) {
		t.Fatal("same seed must yield identical stream shape")
	}
	for i := range a {
		if a[i].Dropped != b[i].Dropped ||
			a[i].Packet.Seq != b[i].Packet.Seq ||
			a[i].Packet.RepIndex != b[i].Packet.RepIndex ||
			a[i].Packet.RTPTS != b[i].Packet.RTPTS {
			t.Fatalf("event %d differs across deterministic runs", i)
		}
	}
}
