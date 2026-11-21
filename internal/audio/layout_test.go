package audio

import (
	"strings"
	"testing"

	"mediapack/internal/fault"
)

func stream(track string, layout Layout, rate uint32, source string) Stream {
	return Stream{Track: track, Layout: layout, SampleRate: rate, Source: source}
}

func TestLayoutChannels(t *testing.T) {
	if got := Count(LayoutStereo); got != 2 {
		t.Fatalf("stereo channels=%d", got)
	}
	if got := Count(Layout5_1); got != 6 {
		t.Fatalf("5.1 channels=%d", got)
	}
	if got := Count(LayoutUnknown); got != 0 {
		t.Fatalf("unknown channels=%d", got)
	}
	if got := Channels(Layout5_1)[2]; got != "FC" {
		t.Fatalf("5.1 channel 2=%s", got)
	}
	// Returned slices must not alias the internal table.
	Channels(LayoutStereo)[0] = "XX"
	if Channels(LayoutStereo)[0] != "FL" {
		t.Fatalf("channel table was mutated")
	}
}

func TestParseLayout(t *testing.T) {
	cases := map[string]Layout{
		"stereo": LayoutStereo, "2.0": LayoutStereo, "5.1": Layout5_1,
		"5.1.2": Layout5_1_2, "atmos": LayoutAtmos, "7.1.4": LayoutAtmos,
		"nonsense": LayoutUnknown,
	}
	for in, want := range cases {
		if got := ParseLayout(in); got != want {
			t.Errorf("ParseLayout(%q)=%s want %s", in, got, want)
		}
	}
}

func TestConsistentStreamsResolve(t *testing.T) {
	res, err := Resolve([]Stream{
		stream("a1", LayoutStereo, 48000, "cam-a"),
		stream("a2", LayoutStereo, 48000, "cam-b"),
	}, Config{}, DefaultLimits())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.MappingRequired || res.Summary() != "stereo,48000Hz" {
		t.Fatalf("unexpected resolution %q", res.Summary())
	}
}

func TestLayoutConflictBlocksByDefault(t *testing.T) {
	_, err := Resolve([]Stream{
		stream("a1", LayoutStereo, 48000, "cam-a"),
		stream("a2", Layout5_1, 48000, "boom"),
	}, Config{}, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Class != fault.ClassDependency || fe.Code != "layout-conflict" {
		t.Fatalf("got %v want dependency layout-conflict", err)
	}
	if fe.BlockedBy != "audio:boom" {
		t.Fatalf("blocked_by=%q want audio:boom", fe.BlockedBy)
	}
	if fault.ExitCode(err) != 2 {
		t.Fatalf("exit code %d", fault.ExitCode(err))
	}
}

func TestRateConflictBlocksAndPreservesSources(t *testing.T) {
	_, err := Resolve([]Stream{
		stream("a1", LayoutStereo, 48000, "cam-a"),
		stream("a2", LayoutStereo, 44100, "remote"),
	}, Config{}, DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "sample-rate-conflict" || !strings.Contains(fe.Msg, "44100") {
		t.Fatalf("got %v", err)
	}
}

func TestPreferSourceNeedsExplicitChoice(t *testing.T) {
	streams := []Stream{
		stream("a1", LayoutStereo, 48000, "cam-a"),
		stream("a2", Layout5_1, 48000, "boom"),
	}
	if _, err := Resolve(streams, Config{Policy: PolicyPrefer, PreferSource: "missing"}, DefaultLimits()); fault.ClassOf(err) != fault.ClassUsage {
		t.Fatalf("missing prefer source should be a usage error, got %v", err)
	}
	// Preferring 5.1 would require an upmix from stereo, which mediapack
	// refuses to invent; the stage blocks on the missing mapping instead.
	if _, err := Resolve(streams, Config{Policy: PolicyPrefer, PreferSource: "boom"}, DefaultLimits()); fault.ClassOf(err) != fault.ClassDependency {
		t.Fatalf("upmix should block, got %v", err)
	}
	res, err := Resolve(streams, Config{Policy: PolicyPrefer, PreferSource: "cam-a"}, DefaultLimits())
	if err != nil {
		t.Fatalf("prefer: %v", err)
	}
	if res.Layout != LayoutStereo || res.Source != "cam-a" || res.AppliedPolicy != PolicyPrefer {
		t.Fatalf("wrong preference: %+v", res)
	}
	if len(res.Conflicts) == 0 {
		t.Fatalf("conflict record was discarded")
	}
	if !res.MappingRequired || len(res.Mappings["a2"]) != 2 {
		t.Fatalf("mapping not built: %+v", res.Mappings)
	}
}

func TestDownmixBuildsExplicitStereoMap(t *testing.T) {
	res, err := Resolve([]Stream{
		stream("a1", Layout5_1, 48000, "boom"),
		stream("a2", Layout5_1, 48000, "boom-b"),
	}, Config{Policy: PolicyMapDown, Target: LayoutStereo}, DefaultLimits())
	if err != nil {
		t.Fatalf("downmix: %v", err)
	}
	if res.Layout != LayoutStereo || !res.MappingRequired || len(res.Mappings) != 2 {
		t.Fatalf("bad downmix result: %+v", res)
	}
	for _, m := range res.Mappings["a1"] {
		if m.TargetChannel != "FL" && m.TargetChannel != "FR" {
			t.Fatalf("unexpected target %s", m.TargetChannel)
		}
		if m.SourceChannel != "FL" && m.SourceChannel != "FR" {
			t.Fatalf("expected direct L/R mapping, got %s", m.SourceChannel)
		}
	}
}

func TestMapChannelsWithoutCounterpartFails(t *testing.T) {
	// Atmos has height channels; stereo has none, and fold-down is only
	// defined for L/R, so a target that cannot be satisfied returns nil.
	src := stream("a", LayoutAtmos, 48000, "x")
	dst := Stream{Layout: LayoutStereo, Track: "a", Source: "x"}
	if got := MapChannels(src, dst); len(got) != 2 {
		t.Fatalf("atmos->stereo should map FL/FR directly, got %+v", got)
	}
	// Downmixing mono to 5.1 has no defined center split.
	if got := MapChannels(stream("a", LayoutMono, 48000, "x"), Stream{Layout: Layout5_1, Track: "a"}); got != nil {
		t.Fatalf("mono->5.1 should not fabricate channels, got %+v", got)
	}
}

func TestLimitsAndValidation(t *testing.T) {
	if _, err := Resolve(nil, Config{}, DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("empty input: %v", err)
	}
	many := make([]Stream, 3)
	for i := range many {
		many[i] = stream("a", LayoutStereo, 48000, "x")
	}
	if _, err := Resolve(many, Config{}, Limits{MaxTracks: 2}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("limit: %v", err)
	}
	if _, err := Resolve([]Stream{stream("a", LayoutUnknown, 48000, "x")}, Config{}, DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("unknown layout: %v", err)
	}
	if _, err := Resolve([]Stream{stream("a", LayoutStereo, 0, "x")}, Config{}, DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("zero rate: %v", err)
	}
	if _, err := Resolve([]Stream{stream("a", LayoutStereo, 48000, "x")}, Config{Policy: "bogus"}, DefaultLimits()); fault.ClassOf(err) != fault.ClassUsage {
		t.Fatalf("bad policy: %v", err)
	}
}
