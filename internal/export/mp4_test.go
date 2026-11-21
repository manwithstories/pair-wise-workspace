package export

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"avcheck/internal/clock"
	"avcheck/internal/render"
	"avcheck/internal/timeline"
)

func buildSequence(frames30fps, ch int) (*render.Sequence, []int32) {
	tpf := int(clock.TicksPerFrame())
	total := tpf * frames30fps
	m := &timeline.Mixed{
		Channels:       ch,
		Frames:         total,
		Samples:        make([]int32, total*ch),
		PerTrackFrames: map[uint32]int{1: total},
	}
	for i := range m.Samples {
		m.Samples[i] = int32((i*13)%4000 - 2000)
	}
	seq := render.NewRenderer(clock.SampleRate).Render(m)
	return seq, m.Samples
}

func TestWriteAndDecodeRoundTrip(t *testing.T) {
	const ch = 2
	seq, samples := buildSequence(6, ch)
	var buf bytes.Buffer
	exp := NewExporter(clock.SampleRate, ch)
	sum, err := exp.Write(&buf, seq, samples, ch)
	if err != nil {
		t.Fatal(err)
	}
	if sum.FragmentCount != 6 {
		t.Fatalf("fragments=%d, want 6", sum.FragmentCount)
	}
	if !strings.Contains(sum.Line(), "mp4_fragments=6") {
		t.Fatalf("summary line=%q", sum.Line())
	}

	// Decode the container back and verify the fragmented track structure.
	dec, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("mp4 decode: %v", err)
	}
	if dec.Init == nil || dec.Init.Moov == nil || len(dec.Init.Moov.Traks) != 1 {
		t.Fatal("missing init/moov/trak")
	}
	mdhd := dec.Init.Moov.Trak.Mdia.Mdhd
	if mdhd.Timescale != clock.SampleRate {
		t.Fatalf("timescale=%d, want %d", mdhd.Timescale, clock.SampleRate)
	}
	entry := dec.Init.Moov.Trak.Mdia.Minf.Stbl.Stsd.Mp4a
	if entry == nil || entry.ChannelCount != ch || entry.SampleSize != 16 {
		t.Fatalf("audio entry=%+v", entry)
	}
	if len(dec.Segments) != 1 || len(dec.Segments[0].Fragments) != 6 {
		t.Fatalf("segments/fragments wrong: %+v", dec.Segments)
	}
	first := dec.Segments[0].Fragments[0]
	if got := first.Moof.Traf.Tfdt.BaseMediaDecodeTime(); got != 0 {
		t.Fatalf("first dts=%d, want 0", got)
	}
	last := dec.Segments[0].Fragments[5]
	wantLast := uint64(5 * int(clock.TicksPerFrame()))
	if got := last.Moof.Traf.Tfdt.BaseMediaDecodeTime(); got != wantLast {
		t.Fatalf("last dts=%d, want %d", got, wantLast)
	}
}

func TestDeterministicBytes(t *testing.T) {
	const ch = 1
	seq1, s1 := buildSequence(4, ch)
	seq2, s2 := buildSequence(4, ch)
	exp := NewExporter(clock.SampleRate, ch)

	var a, b bytes.Buffer
	if _, err := exp.Write(&a, seq1, s1, ch); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.Write(&b, seq2, s2, ch); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("identical input produced different MP4 bytes")
	}
}

func TestPCM16Saturation(t *testing.T) {
	out := pcm16LE([]int32{40000, -40000, 123})
	if u := uint16(out[0]) | uint16(out[1])<<8; int16(u) != 32767 {
		t.Fatalf("positive saturation=%d", int16(u))
	}
	if u := uint16(out[2]) | uint16(out[3])<<8; int16(u) != -32768 {
		t.Fatalf("negative saturation=%d", int16(u))
	}
}
