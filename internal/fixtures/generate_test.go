package fixtures

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/audio"
	"mediapack/internal/color"
	"mediapack/internal/fault"
	"mediapack/internal/frames"
	"mediapack/internal/mp4"
)

func generate(t *testing.T, inject Inject) *Bundle {
	t.Helper()
	b, err := Generate(Options{Seed: 41, Rates: []int{800000, 1600000}, Dir: filepath.Join(t.TempDir(), "corpus"), Inject: inject})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return b
}

func TestGenerateIsDeterministicForSeed(t *testing.T) {
	a := generate(t, Inject{})
	b := generate(t, Inject{})
	if len(a.Rates) != 2 {
		t.Fatalf("rates=%d", len(a.Rates))
	}
	for i := range a.Rates {
		if a.Rates[i].FileHash != b.Rates[i].FileHash {
			t.Fatalf("rate %d mp4 differs across runs", a.Rates[i].Value)
		}
		va := a.Rates[i].Payloads[1]
		vb := b.Rates[i].Payloads[1]
		if !bytes.Equal(va, vb) {
			t.Fatalf("payload differs across runs")
		}
	}
}

func TestGeneratedFrameManifestsValidate(t *testing.T) {
	b := generate(t, Inject{})
	for _, rate := range b.Rates {
		got, err := frames.Validate(&rate.Manifest, rate.Payloads, frames.DefaultLimits())
		if err != nil {
			t.Fatalf("rate %d: %v", rate.Value, err)
		}
		if len(got) != b.FrameCount {
			t.Fatalf("rate %d frames=%d", rate.Value, len(got))
		}
		if !got[0].Keyframe || got[1].Keyframe {
			t.Fatal("keyframe pattern wrong")
		}
	}
}

func TestGeneratedMP4RoundTrips(t *testing.T) {
	b := generate(t, Inject{})
	for _, rate := range b.Rates {
		rt, err := mp4.Check(mustRead(t, rate.MP4Path), mp4.DefaultLimits())
		if err != nil {
			t.Fatalf("rate %d: %v", rate.Value, err)
		}
		if !rt.ByteEqual || rt.MatchPct != 100 {
			t.Fatalf("rate %d round trip: %+v", rate.Value, rt)
		}
		if len(rt.Summary.Tracks) != 2 {
			t.Fatalf("rate %d tracks=%d", rate.Value, len(rt.Summary.Tracks))
		}
	}
}

func TestCorruptFrameIsReportedAtFirstBadFrame(t *testing.T) {
	b := generate(t, Inject{CorruptFrame: true})
	_, err := frames.Validate(&b.Rates[0].Manifest, b.Rates[0].Payloads, frames.DefaultLimits())
	fe, ok := fault.As(err)
	if !ok || fe.Code != "checksum-mismatch" || fe.Frame != 2 {
		t.Fatalf("got %v", err)
	}
	// The second rate is untouched and still validates.
	if _, err := frames.Validate(&b.Rates[1].Manifest, b.Rates[1].Payloads, frames.DefaultLimits()); err != nil {
		t.Fatalf("healthy rate failed: %v", err)
	}
}

func TestDuplicateAndMissingFramesAreReported(t *testing.T) {
	dup := generate(t, Inject{DuplicateFrame: true})
	if _, err := frames.Validate(&dup.Rates[0].Manifest, dup.Rates[0].Payloads, frames.DefaultLimits()); err == nil {
		t.Fatal("duplicate accepted")
	}
	miss := generate(t, Inject{MissingFrame: true})
	fe, ok := fault.As(mustValidateErr(t, &miss.Rates[0]))
	if !ok || fe.Code != "missing-frame" || fe.Frame != 4 {
		t.Fatalf("got %v", fe)
	}
}

func TestTruncatedAndIllegalMP4Reported(t *testing.T) {
	trunc := generate(t, Inject{TruncatedMP4: true})
	if _, err := mp4.Check(mustRead(t, trunc.Rates[0].MP4Path), mp4.DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("truncated: %v", err)
	}
	illegal := generate(t, Inject{IllegalBoxSize: true})
	if _, err := mp4.Check(mustRead(t, illegal.Rates[0].MP4Path), mp4.DefaultLimits()); fault.ClassOf(err) != fault.ClassInput {
		t.Fatalf("illegal: %v", err)
	}
	unknown := generate(t, Inject{UnknownMP4Box: true})
	rt, err := mp4.Check(mustRead(t, unknown.Rates[0].MP4Path), mp4.DefaultLimits())
	if err != nil || rt.Unknown == 0 || !rt.ByteEqual {
		t.Fatalf("unknown box: %v %+v", err, rt)
	}
}

func TestAudioAndColorConflicts(t *testing.T) {
	ok := generate(t, Inject{})
	if _, err := audio.Resolve(ok.Audio, audio.Config{}, audio.DefaultLimits()); err != nil {
		t.Fatalf("healthy audio: %v", err)
	}
	if _, err := color.Resolve(ok.Color, color.Config{}, color.DefaultLimits()); err != nil {
		t.Fatalf("healthy color: %v", err)
	}
	bad := generate(t, Inject{AudioConflict: true, ColorConflict: true})
	if _, err := audio.Resolve(bad.Audio, audio.Config{}, audio.DefaultLimits()); fault.ClassOf(err) != fault.ClassDependency {
		t.Fatalf("audio conflict: %v", err)
	}
	if _, err := color.Resolve(bad.Color, color.Config{}, color.DefaultLimits()); fault.ClassOf(err) != fault.ClassDependency {
		t.Fatalf("color conflict: %v", err)
	}
}

func TestGenerateRejectsBadOptions(t *testing.T) {
	if _, err := Generate(Options{Dir: t.TempDir()}); fault.ClassOf(err) != fault.ClassUsage {
		t.Fatalf("no rates: %v", err)
	}
	if _, err := Generate(Options{Dir: t.TempDir(), Rates: []int{0}}); fault.ClassOf(err) != fault.ClassUsage {
		t.Fatalf("zero rate: %v", err)
	}
	if _, err := Generate(Options{Dir: t.TempDir(), Rates: []int{1, 2, 3, 4, 5, 6, 7, 8, 9}}); fault.ClassOf(err) != fault.ClassLimit {
		t.Fatalf("too many rates: %v", err)
	}
}

func TestFilesExistOnDisk(t *testing.T) {
	b := generate(t, Inject{})
	for _, rate := range b.Rates {
		for _, name := range []string{"frames.json", "segment.mp4", filepath.Join("payloads", "0001.bin")} {
			if _, err := os.Stat(filepath.Join(filepath.Dir(rate.MP4Path), name)); err != nil {
				t.Fatalf("missing %s: %v", name, err)
			}
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustValidateErr(t *testing.T, r *Rate) error {
	t.Helper()
	_, err := frames.Validate(&r.Manifest, r.Payloads, frames.DefaultLimits())
	if err == nil {
		t.Fatal("expected validation failure")
	}
	return err
}
