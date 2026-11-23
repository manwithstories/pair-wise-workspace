package fixture

import (
	"strings"
	"testing"

	"mediarun/internal/manifest"
)

func TestGeneratedHLSIsAccepted(t *testing.T) {
	plan := Generate(DefaultGenConfig(42))
	m, err := manifest.ParseHLSBytes([]byte(HLSSegments("cam0", plan.Segments)),
		manifest.DefaultLimits())
	if err != nil {
		t.Fatalf("parse generated HLS: %v", err)
	}
	if len(m.Segments) != len(plan.Segments) {
		t.Fatalf("segment count = %d, want %d", len(m.Segments), len(plan.Segments))
	}
	if m.Segments[0].StartNS != 0 || m.Segments[1].StartNS != plan.Segments[1].StartNS {
		t.Fatalf("times not preserved: %+v", m.Segments[:2])
	}
	if len(m.Segments[0].Hash) != 64 {
		t.Fatalf("hash not carried: %q", m.Segments[0].Hash)
	}
}

func TestGeneratedDASHIsAccepted(t *testing.T) {
	plan := Generate(DefaultGenConfig(42))
	src := DASHSegments("cam0", plan.Segments, 90_000)
	d, err := manifest.ParseDASH(strings.NewReader(src), manifest.DefaultLimits())
	if err != nil {
		t.Fatalf("parse generated DASH: %v", err)
	}
	if len(d.Segments) != len(plan.Segments) {
		t.Fatalf("segment count = %d, want %d", len(d.Segments), len(plan.Segments))
	}
	if d.Segments[1].StartNS != plan.Segments[1].StartNS {
		t.Fatalf("DASH start drift: %d vs %d", d.Segments[1].StartNS, plan.Segments[1].StartNS)
	}
}
