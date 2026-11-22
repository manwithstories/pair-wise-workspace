package recovery

import (
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

func makeSlots(p Policy, n int, missing map[int]bool) []Slot {
	out := make([]Slot, n)
	for i := 0; i < n; i++ {
		out[i].Seq = uint16(i)
		out[i].Present = !missing[i]
		if out[i].Present {
			out[i].Samples = make([]int16, p.FramesPerPacket*p.Channels)
			for k := range out[i].Samples {
				out[i].Samples[k] = int16((i + 1) * 100)
			}
		}
	}
	return out
}

func countOrigins(b *model.AudioBlock) (real, rec, unrec int) {
	for _, o := range b.Origins {
		switch o {
		case model.OriginReal:
			real++
		case model.OriginRecovered:
			rec++
		default:
			unrec++
		}
	}
	return
}

func TestSilenceFillShortLoss(t *testing.T) {
	p := Policy{Channels: 1, FramesPerPacket: 4, SilenceRunMax: 1, InterpolateRunMax: 3}
	rec := New(p, model.NewLog(32))
	slots := makeSlots(p, 4, map[int]bool{1: true})
	block, st := rec.Rebuild(7, clock.Ticks(0), slots)
	if len(block.Samples) != 16 {
		t.Fatalf("continuous block length: %d", len(block.Samples))
	}
	real, recovered, unrec := countOrigins(block)
	if real != 12 || recovered != 4 || unrec != 0 {
		t.Fatalf("origin counts real=%d rec=%d unrec=%d", real, recovered, unrec)
	}
	if st.SilenceFills != 1 {
		t.Fatal("short run must use silence fill")
	}
	for _, v := range block.Samples[4:8] {
		if v != 0 {
			t.Fatal("silence fill must be zero PCM")
		}
	}
}

func TestInterpolationBurstLoss(t *testing.T) {
	p := Policy{Channels: 1, FramesPerPacket: 2, SilenceRunMax: 1, InterpolateRunMax: 3}
	rec := New(p, model.NewLog(32))
	// Two missing packets (4 frames) bracketed by real constant-ish ramps.
	slots := makeSlots(p, 4, map[int]bool{1: true, 2: true})
	block, st := rec.Rebuild(1, clock.Ticks(0), slots)
	_, recovered, unrec := countOrigins(block)
	if recovered != 4 || unrec != 0 || st.Interpolations != 1 {
		t.Fatalf("burst recovery wrong rec=%d unrec=%d interp=%d", recovered, unrec, st.Interpolations)
	}
	// Interpolated values must strictly sit between the bracket values.
	left, right := int16(100), int16(400)
	for _, v := range block.Samples[4:8] {
		if v < left || v > right {
			t.Fatalf("interpolation out of bracket range: %d", v)
		}
	}
}

func TestUnrecoverableLongRunAndEdgeGap(t *testing.T) {
	p := Policy{Channels: 2, FramesPerPacket: 2, SilenceRunMax: 1, InterpolateRunMax: 2}
	rec := New(p, model.NewLog(32))
	// Run longer than the interpolation cap.
	slots := makeSlots(p, 6, map[int]bool{1: true, 2: true, 3: true, 4: true})
	block, st := rec.Rebuild(1, clock.Ticks(0), slots)
	_, _, unrec := countOrigins(block)
	if unrec != 4*4 || st.Unrecoverable != 4*4 {
		t.Fatalf("long run must be unrecoverable: %d", unrec)
	}
	// Burst-sized edge gap (exceeds silence threshold) cannot be
	// interpolated without a left bracket and must be unrecoverable.
	edge := makeSlots(p, 3, map[int]bool{0: true, 1: true})
	block2, _ := rec.Rebuild(1, clock.Ticks(0), edge)
	if block2.Origins[0] != model.OriginUnrecoverable {
		t.Fatal("burst gap without left bracket must be unrecoverable")
	}
}

func TestOriginSliceMatchesSamples(t *testing.T) {
	p := DefaultPolicy()
	rec := New(p, model.NewLog(32))
	slots := makeSlots(p, 3, map[int]bool{1: true})
	block, _ := rec.Rebuild(9, clock.Milliseconds(20), slots)
	if len(block.Origins) != len(block.Samples) {
		t.Fatal("every sample needs an origin mark")
	}
	if block.Start != clock.Milliseconds(20) || block.Channels != p.Channels {
		t.Fatal("block metadata not preserved")
	}
}
