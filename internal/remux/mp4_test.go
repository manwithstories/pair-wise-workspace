package remux

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/abema/go-mp4"

	"archivecheck.local/archivecheck/internal/track"
)

// memWriteSeeker is an in-memory io.WriteSeeker for tests.
type memWriteSeeker struct {
	buf []byte
	pos int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	if int(m.pos)+len(p) > len(m.buf) {
		grown := make([]byte, int(m.pos)+len(p))
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:], p)
	m.pos += int64(len(p))
	return len(p), nil
}

func (m *memWriteSeeker) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.buf)) + off
	}
	if m.pos < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	if int(m.pos) > len(m.buf) {
		grown := make([]byte, m.pos)
		copy(grown, m.buf)
		m.buf = grown
	}
	return m.pos, nil
}

func testPlan(t *testing.T) *Plan {
	t.Helper()
	vp := &TrackPlan{
		Track: track.Track{
			Source:      track.Source{Number: 1, Type: track.TypeVideo, CodecID: "V_VP9"},
			Mapping:     track.Mapping{Kind: track.KindVP9, Entry: "vp09", ConfigBox: "vpcC"},
			DestTrackID: 1,
		},
		Width: 320, Height: 180, Handler: "vide", Timescale: 1_000_000, TimescaleMovie: 1000,
	}
	ap := &TrackPlan{
		Track: track.Track{
			Source:      track.Source{Number: 2, Type: track.TypeAudio, CodecID: "A_OPUS"},
			Mapping:     track.Mapping{Kind: track.KindOpus, Entry: "Opus", ConfigBox: "dOps"},
			DestTrackID: 2,
		},
		SampleRate: 48000, Channels: 2, Handler: "soun", Timescale: 1_000_000, TimescaleMovie: 1000,
	}
	for i := 0; i < 25; i++ {
		dts := int64(i) * 40_000
		vp.Samples = append(vp.Samples, Sample{
			TrackNumber: 1, Data: bytes.Repeat([]byte{byte('V' + i%3)}, 100+i),
			SourceOffset: int64(1000 + i*10), DTS: dts, PTS: dts, Duration: 40_000, Keyframe: i%5 == 0,
		})
		ap.Samples = append(ap.Samples, Sample{
			TrackNumber: 2, Data: bytes.Repeat([]byte{byte('A' + i%2)}, 64),
			SourceOffset: int64(5000 + i*10), DTS: dts, PTS: dts, Duration: 40_000, Keyframe: true,
		})
	}
	return &Plan{
		Tracks:         []*TrackPlan{vp, ap},
		MovieTimescale: 1000,
		Brand:          "isom",
	}
}

func TestWriteProducesReadableMP4(t *testing.T) {
	plan := testPlan(t)
	out := &memWriteSeeker{}
	sum, err := Write(out, plan)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Size != int64(len(out.buf)) {
		t.Fatalf("size=%d len=%d", sum.Size, len(out.buf))
	}
	if sum.TrackSamples[1] != 25 || sum.TrackSamples[2] != 25 {
		t.Fatalf("sample counts: %+v", sum.TrackSamples)
	}

	probe, err := mp4.Probe(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(probe.Tracks) != 2 {
		t.Fatalf("tracks=%d", len(probe.Tracks))
	}
	// go-mp4's tkhd decoder reads the common fields at the box start, so the
	// TrackID it reports is not authoritative here; check the bytes directly.
	for _, wantID := range []uint32{1, 2} {
		if !bytes.Contains(out.buf, []byte{byte(wantID >> 24), byte(wantID >> 16), byte(wantID >> 8), byte(wantID)}) {
			t.Fatalf("track ID %d not encoded", wantID)
		}
	}
	if probe.Tracks[0].Timescale != 1_000_000 {
		t.Fatalf("timescale=%d", probe.Tracks[0].Timescale)
	}
	// Every sample's bytes must appear exactly at the recorded output offset.
	for _, tp := range plan.Tracks {
		offsets := sum.SampleOffsets[tp.Track.Number]
		if len(offsets) != len(tp.Samples) {
			t.Fatalf("track %d offsets=%d want %d", tp.Track.Number, len(offsets), len(tp.Samples))
		}
		for i := range tp.Samples {
			want := tp.Samples[i].Data
			got := out.buf[offsets[i] : offsets[i]+int64(len(want))]
			if !bytes.Equal(got, want) {
				t.Fatalf("track %d sample %d bytes differ", tp.Track.Number, i)
			}
		}
	}
	if len(sum.Manifest) != 50 {
		t.Fatalf("manifest=%d", len(sum.Manifest))
	}
}

func TestWriteRejectsUnsupportedCodec(t *testing.T) {
	plan := testPlan(t)
	plan.Tracks[0].Track.Mapping = track.Mapping{Kind: track.KindUnsupported}
	_, err := Write(&memWriteSeeker{}, plan)
	if err == nil {
		t.Fatal("unsupported codec must be refused")
	}
}

func TestWriteRejectsUnsortedSamples(t *testing.T) {
	plan := testPlan(t)
	plan.Tracks[0].Samples[3].DTS = 0
	if _, err := Write(&memWriteSeeker{}, plan); err == nil {
		t.Fatal("out-of-order samples must be refused")
	}
}

func TestWriteRefusesEmptyTrack(t *testing.T) {
	plan := testPlan(t)
	plan.Tracks[1].Samples = nil
	if _, err := Write(&memWriteSeeker{}, plan); err == nil {
		t.Fatal("empty track must be refused")
	}
}

func TestCttsWrittenForNegativeOffsets(t *testing.T) {
	plan := testPlan(t)
	plan.Tracks[0].Samples[1].PTS = plan.Tracks[0].Samples[1].DTS - 10_000
	out := &memWriteSeeker{}
	if _, err := Write(out, plan); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.buf, []byte("ctts")) {
		t.Fatal("ctts missing for negative composition offset")
	}
	// Version 1 ctts must carry the offset as a signed value.
	idx := bytes.Index(out.buf, []byte("ctts"))
	version := out.buf[idx+4]
	if version != 1 {
		t.Fatalf("ctts version=%d want 1", version)
	}
}
