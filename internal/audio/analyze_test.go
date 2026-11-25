package audio

import (
	"math"
	"testing"

	"fieldcut/internal/clock"
)

func TestReadPCM16AndMetrics(t *testing.T) {
	// Four stereo frames: full-scale positive/negative clips.
	data := []byte{
		0xff, 0x7f, 0x00, 0x80,
		0x00, 0x00, 0x00, 0x00,
		0xff, 0x7f, 0x00, 0x80,
		0x00, 0x00, 0x00, 0x00,
	}
	p, err := ReadPCM16(10, 48000, LayoutStereo, data)
	if err != nil {
		t.Fatal(err)
	}
	m := Analyze(p)
	if m.Clipped != 4 {
		t.Fatalf("clipped=%d want 4; peak=%.6f", m.Clipped, m.Peak)
	}
	if m.PeakDBFS > 0.01 || m.RMSDBFS >= 0 {
		t.Fatalf("bad dbfs: %+v", m)
	}
}

func TestChannelMappingLayouts(t *testing.T) {
	mono := &PCM{TrackID: 1, SampleRate: 48000, Layout: LayoutMono, Channels: [][]float64{{0.8, -0.2}}}
	st, err := MapChannels(mono, LayoutStereo)
	if err != nil || st.Layout != LayoutStereo || st.Channels[0][0] != st.Channels[1][0] {
		t.Fatalf("mono->stereo failed: %#v err=%v", st, err)
	}
	five := &PCM{TrackID: 2, SampleRate: 48000, Layout: Layout51, Channels: [][]float64{
		{1}, {0}, {1}, {0}, {0}, {0},
	}}
	st, err = MapChannels(five, LayoutStereo)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(st.Channels[0][0]-(1+0.707)) > 0.001 {
		t.Fatalf("5.1 downmix L=%v", st.Channels[0][0])
	}
	if st.Channels[1][0] != 0.707 {
		t.Fatalf("5.1 downmix R=%v", st.Channels[1][0])
	}
}

func TestResample441To48k(t *testing.T) {
	n := 44100
	ch := make([]float64, n)
	for i := range ch {
		ch[i] = math.Sin(2 * math.Pi * 1000 * float64(i) / 44100)
	}
	p := &PCM{TrackID: 1, SampleRate: 44100, Layout: LayoutMono, Channels: [][]float64{ch}}
	q, err := Resample(p, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Channels[0]) != 48000 {
		t.Fatalf("out frames=%d want 48000", len(q.Channels[0]))
	}
	if q.SampleRate != 48000 {
		t.Fatal("sample rate not updated")
	}
}

func TestSilenceIntervalsConfigurable(t *testing.T) {
	rate := 1000
	ch := make([]float64, 1000) // one second
	for i := 100; i < 200; i++ {
		ch[i] = 0.5
	}
	p := &PCM{TrackID: 1, SampleRate: rate, Layout: LayoutMono, Channels: [][]float64{ch}}
	ivs := SilenceIntervals(p, -40, 200*clock.NsPerMs)
	if len(ivs) != 1 {
		t.Fatalf("intervals=%v", ivs)
	}
	if ivs[0].Start != 200*clock.NsPerMs || ivs[0].End != clock.NsPerSecond {
		t.Fatalf("unexpected tail silence: %+v", ivs[0])
	}
	ivs = SilenceIntervals(p, -40, 900*clock.NsPerMs)
	if len(ivs) != 0 {
		t.Fatalf("minimum duration filter failed: %v", ivs)
	}
}

func TestRejectMalformedPCM(t *testing.T) {
	if _, err := ReadPCM16(1, 48000, LayoutStereo, []byte{1, 2, 3}); err == nil {
		t.Fatal("expected frame alignment error")
	}
	big := make([]byte, MaxPCMBytes+2)
	if _, err := ReadPCM16(1, 48000, LayoutMono, big); err == nil {
		t.Fatal("expected size cap error")
	}
}
