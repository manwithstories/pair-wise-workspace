package ts_test

import (
	"testing"

	"mediqc/internal/fixture"
	"mediqc/internal/media"
	"mediqc/internal/ts"
)

func TestDemuxRoundTripsFixtureSamples(t *testing.T) {
	cases, err := fixture.Suite(412)
	if err != nil {
		t.Fatal(err)
	}
	clean := cases[0]
	res, merr := ts.Demux(clean.TS, media.DefaultLimits(), media.NewCollector(256))
	if merr != nil {
		t.Fatal(merr)
	}
	if len(res.Samples) != len(clean.SourceSamples) {
		t.Fatalf("samples = %d, want %d", len(res.Samples), len(clean.SourceSamples))
	}
	for i := range clean.SourceSamples {
		if got, want := res.Samples[i].Hash(), clean.SourceSamples[i].Hash(); got != want {
			t.Fatalf("sample %d hash mismatch", i)
		}
	}
}

func TestDemuxEnforcesSampleLimit(t *testing.T) {
	cases, err := fixture.Suite(412)
	if err != nil {
		t.Fatal(err)
	}
	limits := media.DefaultLimits()
	limits.MaxSamples = 1
	_, merr := ts.Demux(cases[0].TS, limits, media.NewCollector(64))
	if merr == nil || merr.Code != media.CodeResourceLimit {
		t.Fatalf("expected resource limit, got %v", merr)
	}
}

func TestDemuxTracksCCPCRAndTruncation(t *testing.T) {
	cases, err := fixture.Suite(412)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*fixture.Case{}
	for _, c := range cases {
		byID[c.ID[3:]] = c
	}

	cc, merr := ts.Demux(byID["cc-jump"].TS, media.DefaultLimits(), media.NewCollector(256))
	if merr != nil {
		t.Fatal(merr)
	}
	if cc.CCGaps != 1 {
		t.Fatalf("cc gaps = %d, want 1", cc.CCGaps)
	}

	pcr, merr := ts.Demux(byID["pcr-wrap"].TS, media.DefaultLimits(), media.NewCollector(256))
	if merr != nil {
		t.Fatal(merr)
	}
	if pcr.PCRWraps != 1 {
		t.Fatalf("pcr wraps = %d, want 1", pcr.PCRWraps)
	}

	truncated, terr := ts.Demux(byID["truncated"].TS, media.DefaultLimits(), media.NewCollector(256))
	if terr == nil || terr.Code != media.CodeUnexpectedEOF {
		t.Fatalf("expected unexpected EOF, got %v", terr)
	}
	if truncated == nil || len(truncated.Samples) == 0 {
		t.Fatalf("expected partial result before truncation")
	}
}
