package remux

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"

	"archive/internal/clock"
	"archive/internal/demux"
)

func entry(codec string) []byte {
	// Minimal visual sample entry payload; only its bytes matter here.
	p := make([]byte, 78)
	copy(p[12:], codec)
	out := make([]byte, 8, 8+len(p))
	out[0], out[1], out[2], out[3] = 0, 0, 0, byte(8+len(p))
	copy(out[4:], "avc1")
	return append(out, p...)
}

func input(id uint32, dts []int64, cts int64) Input {
	tr := demux.Track{ID: id, Timescale: 90000, Lang: "und", Codec: "avc1", Handler: "vide", Entry: entry("avc1")}
	for i := range dts {
		tr.Samples = append(tr.Samples, demux.Sample{Dur: 3600, CTSOff: cts, Data: []byte{0xde, 0xad, 0x00, byte(i)}})
	}
	return Input{Track: tr, DTS: dts}
}

func TestWriteDeterministic(t *testing.T) {
	in := []Input{input(1, []int64{0, 3600, 7200}, 0)}
	a, ai, err := Write(in)
	if err != nil {
		t.Fatal(err)
	}
	b, bi, err := Write(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same input must produce identical bytes")
	}
	if ai.Bytes != bi.Bytes || ai.Bytes != int64(len(a)) {
		t.Fatalf("reported size %d does not match %d", ai.Bytes, len(a))
	}
}

func TestWriteParsesBackToSameSamples(t *testing.T) {
	in := []Input{input(1, []int64{0, 3600, 7200, 10800}, 0)}
	out, info, err := Write(in)
	if err != nil {
		t.Fatal(err)
	}
	if info.Tracks[0].Samples != 4 {
		t.Fatalf("track sample count = %d", info.Tracks[0].Samples)
	}
	var samples, moofs int
	_, err = mp4.ReadBoxStructure(bytes.NewReader(out), func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoof(), mp4.BoxTypeTraf():
			return h.Expand()
		case mp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			samples += int(box.(*mp4.Trun).SampleCount)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Count the moof separately: the walk above descends into it.
	moofs = bytes.Count(out, []byte("moof"))
	if moofs != 1 || samples != 4 {
		t.Fatalf("moofs=%d samples=%d", moofs, samples)
	}
}

func TestWriteRejectsBackwardsTime(t *testing.T) {
	in := []Input{input(1, []int64{0, 7200, 3600}, 0)}
	if _, _, err := Write(in); err == nil {
		t.Fatal("non-monotonic decode times must be rejected")
	}
}

func TestWriteRejectsMissingEntry(t *testing.T) {
	in := []Input{input(1, []int64{0}, 0)}
	in[0].Track.Entry = nil
	if _, _, err := Write(in); err == nil {
		t.Fatal("track without sample entry must be rejected")
	}
}

func TestWriteRejectsTimestampCount(t *testing.T) {
	in := []Input{input(1, []int64{0, 1}, 0)}
	in[0].DTS = in[0].DTS[:1]
	if _, _, err := Write(in); err == nil {
		t.Fatal("timestamp/sample count mismatch must be rejected")
	}
}

func TestSyncDelta(t *testing.T) {
	v := TrackInfo{Role: "video", Samples: 1}
	a := TrackInfo{Role: "audio", Samples: 1}
	zero := mustRat(0, 1)
	v.Start = mustRat(900, 90000)
	a.Start = zero
	tis := []TrackInfo{v, a}
	if got := syncDelta(tis); got < 9.9 || got > 10.1 {
		t.Fatalf("sync delta = %v, want ~10ms", got)
	}
	if got := syncDelta([]TrackInfo{v}); got != 0 {
		t.Fatalf("single-track sync delta = %v", got)
	}
}

func mustRat(n, d int64) clock.Rational {
	return clock.Must(n, d)
}
