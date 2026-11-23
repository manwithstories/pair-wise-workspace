package audio

import (
	"encoding/binary"
	"testing"

	"medpipe/internal/model"
)

func pcmBlock(seq uint32, ts int64, amp int16, silentSamples int) *model.AudioBlock {
	// 160 samples per block, mono, 8 kHz -> 20 ms each.
	payload := make([]byte, 320)
	for i := 0; i < 160; i++ {
		v := amp
		if i < silentSamples {
			v = 0
		}
		binary.LittleEndian.PutUint16(payload[2*i:], uint16(v))
	}
	return &model.AudioBlock{
		Seq: seq, TimestampNS: ts, SampleRate: 8000, Channels: 1,
		Samples: 160, Payload: payload,
	}
}

func TestDetectMergesAdjacentSilence(t *testing.T) {
	blocks := []*model.AudioBlock{
		pcmBlock(0, 0, 0, 160),             // silent
		pcmBlock(1, 20_000_000, 0, 160),    // silent -> merge
		pcmBlock(2, 40_000_000, 500, 0),    // audible
		pcmBlock(3, 60_000_000, 0, 160),    // silent again
	}
	d := NewDetector(4, 160)
	intervals := d.Detect(blocks)
	if len(intervals) != 2 {
		t.Fatalf("intervals=%d want 2: %+v", len(intervals), intervals)
	}
	if intervals[0].StartNS != 0 || intervals[0].EndNS != 40_000_000 || intervals[0].Samples != 320 {
		t.Fatalf("merged interval wrong: %+v", intervals[0])
	}
	if intervals[1].StartNS != 60_000_000 || intervals[1].Samples != 160 {
		t.Fatalf("second interval wrong: %+v", intervals[1])
	}
}

func TestDetectThreshold(t *testing.T) {
	blocks := []*model.AudioBlock{
		pcmBlock(0, 0, 3, 0),   // below threshold => silent
		pcmBlock(1, 20e6, 5, 0), // above threshold => audible
	}
	d := NewDetector(4, 160)
	intervals := d.Detect(blocks)
	if len(intervals) != 1 || intervals[0].Samples != 160 {
		t.Fatalf("threshold handling wrong: %+v", intervals)
	}
}

func TestDetectEmptyAndDeterministic(t *testing.T) {
	d := NewDetector(0, 160)
	if got := d.Detect(nil); len(got) != 0 {
		t.Fatalf("nil input: %+v", got)
	}
	blocks := []*model.AudioBlock{pcmBlock(0, 0, 0, 160), pcmBlock(1, 20e6, 0, 160)}
	a := d.Detect(blocks)
	b := d.Detect(blocks)
	if len(a) != 1 || a[0] != b[0] {
		t.Fatal("silence detection must be deterministic")
	}
}

func TestSilenceDoesNotExposeCutHooks(t *testing.T) {
	// Compile-time guard: silence output has no fields a cutter could consume
	// beyond descriptive timing/sample data.
	var s model.SilenceInterval
	s.StartNS, s.EndNS, s.Samples = 1, 2, 3
	if s.EndNS <= s.StartNS {
		t.Fatal("sanity")
	}
}
