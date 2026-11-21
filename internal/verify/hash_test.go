package verify

import (
	"bytes"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/remux"
	"archivecheck.local/archivecheck/internal/track"
)

type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	if int(m.pos)+len(p) > len(m.buf) {
		grown := make([]byte, int(m.pos)+len(p))
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:], p)
	m.pos += int64(len(p))
	return len(p), nil
}

func (m *memWS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 0:
		m.pos = off
	case 1:
		m.pos += off
	case 2:
		m.pos = int64(len(m.buf)) + off
	}
	if int(m.pos) > len(m.buf) {
		grown := make([]byte, m.pos)
		copy(grown, m.buf)
		m.buf = grown
	}
	return m.pos, nil
}

func planWith(t *testing.T) (*remux.Plan, []byte) {
	t.Helper()
	vp := &remux.TrackPlan{
		Track: track.Track{
			Source:  track.Source{Number: 1, Type: track.TypeVideo, CodecID: "V_VP9"},
			Mapping: track.Mapping{Kind: track.KindVP9, Entry: "vp09", ConfigBox: "vpcC"},
		},
		Width: 160, Height: 90, Handler: "vide", Timescale: 1_000_000, TimescaleMovie: 1000,
	}
	var src []byte
	for i := 0; i < 20; i++ {
		data := bytes.Repeat([]byte{byte('A' + i)}, 40+i)
		off := int64(len(src))
		src = append(src, data...)
		vp.Samples = append(vp.Samples, remux.Sample{
			TrackNumber: 1, Data: data, SourceOffset: off,
			DTS: int64(i) * 40_000, PTS: int64(i) * 40_000, Duration: 40_000, Keyframe: i%4 == 0,
		})
	}
	return &remux.Plan{Tracks: []*remux.TrackPlan{vp}, MovieTimescale: 1000, Brand: "isom"}, src
}

func TestCheckMatchesSource(t *testing.T) {
	plan, src := planWith(t)
	out := &memWS{}
	sum, err := remux.Write(out, plan)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(bytes.NewReader(out.buf), int64(len(out.buf)), sum.Manifest, Options{
		Input: bytes.NewReader(src), InputSize: int64(len(src)),
	})
	if err != nil {
		t.Fatal(err)
	}
	rates := res.ComputeRates()
	if rates.RoundtripMatch != 100 || rates.SegmentHashMatch != 100 {
		t.Fatalf("rates=%+v", rates)
	}
	if res.KeyframeQueries == 0 {
		t.Fatal("no keyframe queries")
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("gaps=%+v", res.Gaps)
	}
}

func TestCheckDetectsCorruptedOutput(t *testing.T) {
	plan, src := planWith(t)
	out := &memWS{}
	sum, err := remux.Write(out, plan)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte of the first sample in mdat.
	out.buf[int(sum.MdatOffset)+16+2] ^= 0xFF
	res, err := Check(bytes.NewReader(out.buf), int64(len(out.buf)), sum.Manifest, Options{
		Input: bytes.NewReader(src), InputSize: int64(len(src)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if int64(res.Matched) == res.OutputSamples {
		t.Fatal("corruption not detected")
	}
	if int64(res.Matched) >= res.OutputSamples {
		t.Fatalf("matched=%d output=%d", res.Matched, res.OutputSamples)
	}
}

func TestCheckRejectsTruncatedOutput(t *testing.T) {
	plan, src := planWith(t)
	out := &memWS{}
	sum, err := remux.Write(out, plan)
	if err != nil {
		t.Fatal(err)
	}
	short := out.buf[:int(sum.MdatOffset)+8]
	_, err = Check(bytes.NewReader(short), int64(len(short)), sum.Manifest, Options{
		Input: bytes.NewReader(src), InputSize: int64(len(src)),
	})
	if err == nil {
		t.Fatal("truncated output must fail")
	}
	if fault.ExitCode(err) != 2 {
		t.Fatalf("exit=%d", fault.ExitCode(err))
	}
}

func TestMissingInputHashIsNotAMatch(t *testing.T) {
	plan, _ := planWith(t)
	out := &memWS{}
	sum, err := remux.Write(out, plan)
	if err != nil {
		t.Fatal(err)
	}
	// With no Input reader the source hash is empty; output hashes are real,
	// so nothing may be counted as matching.
	res, err := Check(bytes.NewReader(out.buf), int64(len(out.buf)), sum.Manifest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 0 {
		t.Fatalf("unverified hashes counted as matches: %d", res.Matched)
	}
}
