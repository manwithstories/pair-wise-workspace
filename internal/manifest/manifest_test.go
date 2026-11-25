package manifest

import (
	"strings"
	"testing"
	"time"

	"avlab/internal/media"
)

func sampleDASH() *Manifest {
	segDur := media.FromDuration(2 * time.Second) // 180000 ticks
	bitrates := []struct {
		id   string
		bw   uint32
		w, h uint32
	}{{"low", 400_000, 640, 360}, {"mid", 1_200_000, 1280, 720}, {"high", 3_000_000, 1920, 1080}}
	m := &Manifest{
		Kind: KindDASH, TargetTick: segDur,
		MinBufferTick: media.FromDuration(time.Second),
	}
	for _, br := range bitrates {
		rep := Representation{ID: br.id, Bandwidth: br.bw, Width: br.w, Height: br.h,
			MIME: "video/mp4"}
		var start media.MediaTime
		for i := uint32(1); i <= 8; i++ {
			rep.Segments = append(rep.Segments, Segment{
				Index: i, URL: br.id + "/seg-" + itoa(i) + ".m4s",
				StartTick: start, DurationTick: segDur, Keyframe: true})
			start += segDur
		}
		m.Representations = append(m.Representations, rep)
	}
	return m
}

func sampleHLS() *Manifest {
	m := sampleDASH()
	m.Kind = KindHLS
	disco := map[uint32]bool{3: true, 6: true}
	for ri := range m.Representations {
		var epoch media.Epoch
		var start media.MediaTime
		for si := range m.Representations[ri].Segments {
			s := &m.Representations[ri].Segments[si]
			if disco[s.Index] {
				epoch++
			}
			s.Epoch = epoch
			s.Discontinuity = disco[s.Index]
			s.StartTick = start
			start += s.DurationTick
		}
	}
	return m
}

func TestDASHRoundtrip(t *testing.T) {
	want := sampleDASH()
	raw, err := RenderDASH(want)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := ParseDASH(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d := Compare(want, got); d != nil {
		t.Fatalf("DASH roundtrip mismatch at %s", d)
	}
}

func TestHLSRoundtrip(t *testing.T) {
	want := sampleHLS()
	raw, err := RenderHLS(want)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := ParseHLS(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d := Compare(want, got); d != nil {
		t.Fatalf("HLS roundtrip mismatch at %s", d)
	}
	if len(got.Representations) != 3 {
		t.Fatalf("rep count = %d, want 3", len(got.Representations))
	}
	var epochs media.Epoch
	for _, s := range got.Representations[0].Segments {
		if s.Epoch > epochs {
			epochs = s.Epoch
		}
	}
	if epochs != 2 {
		t.Fatalf("discontinuity epochs = %d, want 2", epochs)
	}
}

func TestCompareReportsFirstField(t *testing.T) {
	base := sampleDASH()
	mut := sampleDASH()
	mut.Representations[2].Segments[4].DurationTick = 7
	if d := Compare(base, mut); d == nil ||
		d.Field != "representations[2].segments[4].duration_tick" {
		t.Fatalf("diff = %+v, want first mismatching field path", d)
	}
}

func TestDamagedInputs(t *testing.T) {
	cases := map[string]struct {
		kind Kind
		raw  []byte
	}{
		"dash_bad_xml": {KindDASH, []byte("<MPD><unclosed>")},
		"dash_wrong_ns": {KindDASH, []byte(
			`<MPD xmlns="http://example.invalid/x" minBufferTime="PT1S"/>`)},
		"hls_missing_endlist": {KindHLS, []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n")},
		"hls_undeclared_rep": {KindHLS, []byte(strings.Join([]string{
			"#EXTM3U", "#EXT-X-VERSION:7", "#EXT-X-TARGETDURATION:2",
			`#EXT-X-AVLAB-REP:ID=low,BANDWIDTH=100000`,
			"#EXTINF:2.000,", "#EXT-X-AVLAB-SEGMENT:low=a.m4s,mid=b.m4s",
			"#EXT-X-ENDLIST", ""}, "\n"))},
	}
	for name, tc := range cases {
		var err error
		if tc.kind == KindDASH {
			_, err = ParseDASH(tc.raw)
		} else {
			_, err = ParseHLS(tc.raw)
		}
		if !media.IsCode(err, media.CodeManifestDamaged) {
			t.Fatalf("%s: err = %v, want CodeManifestDamaged", name, err)
		}
	}
}

func TestManifestTooLarge(t *testing.T) {
	big := make([]byte, media.MaxManifestBytes+1)
	for i := range big {
		big[i] = ' '
	}
	if _, err := ParseDASH(big); !media.IsCode(err, media.CodeManifestTooLarge) {
		t.Fatalf("DASH size limit err = %v", err)
	}
	if _, err := ParseHLS(big); !media.IsCode(err, media.CodeManifestTooLarge) {
		t.Fatalf("HLS size limit err = %v", err)
	}
}

func TestAlignedSegmentRequirement(t *testing.T) {
	m := sampleDASH()
	m.Representations[1].Segments = m.Representations[1].Segments[:7]
	raw, err := RenderDASH(m)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err = ParseDASH(raw); !media.IsCode(err, media.CodeManifestDamaged) {
		t.Fatalf("unaligned segments err = %v, want damaged", err)
	}
}

func itoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
