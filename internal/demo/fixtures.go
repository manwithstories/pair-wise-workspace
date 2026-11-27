package demo

import (
	"bufio"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"avpipe/internal/layout"
	"avpipe/internal/media"
	"avpipe/internal/wav"
)

// AnchorSpec is one video keyframe timestamp from the generated fixture.
type AnchorSpec struct {
	At    time.Duration
	Label string
}

// Fixtures describes the deterministically generated sample data.
type Fixtures struct {
	Dir       string
	Normal    string
	Truncated string
	Keyframes string
	Anchors   []AnchorSpec
	Rate      media.Rate
	Labels    []string
	Frames    int
}

// GenerateFixtures renders a 5.1 PCM16 WAV from the seed, a copy with the
// requested number of tail bytes removed, and the keyframe anchor list. No
// external files are read; every byte is produced here.
func GenerateFixtures(dir string, seed int64, frames, corruptTailBytes int) (Fixtures, error) {
	if frames <= 0 {
		return Fixtures{}, fmt.Errorf("fixture frames must be positive")
	}
	// Source material is 44.1 kHz so the pipeline exercises a genuine,
	// non-integer resample to the 48 kHz delivery rate.
	rate := media.Rate{Num: 44100, Den: 1}
	labels := append([]string(nil), layout.Surround.Labels...)
	frameBytes := len(labels) * 2
	if corruptTailBytes < 0 || corruptTailBytes >= frames*frameBytes {
		return Fixtures{}, fmt.Errorf("corrupt tail %d outside 0..%d", corruptTailBytes, frames*frameBytes)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Fixtures{}, fmt.Errorf("create output dir: %w", err)
	}
	samples := hostSamples(seed, frames)
	fx := Fixtures{
		Dir:    dir,
		Normal: filepath.Join(dir, "normal.wav"),
		Rate:   rate, Labels: labels, Frames: frames,
	}
	if err := writeWav(fx.Normal, rate, len(labels), samples); err != nil {
		return Fixtures{}, err
	}
	fx.Truncated = filepath.Join(dir, "truncated.wav")
	if err := truncateCopy(fx.Normal, fx.Truncated, int64(corruptTailBytes)); err != nil {
		return Fixtures{}, err
	}
	fx.Keyframes = filepath.Join(dir, "keyframes.txt")
	// Anchors live on the shared output clock, not the source clock.
	anchors, err := writeKeyframes(fx.Keyframes, frames, media.Rate{Num: 48000, Den: 1})
	if err != nil {
		return Fixtures{}, err
	}
	fx.Anchors = anchors
	return fx, nil
}

// hostSamples synthesizes program material: a centred 440 Hz tone plus a low
// LFE tone and seeded surround noise. The level is set below the loudness
// target so the controller has to lift it, without ever nearing clipping.
func hostSamples(seed int64, frames int) []float64 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float64, frames*6)
	phase := 0.0
	for i := 0; i < frames; i++ {
		t := float64(i) / 44100
		carrier := 0.08 * math.Sin(2*math.Pi*440*t)
		phase += 2 * math.Pi * 440 / 44100
		lfe := 0.05 * math.Sin(2*math.Pi*60*t)
		ls := 0.006 * (rng.Float64()*2 - 1)
		rs := 0.006 * (rng.Float64()*2 - 1)
		out[i*6+0] = carrier
		out[i*6+1] = 0.08 * math.Sin(phase+0.01)
		out[i*6+2] = carrier * 0.75
		out[i*6+3] = lfe
		out[i*6+4] = ls
		out[i*6+5] = rs
	}
	return out
}

func writeWav(path string, rate media.Rate, channels int, samples []float64) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := wav.WritePCM16(f, rate, channels, samples); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// truncateCopy copies src to dst with the last n bytes missing while leaving
// the declared RIFF/JUNK sizes untouched, simulating a dropped tail.
func truncateCopy(src, dst string, n int64) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if n > int64(len(data)) {
		return fmt.Errorf("truncate %d beyond file size %d", n, len(data))
	}
	if err := os.WriteFile(dst, data[:int64(len(data))-n], 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// writeKeyframes emits anchors every 60 ms while they fall strictly inside the
// program, in shared-timeline microseconds, then reads them back through the
// same parser the demo uses.
func writeKeyframes(path string, frames int, rate media.Rate) ([]AnchorSpec, error) {
	clock, err := media.NewClock(rate)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for ms := 0; ; ms += 60 {
		f := clock.Frames(time.Duration(ms) * time.Millisecond)
		if f >= int64(frames) {
			break
		}
		fmt.Fprintf(&b, "%d k%d\n", ms*1000, ms/60)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return LoadKeyframes(path)
}

// LoadKeyframes parses "<microseconds> <label>" lines.
func LoadKeyframes(path string) ([]AnchorSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open keyframes: %w", err)
	}
	defer f.Close()
	var out []AnchorSpec
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("keyframes line %d: want '<micros> <label>'", line)
		}
		us, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || us < 0 {
			return nil, fmt.Errorf("keyframes line %d: invalid timestamp %q", line, fields[0])
		}
		out = append(out, AnchorSpec{At: time.Duration(us) * time.Microsecond, Label: fields[1]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read keyframes: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("keyframes file %s is empty", path)
	}
	return out, nil
}
