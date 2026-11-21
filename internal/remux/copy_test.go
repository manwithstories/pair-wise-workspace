package remux

import (
	"os"
	"path/filepath"
	"testing"

	"mediameld/internal/fixture"
	"mediameld/internal/ingest"
	"mediameld/internal/model"
)

// buildInputFromFixture demuxes the studio MP4 and lays out the remux inputs.
func buildInputFromFixture(t *testing.T) (Input, string) {
	t.Helper()
	b, err := fixture.Generate(t.TempDir(), fixture.Options{
		Seed: 17, DriftPPM: 85, WrapAt: 4_294_960_000, VFR: []int64{24, 25, 23},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(b.StudioMP4)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tracks, err := ingest.DemuxMP4(f, b.StudioMP4)
	if err != nil {
		t.Fatal(err)
	}
	var in Input
	for _, tr := range tracks {
		ti := TrackInput{
			ID: tr.ID, Kind: tr.Kind, Timescale: tr.Timescale, Desc: tr.SampleDesc,
			Width: tr.CodedWidth, Height: tr.CodedHeight, Rotation: tr.ContainerRotation,
		}
		for _, s := range tr.Samples {
			ti.Samples = append(ti.Samples, InputSample{
				SourcePath: b.StudioMP4, Offset: s.SourceOffset, Size: s.Size,
				Hash: s.Hash, TrackID: tr.ID, TrackKind: tr.Kind,
				Timescale: tr.Timescale, DTS: s.DTS, PTS: s.PTS, Duration: s.Duration,
			})
		}
		in.Tracks = append(in.Tracks, ti)
	}
	return in, b.StudioMP4
}

func TestRemuxPreservesEverySampleByte(t *testing.T) {
	in, _ := buildInputFromFixture(t)
	out := filepath.Join(t.TempDir(), "out.mp4")
	res, err := Remux(out, in, Fingerprint(in))
	if err != nil {
		t.Fatalf("remux: %v", err)
	}
	if res.Written != 72 {
		t.Fatalf("written=%d want 72", res.Written)
	}
	if res.HashMismatch != 0 {
		t.Fatalf("mismatches=%d", res.HashMismatch)
	}
	// Independent proof: re-demux the output and compare all sample hashes.
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tracks, err := ingest.DemuxMP4(f, out)
	if err != nil {
		t.Fatalf("output not re-demuxable: %v", err)
	}
	if len(tracks) != len(in.Tracks) {
		t.Fatalf("tracks=%d want %d", len(tracks), len(in.Tracks))
	}
	for ti, tr := range tracks {
		if len(tr.Samples) != len(in.Tracks[ti].Samples) {
			t.Fatalf("track %d samples=%d want %d", ti, len(tr.Samples), len(in.Tracks[ti].Samples))
		}
		for si, s := range tr.Samples {
			if s.Hash != in.Tracks[ti].Samples[si].Hash {
				t.Fatalf("track %d sample %d hash changed", ti, si)
			}
			if s.PTS != in.Tracks[ti].Samples[si].PTS {
				t.Fatalf("track %d sample %d PTS drifted", ti, si)
			}
			if s.Duration != in.Tracks[ti].Samples[si].Duration {
				t.Fatalf("track %d sample %d duration drifted", ti, si)
			}
		}
	}
}

func TestResumeRejectsFingerprintMismatch(t *testing.T) {
	cp := Checkpoint{Fingerprint: "aaaa", Stage: "mdat"}
	err := Resume(cp, "bbbb")
	if err == nil {
		t.Fatal("mismatched fingerprint accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeFingerprint {
		t.Fatalf("code=%v", code)
	}
	if Resume(cp, "aaaa") != nil {
		t.Fatal("matching fingerprint rejected")
	}
	bad := Checkpoint{Fingerprint: "x", Stage: "bogus"}
	if code, _ := model.CodeOf(Resume(bad, "x")); code != model.CodeCheckpoint {
		t.Fatalf("unknown stage code=%v", code)
	}
}

func TestRemuxMissingSourceFails(t *testing.T) {
	in := Input{Tracks: []TrackInput{{
		ID: "1", Timescale: 1000,
		Samples: []InputSample{{SourcePath: "/nonexistent/x", Offset: 0, Size: 4}},
	}}}
	if _, err := Remux(filepath.Join(t.TempDir(), "o.mp4"), in, "fp"); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestFingerprintIsStableAndContentSensitive(t *testing.T) {
	in, _ := buildInputFromFixture(t)
	fp1 := Fingerprint(in)
	fp2 := Fingerprint(in)
	if fp1 != fp2 {
		t.Fatal("fingerprint unstable for identical input")
	}
	// Changing the declared hash of one sample must change the fingerprint.
	mutated := Input{Tracks: make([]TrackInput, len(in.Tracks))}
	copy(mutated.Tracks, in.Tracks)
	mutated.Tracks[0].Samples = append([]InputSample(nil), in.Tracks[0].Samples...)
	mutated.Tracks[0].Samples[0].Hash[0] ^= 0xFF
	if Fingerprint(mutated) == fp1 {
		t.Fatal("fingerprint ignored a sample hash change")
	}
}

func TestRemuxRefusesDeclaredHashMismatch(t *testing.T) {
	in, _ := buildInputFromFixture(t)
	// Corrupt the declared hash; the copy verifier must reject the output.
	in.Tracks[0].Samples[0].Hash[0] ^= 0xFF
	_, err := Remux(filepath.Join(t.TempDir(), "bad.mp4"), in, "fp")
	if err == nil {
		t.Fatal("hash mismatch not detected")
	}
	if code, _ := model.CodeOf(err); code != model.CodeFingerprint {
		t.Fatalf("code=%v", code)
	}
}
