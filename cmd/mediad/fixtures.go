package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fashionweek/mediad/internal/pipeline"
)

// fixtureCase describes one generated sample for the verifier.
type fixtureCase struct {
	Name         string `json:"name"`
	File         string `json:"file"`
	Kind         string `json:"kind"` // valid | truncated_container | pts_rollback | invalid_timebase
	ExpectReject bool   `json:"expect_reject"`
}

// fixturesManifest is the cases/index.json consumed by `mediad verify`.
type fixturesManifest struct {
	Seed  int64         `json:"seed"`
	Cases []fixtureCase `json:"cases"`
	Job   string        `json:"job"`
}

func runFixtures(args []string) error {
	fs := flag.NewFlagSet("fixtures", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	seed := fs.Int64("seed", 1, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("fixtures requires --out")
	}
	root, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	srcDir := filepath.Join(root, "sources")
	casesDir := filepath.Join(root, "cases")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(casesDir, 0o755); err != nil {
		return err
	}

	gen := &fixtureGenerator{root: root, srcDir: srcDir, casesDir: casesDir, seed: *seed}
	cases := make([]fixtureCase, 0, 5)

	// Two healthy cameras with different frame rates and start instants.
	if err := gen.healthy("camA", "testsourcesize=1280x720:rate=30", 30); err != nil {
		return err
	}
	cases = append(cases, fixtureCase{Name: "camA", File: "sources/camA.mp4", Kind: "valid"})
	// Both cameras share the same wall-clock origin (genlocked main-stage
	// angles) but run different frame rates (30 vs 30000/1001), which still
	// exercises exact rational VFR alignment while every 2s cut lands on an IDR.
	if err := gen.healthy("camB", "testsourcesize=1280x720:rate=30000/1001", 0); err != nil {
		return err
	}
	cases = append(cases, fixtureCase{Name: "camB", File: "sources/camB.mp4", Kind: "valid"})

	// Three deliberately corrupt samples, each exercised in isolation.
	if err := gen.truncated("case_truncated"); err != nil {
		return err
	}
	cases = append(cases, fixtureCase{
		Name: "case_truncated", File: "cases/case_truncated.mp4",
		Kind: "truncated_container", ExpectReject: true,
	})
	if err := gen.badTimebase("case_badtimebase"); err != nil {
		return err
	}
	cases = append(cases, fixtureCase{
		Name: "case_badtimebase", File: "cases/case_badtimebase.mp4",
		Kind: "invalid_timebase", ExpectReject: true,
	})
	if err := gen.ptsRollback("case_ptsrollback"); err != nil {
		return err
	}
	cases = append(cases, fixtureCase{
		Name: "case_ptsrollback", File: "cases/case_ptsrollback.mp4",
		Kind: "pts_rollback", ExpectReject: true,
	})

	job := pipeline.JobConfig{
		JobID:          "fashionweek-mainstage",
		SegmentSeconds: 2,
		Cameras: []pipeline.CameraInput{
			{Name: "camA", Path: filepath.Join(srcDir, "camA.mp4")},
			{Name: "camB", Path: filepath.Join(srcDir, "camB.mp4")},
		},
	}
	jobBytes, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	jobPath := filepath.Join(root, "job.json")
	if err := os.WriteFile(jobPath, append(jobBytes, '\n'), 0o644); err != nil {
		return err
	}

	manifest := fixturesManifest{Seed: *seed, Cases: cases, Job: "job.json"}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(casesDir, "index.json"),
		append(mb, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("fixtures written under %s: %d samples, job=%s\n",
		root, len(cases), jobPath)
	return nil
}

type fixtureGenerator struct {
	root, srcDir, casesDir string
	seed                   int64
}

// ffmpeg runs a local ffmpeg command, surfacing stderr on failure.
func (g *fixtureGenerator) ffmpeg(args ...string) error {
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg %v: %w", args[:2], err)
	}
	return nil
}

// truncated builds a healthy clip then cuts it through the mdat payload so the
// container's declared boxes run past EOF.
func (g *fixtureGenerator) truncated(name string) error {
	src := filepath.Join(g.casesDir, name+"-tmp.mp4")
	if err := g.makeHealthy(src); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Drop roughly a quarter from the tail, guaranteed inside mdat.
	cut := info.Size() - info.Size()/4
	dst := filepath.Join(g.casesDir, name+".mp4")
	if err := os.WriteFile(dst, data[:cut], 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}

// badTimebase rewrites the mdhd timescale field to zero. The offset is located
// by scanning for the "mdhd" atom rather than hard-coding a byte position.
func (g *fixtureGenerator) badTimebase(name string) error {
	src := filepath.Join(g.casesDir, name+"-tmp.mp4")
	if err := g.makeHealthy(src); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	idx := indexOfAtom(data, "mdhd")
	if idx < 0 {
		return fmt.Errorf("mdhd atom not found in %s", src)
	}
	// idx points at the 4CC type field. Payload starts at idx+4.
	// mdhd v0 payload: version/flags(4) creation(4) mod(4) timescale(4),
	// so timescale sits at payload+12 == idx+16.
	tsOff := idx + 16
	binary.BigEndian.PutUint32(data[tsOff:tsOff+4], 0)
	dst := filepath.Join(g.casesDir, name+".mp4")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}

// ptsRollback corrupts the ctts composition offsets so presentation time jumps
// outside any bounded B-frame reorder window, which the indexer rejects as a
// PTS rollback (distinct from an invalid time base).
func (g *fixtureGenerator) ptsRollback(name string) error {
	src := filepath.Join(g.casesDir, name+"-tmp.mp4")
	if err := g.makeHealthy(src); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	ctts := indexOfAtom(data, "ctts")
	if ctts < 0 {
		return fmt.Errorf("ctts atom not found in %s", src)
	}
	// ctts points at the 4CC (idx). Payload at idx+4:
	// fullbox(4) entry_count(4) then per entry sample_count(4) sample_offset(4).
	// The first entry's sample_offset is therefore at idx+16 (4 payload header
	// + 4 count + 4 sample_count). Inflate it beyond the track duration to drag
	// one presentation timestamp out of the bounded reorder window.
	offField := ctts + 16
	binary.BigEndian.PutUint32(data[offField:offField+4], 0x7FFFFFFF)
	dst := filepath.Join(g.casesDir, name+".mp4")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}

func (g *fixtureGenerator) makeHealthy(dst string) error {
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=6:size=640x360:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-g", "30", "-c:a", "aac",
		"-fflags", "+bitexact", "-flags", "+bitexact", "-map_metadata", "-1",
		dst,
	}
	return g.ffmpeg(args...)
}

// indexOfAtom locates a top-level-or-nested atom's type field by its 4CC.
func indexOfAtom(data []byte, typ string) int {
	needle := []byte(typ)
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if bytes.Equal(data[off+4:off+8], needle) {
			return off
		}
		if size <= 8 || off+size > len(data) {
			// Fall back to a raw byte scan for deeply nested atoms.
			next := bytes.Index(data[off+1:], needle)
			if next < 0 {
				return -1
			}
			return off + 1 + next
		}
		off += size
	}
	return -1
}

