package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fixture generation: three cameras exercising every recovery path.
//
//	cam1 - healthy MP4, SRT with one isolated bad cue, 25 fps NDF timecode
//	cam2 - MPEG-TS with truncated tail, WebVTT, 30000/1001 DF timecode
//	cam3 - fMP4 with missing sidecar index, truncated-tail SRT, cross-day TC
func generateFixtures(seed, seconds int, srcDir string) error {
	specs := []struct {
		name    string
		rate    string
		suffix  string
		muxArgs []string
	}{
		{"cam1", "25", "mp4", nil},
		{"cam2", "30000/1001", "ts", []string{"-f", "mpegts"}},
		{"cam3", "25", "mp4", []string{"-movflags", "frag_keyframe+empty_moov+default_base_moof"}},
	}
	for i, sp := range specs {
		dir := filepath.Join(srcDir, sp.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		media := filepath.Join(dir, "video."+sp.suffix)
		hue := (seed*37 + i*60) % 360
		args := []string{"-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i",
			fmt.Sprintf("testsrc2=size=640x360:rate=%s:duration=%d", sp.rate, seconds),
			"-vf", fmt.Sprintf("hue=h=%d", hue),
			"-c:v", "libx264", "-g", "50", "-pix_fmt", "yuv420p"}
		args = append(args, sp.muxArgs...)
		args = append(args, media)
		if err := runFF(args...); err != nil {
			return err
		}
		if sp.name == "cam2" {
			if err := truncateTS(media, 0.6); err != nil {
				return err
			}
		}
		if sp.name != "cam3" { // cam3 loses its index on purpose
			if err := os.WriteFile(media+".idx", []byte("presspack-index v1\n"), 0o644); err != nil {
				return err
			}
		}
		if err := writeSubtitleFixture(dir, sp.name, seconds); err != nil {
			return err
		}
		if err := writeTimecodeFixture(dir, sp.name, seconds); err != nil {
			return err
		}
	}
	return nil
}

// truncateTS cuts a transport stream to a fraction of its size, aligned to
// 188-byte packets, simulating a recorder stopped mid-write.
func truncateTS(path string, frac float64) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	n := int(float64(len(raw)) * frac)
	n -= n % 188
	return os.WriteFile(path, raw[:n], 0o644)
}

// writeSubtitleFixture emits per-camera subtitle files: cam1 SRT with one
// malformed cue, cam2 WebVTT, cam3 SRT whose final cue is cut off.
func writeSubtitleFixture(dir, cam string, seconds int) error {
	cue := func(i int) (string, string) {
		start := srtTime(i * 2)
		end := srtTime(i*2 + 1)
		if i*2+1 > seconds {
			end = srtTime(seconds)
		}
		return start, end
	}
	n := (seconds + 1) / 2
	var b strings.Builder
	switch cam {
	case "cam2":
		b.WriteString("WEBVTT\n\n")
		for i := 0; i < n; i++ {
			s, e := cue(i)
			fmt.Fprintf(&b, "%s --> %s\n%s cam2 cue %d\n\n",
				strings.Replace(s, ",", ".", 1), strings.Replace(e, ",", ".", 1),
				"stage", i)
		}
		return os.WriteFile(filepath.Join(dir, "subs.vtt"), []byte(b.String()), 0o644)
	case "cam3":
		for i := 0; i < n; i++ {
			s, e := cue(i)
			fmt.Fprintf(&b, "%d\n%s --> %s\nstage cam3 cue %d\n\n", i+1, s, e, i)
		}
		doc := b.String()
		doc = doc[:len(doc)-7] // tail cue cut mid-write
		return os.WriteFile(filepath.Join(dir, "subs.srt"), []byte(doc), 0o644)
	default: // cam1
		for i := 0; i < n; i++ {
			s, e := cue(i)
			fmt.Fprintf(&b, "%d\n%s --> %s\nstage cam1 cue %d\n\n", i+1, s, e, i)
			if i == 1 {
				b.WriteString("99\nnot-a-timeline\nbroken payload\n\n")
			}
		}
		return os.WriteFile(filepath.Join(dir, "subs.srt"), []byte(b.String()), 0o644)
	}
}

func srtTime(sec int) string {
	return fmt.Sprintf("%02d:%02d:%02d,000", sec/3600, sec/60%60, sec%60)
}

// writeTimecodeFixture writes "fps=<rate>" plus one SMPTE label per second.
// cam1: 25 fps NDF from 10:00:00:00; cam2: 29.97 DF from 01:00:00:00;
// cam3: 25 fps crossing the 24-hour boundary.
func writeTimecodeFixture(dir, cam string, seconds int) error {
	var fps string
	var h, m, s int
	drop := false
	switch cam {
	case "cam2":
		fps, h, m, s, drop = "30000/1001", 1, 0, 0, true
	case "cam3":
		fps, h, m, s = "25", 23, 59, 55
	default:
		fps, h, m, s = "25", 10, 0, 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fps=%s\n", fps)
	for i := 0; i <= seconds; i++ {
		sep := ":"
		if drop {
			sep = ";"
		}
		f := 0
		if drop && s == 0 && m%10 != 0 {
			f = 2 // dropped labels 00/01 at non-multiple-of-ten minutes
		}
		fmt.Fprintf(&b, "%02d:%02d:%02d%s%02d\n", h, m, s, sep, f)
		s++
		if s == 60 {
			s = 0
			m++
			if m == 60 {
				m = 0
				h = (h + 1) % 24
			}
		}
	}
	return os.WriteFile(filepath.Join(dir, "timecode.txt"), []byte(b.String()), 0o644)
}
