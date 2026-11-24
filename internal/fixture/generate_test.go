package fixture

import (
	"bytes"
	"testing"

	"mediascan/internal/clock"
	"mediascan/internal/manifest"
	"mediascan/internal/probe"
	"mediascan/internal/repair"
)

func TestGenerateDeterministic(t *testing.T) {
	a := Generate(Options{Seed: 41, DropEvery: 97})
	b := Generate(Options{Seed: 41, DropEvery: 97})
	if !bytes.Equal(a.Manifest, b.Manifest) {
		t.Fatal("manifest differs between runs")
	}
	if !bytes.Equal(a.MP4, b.MP4) {
		t.Fatal("mp4 differs between runs")
	}
	if len(a.RTP) != len(b.RTP) {
		t.Fatalf("rtp packet count differs: %d vs %d", len(a.RTP), len(b.RTP))
	}
	for i := range a.RTP {
		if !bytes.Equal(a.RTP[i], b.RTP[i]) {
			t.Fatalf("rtp packet %d differs", i)
		}
	}
}

func TestGenerateSeedChangesStream(t *testing.T) {
	a := Generate(Options{Seed: 41})
	b := Generate(Options{Seed: 42})
	same := len(a.RTP) == len(b.RTP)
	if same {
		for i := range a.RTP {
			if !bytes.Equal(a.RTP[i], b.RTP[i]) {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatal("different seeds should change the generated stream")
	}
	// The seed also sets the audio media-time lead, so the container bytes
	// differ; only the RTP stream identity is asserted here.
	if len(a.MP4) == 0 || len(b.MP4) == 0 {
		t.Fatal("generated containers must not be empty")
	}
}

func TestGeneratedManifestIsParseable(t *testing.T) {
	set := Generate(Options{Seed: 41})
	g, err := manifest.Parse(set.Manifest, set.Loader, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Plans) != 2 {
		t.Fatalf("plans=%d", len(g.Plans))
	}
	if got := len(g.Plans[0].Segments); got != SegmentCount {
		t.Fatalf("segments=%d want %d", got, SegmentCount)
	}
}

func TestGeneratedRTPRecoversLosses(t *testing.T) {
	set := Generate(Options{Seed: 41, DropEvery: 97})
	res := repair.Analyze(set.RTP)
	if res.Lost != 0 {
		t.Fatalf("isolated drops should be recoverable, lost=%d gaps=%v", res.Lost, res.Gaps)
	}
	if res.Recovered == 0 {
		t.Fatal("expected at least one recovery")
	}
	if res.RecoveryPct < 95 {
		t.Fatalf("recovery=%v", res.RecoveryPct)
	}
}

func TestDamagedTimelineWrapsAndQuarantines(t *testing.T) {
	set := Generate(Options{Seed: 41, Damaged: true})
	m := clock.Build(set.Timestamps, clock.Options{})
	if len(m.Unavailable) == 0 {
		t.Fatal("damaged fixture must quarantine at least one segment")
	}
	var wrapped bool
	for _, w := range m.Warnings {
		if w != "" && bytes.Contains([]byte(w), []byte("wraparound")) {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("damaged fixture should cross the 33-bit wraparound")
	}
}

func TestDamagedManifestHasDiscontinuity(t *testing.T) {
	set := Generate(Options{Seed: 41, Damaged: true})
	g, err := manifest.Parse(set.Manifest, set.Loader, manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if g.Plans[0].Discontinuities == 0 {
		t.Fatal("damaged manifest should declare a discontinuity")
	}
}

func TestTruncatedContainerIsShorter(t *testing.T) {
	full := Generate(Options{Seed: 41})
	cut := Generate(Options{Seed: 41, TruncateTail: 200})
	if len(cut.MP4) != len(full.MP4)-200 {
		t.Fatalf("truncated length %d, want %d", len(cut.MP4), len(full.MP4)-200)
	}
	if !bytes.Equal(cut.MP4, full.MP4[:len(cut.MP4)]) {
		t.Fatal("truncation should only remove the tail")
	}
}

func TestConflictOptionsMarkRepresentations(t *testing.T) {
	set := Generate(Options{Seed: 41, ConflictingAudioMask: true, ConflictingColor: true})
	var audio *probe.AudioInfo
	var colors int
	seen := map[uint16]bool{}
	for _, r := range set.Representations {
		if r.Audio != nil {
			audio = r.Audio
		}
		if r.Video != nil && !seen[r.Video.Color.Primaries] {
			seen[r.Video.Color.Primaries] = true
			colors++
		}
	}
	if audio == nil || audio.Mask != 0x3 || audio.EntryChannels != 6 {
		t.Fatalf("audio conflict not modelled: %+v", audio)
	}
	if colors < 2 {
		t.Fatalf("colour conflict not modelled: %d distinct primaries", colors)
	}
}

func TestFragmentedMP4IsNonEmpty(t *testing.T) {
	set := Generate(Options{Seed: 41})
	if len(set.MP4) < 1024 {
		t.Fatalf("mp4 too small: %d bytes", len(set.MP4))
	}
	if set.SourceBytes[0] != 0 || string(set.SourceBytes[4:8]) != "ftyp" {
		t.Fatal("mp4 does not start with an ftyp box")
	}
}