func (g *fixtureGenerator) healthy(name, rateSrc string, startOffsetMs int) error {
	dst := filepath.Join(g.srcDir, name+".mp4")
	src := fmt.Sprintf("testsrc=duration=12:size=1280x720:rate=%s", rateValue(rateSrc))
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", src,
		"-f", "lavfi", "-i", "sine=frequency=440:duration=12",
		"-t", "12",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-g", "60", "-keyint_min", "60", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*2)",
		"-c:a", "aac", "-ar", "48000", "-ac", "2",
		"-fflags", "+bitexact", "-flags", "+bitexact",
		"-map_metadata", "-1",
		"-metadata", "creation_time=" + creationTime(g.seed, startOffsetMs),
		dst,
	}
	return g.ffmpeg(args...)
}

func rateValue(s string) string {
	if s == "testsourcesize=1280x720:rate=30" {
		return "30"
	}
	return "30000/1001"
}

// creationTime returns an ISO-8601 UTC stamp anchored on the deterministic seed
// plus a per-camera stagger, so distinct cameras get distinct origins.
func creationTime(seed int64, offsetMs int) string {
	base := time.Unix(1795000000+(seed%100000), 0).UTC()
	return base.Add(time.Duration(offsetMs) * time.Millisecond).
		Format("2006-01-02T15:04:05Z")
}
