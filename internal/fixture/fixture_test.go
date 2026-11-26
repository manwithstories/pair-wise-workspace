package fixture

import (
	"bytes"
	"reflect"
	"testing"

	"mediaflow/internal/timeline"
)

func TestGenerateDeterministic(t *testing.T) {
	cfg := DefaultConfig(41)
	a, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Packets) != len(b.Packets) || len(a.Events) != len(b.Events) {
		t.Fatal("rerun length mismatch")
	}
	for i := range a.Packets {
		if !reflect.DeepEqual(a.Packets[i], b.Packets[i]) {
			t.Fatalf("packet %d differs across reruns", i)
		}
	}
	for i := range a.Events {
		if !bytes.Equal(a.Events[i].Payload, b.Events[i].Payload) {
			t.Fatalf("event %d payload differs", i)
		}
	}
}

func TestVFREnabledAndMonotonic(t *testing.T) {
	cfg := DefaultConfig(41)
	cfg.VFR = true
	clip, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var durs []int64
	var prevUS int64 = -1
	for _, p := range clip.Packets {
		if p.Track != 1 || p.IsParity {
			continue
		}
		if p.TrueUS <= prevUS {
			t.Fatal("video timestamps not strictly monotonic")
		}
		prevUS = p.TrueUS
		durs = append(durs, p.Duration)
	}
	// VFR must actually vary frame durations rather than emit a constant cadence.
	first := durs[0]
	varied := false
	for _, d := range durs[1:] {
		if d != first {
			varied = true
			break
		}
	}
	if !varied {
		t.Fatal("VFR produced uniform frame durations")
	}
}

func TestDriftApplied(t *testing.T) {
	cfg := DefaultConfig(41)
	clip, _ := Generate(cfg)
	// Video track drifts +120 PPM; the monotonic (pre-wrap) tick must exceed
	// the undrifted value after enough time has elapsed.
	tb := mustTB(clip.Tracks[0].Timebase)
	var checked bool
	for _, p := range clip.Packets {
		if p.Track != 1 || p.TrueUS < 2_000_000 {
			continue
		}
		undrifted, err := timeline.FromStandard(p.TrueUS, tb)
		if err != nil {
			t.Fatal(err)
		}
		if p.UnwrappedTS <= undrifted {
			t.Fatalf("drift not applied: unwrapped=%d undrifted=%d", p.UnwrappedTS, undrifted)
		}
		// The wire value must stay inside the wrap modulus.
		if p.RawTS >= clip.Tracks[0].WrapModulus || p.RawTS < 0 {
			t.Fatalf("wrapped raw out of modulus: %d", p.RawTS)
		}
		checked = true
		break
	}
	if !checked {
		t.Fatal("no late video packet found for drift check")
	}
}

func TestWraparoundGenerated(t *testing.T) {
	clip, _ := Generate(DefaultConfig(41))
	mod := map[uint32]int64{}
	for _, tr := range clip.Tracks {
		mod[tr.ID] = tr.WrapModulus
	}
	wrapped := 0
	for _, p := range clip.Packets {
		if p.IsParity || mod[p.Track] == 0 {
			continue
		}
		if p.UnwrappedTS >= p.RawTS+mod[p.Track] {
			wrapped++
		}
	}
	if wrapped == 0 {
		t.Fatal("expected wrapped timestamps in the generated clip")
	}
}

func TestFECGroupingAndParity(t *testing.T) {
	cfg := DefaultConfig(41)
	clip, _ := Generate(cfg)
	parityByGroup := map[[2]uint32]Packet{}
	mediaByGroup := map[[2]uint32][]*Packet{}
	for i := range clip.Packets {
		p := &clip.Packets[i]
		if !p.IsParity {
			continue
		}
		key := [2]uint32{p.Track, uint32(p.FECGroup)}
		parityByGroup[key] = *p
	}
	for i := range clip.Packets {
		p := &clip.Packets[i]
		if p.IsParity || p.FECGroup < 0 {
			continue
		}
		key := [2]uint32{p.Track, uint32(p.FECGroup)}
		mediaByGroup[key] = append(mediaByGroup[key], p)
	}
	if len(parityByGroup) == 0 {
		t.Fatal("no parity packets generated")
	}
	for key, media := range mediaByGroup {
		if len(media) != FECGroupSize {
			t.Fatalf("group %v has %d media packets", key, len(media))
		}
		parity, ok := parityByGroup[key]
		if !ok {
			t.Fatalf("group %v missing parity", key)
		}
		// XOR of all media payloads must equal the parity payload.
		acc := make([]byte, len(parity.Payload))
		for _, m := range media {
			for j, b := range m.Payload {
				acc[j] ^= b
			}
		}
		if !bytes.Equal(acc, parity.Payload) {
			t.Fatalf("group %v parity mismatch", key)
		}
	}
}

func TestDamageDropsAndTruncation(t *testing.T) {
	cfg := DefaultConfig(41)
	clip, _ := Generate(cfg)
	clip.Spec.DropSingles = []int{0, 1}
	clip.Spec.DropMultiples = map[int]int{3: 2}
	clip.Spec.TruncateEvents = []int{0}
	clip.Spec.ReorderWindow = 6
	damaged := ApplyDamage(clip)

	dropped := 0
	for _, p := range damaged.Packets {
		if p.Dropped {
			dropped++
		}
	}
	// ApplyDamage removes dropped packets from delivery; verify via counts.
	srcMedia := 0
	for _, p := range clip.Packets {
		if !p.IsParity {
			srcMedia++
		}
	}
	deliveredMedia := 0
	for _, p := range damaged.Packets {
		if !p.IsParity {
			deliveredMedia++
		}
	}
	// 1 + 1 singles and 2 from the multiple = 4 media packets removed.
	if srcMedia-deliveredMedia != 4 {
		t.Fatalf("expected 4 removed, got %d (src=%d delivered=%d)", srcMedia-deliveredMedia, srcMedia, deliveredMedia)
	}
	if !damaged.Events[0].Truncated || len(damaged.Events[0].Payload) >= len(clip.Events[0].Payload) {
		t.Fatal("event truncation not applied")
	}
}

func TestDuplicateExpansion(t *testing.T) {
	cfg := DefaultConfig(41)
	clip, _ := Generate(cfg)
	clip.Spec.DuplicateSeq = map[uint32][]uint32{1: {0, 5}}
	damaged := ApplyDamage(clip)
	expanded := ExpandDuplicates(damaged.Packets)
	base := len(damaged.Packets)
	if len(expanded) != base+2 {
		t.Fatalf("expected 2 duplicates, got delta %d", len(expanded)-base)
	}
}

func TestHardLimits(t *testing.T) {
	cfg := DefaultConfig(41)
	cfg.VideoFrames = MaxPackets + 1
	if _, err := Generate(cfg); err == nil {
		t.Fatal("expected limit error for oversized clip")
	}
}
