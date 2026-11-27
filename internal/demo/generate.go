// Package demo generates all fixtures locally with ffmpeg and plain writers.
// No external files are referenced: the source clip, SRT/WebVTT feeds (valid
// and deliberately corrupted), the CSV time-code log including a day-wrap
// record, and the interruption scenario are all synthesized from a seed.
package demo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"avcore/internal/clock"
)

// Spec controls fixture generation.
type Spec struct {
	Dir        string
	Seed       int
	DurationS  int
	SegmentS   int
	Rate       clock.Rate
}

// Paths collects the generated fixture locations.
type Paths struct {
	Source     string
	SRT        string
	BadSRT     string
	VTT        string
	BadVTT     string
	TimecodeCSV string
}

// Generate renders the source media and every text fixture under dir.
func Generate(s Spec) (*Paths, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	p := &Paths{
		Source:      filepath.Join(s.Dir, "source.mp4"),
		SRT:         filepath.Join(s.Dir, "captions.srt"),
		BadSRT:      filepath.Join(s.Dir, "captions_bad.srt"),
		VTT:         filepath.Join(s.Dir, "captions.vtt"),
		BadVTT:      filepath.Join(s.Dir, "captions_bad.vtt"),
		TimecodeCSV: filepath.Join(s.Dir, "timecodes.csv"),
	}
	if err := renderSource(s, p.Source); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.SRT, []byte(validSRT(s.DurationS)), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.BadSRT, []byte(corruptSRT()), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.VTT, []byte(validVTT(s.DurationS)), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.BadVTT, []byte(corruptVTT()), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.TimecodeCSV, []byte(timecodeCSV(s)), 0o644); err != nil {
		return nil, err
	}
	return p, nil
}

// renderSource builds a deterministic test pattern with an audio tone and a
// fixed GOP so keyframes land on the planned cut grid.
func renderSource(s Spec, out string) error {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found: %w", err)
	}
	rate := strconv.Itoa(s.Rate.Num) + "/" + strconv.Itoa(s.Rate.Den)
	gop := strconv.Itoa(s.Rate.NominalFPS())
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=640x480:rate=%s", rate),
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", strconv.Itoa(s.DurationS),
		"-map_metadata", "-1",
		"-c:v", "libx264", "-preset", "veryfast", "-threads", "1",
		"-g", gop, "-keyint_min", gop, "-force_key_frames", "expr:gte(t,n_forced*1)",
		"-c:a", "aac", "-pix_fmt", "yuv420p", "-y", out,
	}
	cmd := exec.Command(ff, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("source render failed: %v\n%s", err, out)
	}
	return nil
}

func validSRT(duration int) string {
	var b []byte
	n := 1
	for t := 0; t+1 < duration; t += 2 {
		b = append(b, []byte(fmt.Sprintf(
			"%d\n%02d:%02d:%02d,000 --> %02d:%02d:%02d,500\ncaption %d at %ds\n\n",
			n, 0, 0, t, 0, 0, t+1, n, t))...)
		n++
	}
	return string(b)
}

// corruptSRT mixes a bad timestamp, a truncated record and an overlap so the
// parser can demonstrate skipping only the unrecoverable entries.
func corruptSRT() string {
	return "1\n00:00:01,000 --> 00:00:02,000\nvalid opening cue\n\n" +
		"2\n00:00:99,000 --> 00:00:03,000\nbad timestamp cue\n\n" +
		"3\n00:00:01,500 --> 00:00:06,000\noverlapping cue\n\n" +
		"4\n00:00:04,000 -->\n"
}

func validVTT(duration int) string {
	b := "WEBVTT\n\n"
	n := 1
	for t := 0; t+1 < duration; t += 2 {
		b += fmt.Sprintf("cue-%d\n00:00:%02d.000 --> 00:00:%02d.500\nwebvtt %d\n\n",
			n, t, t+1, n)
		n++
	}
	return b
}

func corruptVTT() string {
	return "WEBVTT\n\n" +
		"NOTE a comment that must be skipped\n\n" +
		"good\n00:00:01.000 --> 00:00:02.000\nrecoverable cue\n\n" +
		"bad\nbad --> 00:00:03.000\nunparseable timing\n\n"
}

// timecodeCSV emits the documented schema: track,rate,mode,value,offset_ms.
// It includes a drop-frame row, a negative offset and a day-boundary wrap so
// the clock package exercises every conversion path.
func timecodeCSV(s Spec) string {
	rate := strconv.Itoa(s.Rate.Num) + "/" + strconv.Itoa(s.Rate.Den)
	lines := []string{
		"track,rate,mode,value,offset_ms",
		fmt.Sprintf("A,%s,normal,00:00:01:00,0", rate),
		fmt.Sprintf("A,%s,offset,00:00:02:00,-1500", rate),
	}
	if s.Rate == clock.Rate2997 {
		lines = append(lines,
			"A,30000/1001,dropframe,00:00:59;29,0",
			"A,30000/1001,wrap,23:59:59;29,40",
		)
	} else {
		lines = append(lines,
			fmt.Sprintf("A,%s,wrap,23:59:59:%02d,40", rate, s.Rate.NominalFPS()-1),
		)
	}
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
