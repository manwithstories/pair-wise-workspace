package mapping

import (
	"errors"
	"testing"

	"mediapipe/internal/media"
)

func testTracks() []media.Track {
	return []media.Track{
		{ID: 1, Type: media.TrackVideo, Codec: media.CodecAVC, Language: "eng"},
		{ID: 2, Type: media.TrackAudio, Codec: media.CodecOpus, Language: "eng"},
		{ID: 3, Type: media.TrackSubtitle, Codec: media.CodecText, Language: "deu"},
	}
}

func TestResolveByTrackNumber(t *testing.T) {
	r := NewResolver(TargetMatroska, DefaultRules())
	plan, err := r.Resolve(testTracks())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 3 {
		t.Fatalf("kept=%d", len(plan.Kept))
	}
	if out, ok := plan.Remap(3); !ok || out != 3 {
		t.Fatalf("remap(3)=%d ok=%v", out, ok)
	}
}

func TestResolveDeterministicAcrossRuns(t *testing.T) {
	rules := []Rule{
		{Type: media.TrackAudio, OutTrack: 2},
		{TrackNumber: 1, OutTrack: 1},
		{Language: "deu", OutTrack: 9},
	}
	var first []uint64
	for i := 0; i < 5; i++ {
		plan, err := NewResolver(TargetMatroska, rules).Resolve(testTracks())
		if err != nil {
			t.Fatal(err)
		}
		var got []uint64
		for _, r := range plan.OutputTracks() {
			got = append(got, r.OutTrack)
		}
		if i == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d order changed: %v vs %v", i, got, first)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d order changed: %v vs %v", i, got, first)
			}
		}
	}
}

func TestTrackNumberBeatsLanguage(t *testing.T) {
	rules := []Rule{
		{Language: "eng", OutTrack: 4},
		{TrackNumber: 1, OutTrack: 1},
	}
	plan, err := NewResolver(TargetMatroska, rules).Resolve(testTracks()[:1])
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kept[0].OutTrack != 1 {
		t.Fatalf("out=%d want 1", plan.Kept[0].OutTrack)
	}
}

func TestUnmappedTrackFails(t *testing.T) {
	rules := []Rule{{TrackNumber: 1, OutTrack: 1}, {TrackNumber: 2, OutTrack: 2}}
	_, err := NewResolver(TargetMatroska, rules).Resolve(testTracks())
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestUnsupportedCodecForWebM(t *testing.T) {
	// AVC cannot be stream-copied into WebM.
	_, err := NewResolver(TargetWebM, DefaultRules()).Resolve(testTracks())
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestWebMKeepsVP9(t *testing.T) {
	tracks := []media.Track{
		{ID: 1, Type: media.TrackVideo, Codec: media.CodecVP9, Language: "eng"},
		{ID: 2, Type: media.TrackAudio, Codec: media.CodecOpus, Language: "eng"},
	}
	plan, err := NewResolver(TargetWebM, DefaultRules()).Resolve(tracks)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 2 {
		t.Fatalf("kept=%d", len(plan.Kept))
	}
}

func TestFMP4RejectsSubtitle(t *testing.T) {
	_, err := NewResolver(TargetFMP4, DefaultRules()).Resolve(testTracks())
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestFMP4AcceptsAVCOpus(t *testing.T) {
	tracks := testTracks()[:2]
	plan, err := NewResolver(TargetFMP4, DefaultRules()).Resolve(tracks)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 2 {
		t.Fatalf("kept=%d", len(plan.Kept))
	}
}

func TestDropRuleIsNotAnError(t *testing.T) {
	rules := []Rule{
		{TrackNumber: 1, OutTrack: 1},
		{TrackNumber: 2, OutTrack: 2},
		{TrackNumber: 3, Drop: true},
	}
	plan, err := NewResolver(TargetMatroska, rules).Resolve(testTracks())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 2 || len(plan.Dropped) != 1 {
		t.Fatalf("kept=%d dropped=%d", len(plan.Kept), len(plan.Dropped))
	}
}

func TestRuleAssigningZeroFails(t *testing.T) {
	rules := []Rule{{TrackNumber: 1, OutTrack: 0}}
	_, err := NewResolver(TargetMatroska, rules).Resolve(testTracks()[:1])
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestDuplicateOutputTrackFails(t *testing.T) {
	rules := []Rule{
		{TrackNumber: 1, OutTrack: 7},
		{TrackNumber: 2, OutTrack: 7},
		{TrackNumber: 3, OutTrack: 3},
	}
	_, err := NewResolver(TargetMatroska, rules).Resolve(testTracks())
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestUnknownTargetFails(t *testing.T) {
	_, err := NewResolver(Target("mkv2"), DefaultRules()).Resolve(testTracks())
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v", err)
	}
}

func TestCodecOverrideApplied(t *testing.T) {
	rules := []Rule{{TrackNumber: 1, OutTrack: 1, CodecOverride: media.CodecVP9}}
	plan, err := NewResolver(TargetWebM, rules).Resolve(testTracks()[:1])
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kept[0].OutCodec != media.CodecVP9 {
		t.Fatalf("codec=%s", plan.Kept[0].OutCodec)
	}
}

func TestLanguageSelectsAlternateAudio(t *testing.T) {
	tracks := []media.Track{
		{ID: 1, Type: media.TrackVideo, Codec: media.CodecAVC, Language: "eng"},
		{ID: 5, Type: media.TrackAudio, Codec: media.CodecAAC, Language: "fra"},
		{ID: 6, Type: media.TrackAudio, Codec: media.CodecAAC, Language: "eng"},
	}
	rules := []Rule{
		{TrackNumber: 1, OutTrack: 1},
		{Language: "eng", Type: media.TrackAudio, OutTrack: 2},
		{Language: "fra", Type: media.TrackAudio, OutTrack: 3},
	}
	plan, err := NewResolver(TargetMatroska, rules).Resolve(tracks)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Kept) != 3 {
		t.Fatalf("kept=%d", len(plan.Kept))
	}
}

func TestMediaErrorUnwrap(t *testing.T) {
	base := errors.New("bad")
	err := media.NewError("mapping", media.ClassUnsupported, base)
	if !errors.Is(err, base) {
		t.Fatal("unified error must unwrap")
	}
	if media.ExitCode(err) != 3 {
		t.Fatalf("exit=%d", media.ExitCode(err))
	}
}
