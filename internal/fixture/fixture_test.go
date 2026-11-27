package fixture

import (
	"strings"
	"testing"

	"avrepair/internal/clock"
	"avrepair/internal/subtitle"
	"avrepair/internal/track"
)

func TestDeterministicGeneration(t *testing.T) {
	a, err := Build(CaseClean)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Build(CaseClean)
	if len(a.Video) != len(b.Video) || len(a.Audio) != len(b.Audio) {
		t.Fatal("regenerated streams differ in length")
	}
	for i := range a.Video {
		va, vb := a.Video[i], b.Video[i]
		if va.TS != vb.TS || va.Bytes != vb.Bytes || va.Luma != vb.Luma ||
			va.Seq != vb.Seq || va.Duration != vb.Duration {
			t.Fatalf("video packet %d not deterministic", i)
		}
	}
	for i := range a.Audio {
		if a.Audio[i].TS != b.Audio[i].TS ||
			len(a.Audio[i].Samples) != len(b.Audio[i].Samples) ||
			a.Audio[i].Samples[0] != b.Audio[i].Samples[0] {
			t.Fatalf("audio packet %d not deterministic", i)
		}
	}
	if SRT(a.Subtitles) != SRT(b.Subtitles) {
		t.Fatal("SRT output not deterministic across runs")
	}
}

func TestAllCasesBuild(t *testing.T) {
	for _, name := range KnownCases {
		sc, err := Build(name)
		if err != nil {
			t.Fatalf("case %s: %v", name, err)
		}
		if len(sc.Video) == 0 || len(sc.Audio) == 0 || len(sc.Subtitles) == 0 {
			t.Fatalf("case %s produced empty streams", name)
		}
		if len(sc.Nodes) < 3 {
			t.Fatalf("case %s graph too small", name)
		}
	}
	if _, err := Build("nope"); err == nil {
		t.Fatal("unknown case must error")
	}
}

func TestRollbackInjected(t *testing.T) {
	sc, _ := Build(CaseRollback)
	// Frame 120 must sit before frame 119 after the injected backward jump.
	if sc.Video[120].TS >= sc.Video[119].TS {
		t.Fatalf("rollback not injected: %d >= %d", sc.Video[120].TS, sc.Video[119].TS)
	}
	// The injected 40ms jump stays within the 500ms repair budget.
	if sc.Video[119].TS-sc.Video[120].TS > clock.MaxRepairDelta {
		t.Fatal("injected defect must remain repairable")
	}
}

func TestOverlapInjectedAndReconciled(t *testing.T) {
	sc, _ := Build(CaseOverlap)
	if sc.Subtitles[3].EndNS <= sc.Subtitles[4].StartNS {
		t.Fatal("overlap defect not injected")
	}
	tr, err := subtitle.Parse("sub", subtitle.FormatSRT,
		strings.NewReader(SRT(sc.Subtitles)))
	if err != nil {
		t.Fatal(err)
	}
	res := subtitle.Reconcile(tr)
	if subtitle.OverlapCount(res.Errors) == 0 {
		t.Fatal("reconciler must report the injected overlap")
	}
	for i := 1; i < len(res.Cues); i++ {
		if res.Cues[i-1].EndNS > res.Cues[i].StartNS {
			t.Fatal("overlap survived reconciliation")
		}
	}
}

func TestSRTWebVTTRoundTrip(t *testing.T) {
	sc, _ := Build(CaseClean)
	srtTr, err := subtitle.Parse("s", subtitle.FormatSRT,
		strings.NewReader(SRT(sc.Subtitles)))
	if err != nil {
		t.Fatal(err)
	}
	vttTr, err := subtitle.Parse("v", subtitle.FormatWebVTT,
		strings.NewReader(WebVTT(sc.Subtitles)))
	if err != nil {
		t.Fatal(err)
	}
	if len(srtTr.Cues) != len(vttTr.Cues) ||
		len(srtTr.Cues) != len(sc.Subtitles) {
		t.Fatal("cue count mismatch across formats")
	}
	if srtTr.Cues[0].StartNS != 0 ||
		srtTr.Cues[1].StartNS != clock.Second {
		t.Fatal("SRT timestamps not on the nanosecond time base")
	}
}

func TestTruncationAndInterruptionFlags(t *testing.T) {
	tr, _ := Build(CaseTruncated)
	if tr.TruncateAt <= 0 || tr.Interrupted {
		t.Fatal("truncated scenario flags wrong")
	}
	valid := 0
	for _, p := range tr.Video {
		if p.TS < tr.TruncateAt {
			valid++
		}
	}
	if valid == 0 || valid == len(tr.Video) {
		t.Fatal("truncation must keep a strict prefix")
	}
	in, _ := Build(CaseInterrupted)
	if !in.Interrupted || in.ExpectExit != 3 {
		t.Fatal("interrupted scenario flags wrong")
	}
}

func TestMetaConflictGraph(t *testing.T) {
	sc, _ := Build(CaseMetaConflict)
	byID := map[string]*track.Node{}
	for _, n := range sc.Nodes {
		byID[n.ID] = n
	}
	src, video := byID["video-src"], byID["video"]
	if src == nil || video == nil {
		t.Fatal("conflict graph nodes missing")
	}
	if src.Color.Source == video.Color.Source {
		t.Fatal("color sources must differ to trigger a conflict")
	}
}
