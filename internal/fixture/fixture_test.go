package fixture

import (
	"bytes"
	"io"
	"testing"

	"mkvarchive/internal/audio"
	"mkvarchive/internal/ebml"
	"mkvarchive/internal/track"
	"mkvarchive/internal/video"
)

func TestAllCasesParseWithRealScanner(t *testing.T) {
	cases := []Case{
		CaseStandard, CaseUnknownBlock, CaseTruncated,
		CaseChannelMismatch, CaseSampleRateConflict, CaseColorMissing,
	}
	for _, c := range cases {
		s := Generate(c, 6)
		sc, err := ebml.NewScanner(bytes.NewReader(s.Data))
		if err != nil {
			t.Fatalf("%s: scanner: %v", c, err)
		}
		n := 0
		for {
			_, err = sc.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			n++
		}
		if n != s.BlockCount {
			t.Fatalf("%s: recovered blocks=%d want %d firstErr=%v",
				c, n, s.BlockCount, sc.FirstError())
		}
	}
}

func TestTruncatedErrorCarriesPreciseOffset(t *testing.T) {
	s := Generate(CaseTruncated, 4)
	sc, err := ebml.NewScanner(bytes.NewReader(s.Data))
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = sc.Next()
		if err != nil {
			break
		}
	}
	ae := sc.FirstError()
	if ae == nil || ae.Code != "EBML_TRUNCATED_HEADER" {
		t.Fatalf("want anchored truncation, got %+v", ae)
	}
	if ae.Offset != int64(len(s.Data)-1) || ae.Offset <= 0 {
		t.Fatalf("offset must be a positive integer at the tail marker, got %d (len=%d)",
			ae.Offset, len(s.Data))
	}
}

func TestMetadataCasesPropagateManifestWarnings(t *testing.T) {
	s := Generate(CaseChannelMismatch, 2)
	sc, err := ebml.NewScanner(bytes.NewReader(s.Data))
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := track.ParseTracks(sc.Meta().TracksData)
	if err != nil {
		t.Fatalf("parse tracks: %v", err)
	}
	g, err := track.BuildGraph(tracks)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	policy := track.Policy{Audio: audio.PolicyRetain, Video: video.PolicyRetain}
	_, warnings, err := track.BuildMapping(g, policy)
	if err != nil {
		t.Fatalf("retain policy should not hard-fail: %v", err)
	}
	found := false
	for _, w := range warnings {
		if w.Code == s.ExpectWarning {
			found = true
		}
	}
	if !found {
		t.Fatalf("warning %q not propagated, got %+v", s.ExpectWarning, warnings)
	}
}

func TestDeterministicGeneration(t *testing.T) {
	a := Generate(CaseStandard, 8)
	b := Generate(CaseStandard, 8)
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatal("fixture generation must be deterministic")
	}
}
