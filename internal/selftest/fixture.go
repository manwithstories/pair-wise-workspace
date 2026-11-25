package selftest

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"medpipe/internal/clock"
)

// Fixtures groups every generated input and the expected corruption facts.
type Fixtures struct {
	Dir string

	// Good inputs.
	Stereo48MP4  string
	Stereo441MP4 string
	SurroundMOV  string
	VideoOnlyMP4 string

	// Corruption samples and their expected first-bad-byte delta (always 0
	// when the scanner's offset matches the true anomaly location).
	TruncatedPath  string
	TruncatedDelta int64
	BadLenPath     string
	BadLenDelta    int64
	BoundaryPath   string
}

// Generate renders all fixtures under opts.Dir/src using deterministic
// ffmpeg lavfi sources plus hand-crafted byte corruption.
func Generate(opts Options) (*Fixtures, error) {
	srcDir := filepath.Join(opts.Dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return nil, clock.E(clock.KindOther, "selftest.fixture", clock.WithWrap(err))
	}
	fx := &Fixtures{Dir: srcDir}

	// Static video + constant-tone audio make normalized windows repeatable,
	// which the dedup stage relies on for a deterministic dedup hit.
	stereo48 := filepath.Join(srcDir, "stereo48.mp4")
	if err := renderClip(renderSpec{
		out: stereo48, dur: 4, size: "320x240", fps: 25,
		channels: 2, rate: 48000,
		color: colorFlags("bt709", "bt709", "bt709"),
	}); err != nil {
		return nil, err
	}
	fx.Stereo48MP4 = stereo48

	stereo441 := filepath.Join(srcDir, "stereo441.mp4")
	if err := renderClip(renderSpec{
		out: stereo441, dur: 4, size: "320x240", fps: 30,
		channels: 2, rate: 44100,
		color: colorFlags("smpte170m", "smpte170m", "smpte170m"),
	}); err != nil {
		return nil, err
	}
	fx.Stereo441MP4 = stereo441

	surround := filepath.Join(srcDir, "surround.mov")
	if err := renderClip(renderSpec{
		out: surround, dur: 4, size: "320x240", fps: 25,
		channels: 6, rate: 44100, format: "mov",
	}); err != nil {
		return nil, err
	}
	fx.SurroundMOV = surround

	videoOnly := filepath.Join(srcDir, "videoonly.mp4")
	if err := renderClip(renderSpec{
		out: videoOnly, dur: 3, size: "320x240", fps: 25,
		noAudio: true,
		color:   colorFlags("bt2020", "smpte2084", "bt2020nc"),
	}); err != nil {
		return nil, err
	}
	fx.VideoOnlyMP4 = videoOnly

	// Corruption 1: truncate the final bytes so the last box (typically mdat)
	// declares an end past EOF. The first anomalous byte is the EOF offset.
	truncPath := filepath.Join(srcDir, "truncated.mp4")
	if err := copyTruncated(stereo48, truncPath, 9); err != nil {
		return nil, err
	}
	fx.TruncatedPath = truncPath
	fx.TruncatedDelta = 0 // scanner must point exactly at the new EOF

	// Corruption 2: illegal top-level box length (<8) at byte zero.
	badLenPath := filepath.Join(srcDir, "badlen.mp4")
	if err := copyWithBadLength(stereo48, badLenPath); err != nil {
		return nil, err
	}
	fx.BadLenPath = badLenPath
	fx.BadLenDelta = 0

	// Corruption 3: boundary timestamps - a tiny valid-but-edge case file.
	boundaryPath := filepath.Join(srcDir, "boundary.mp4")
	if err := renderClip(renderSpec{
		out: boundaryPath, dur: 1, size: "64x48", fps: 5,
		channels: 1, rate: 48000,
	}); err != nil {
		return nil, err
	}
	fx.BoundaryPath = boundaryPath

	return fx, nil
}

type renderSpec struct {
	out      string
	dur      int
	size     string
	fps      int
	channels int
	rate     int
	format   string
	noAudio  bool
	color    []string
}

func renderClip(s renderSpec) error {
	args := []string{
		"-y",
		"-f", "lavfi", "-i",
		fmt.Sprintf("color=c=black:size=%s:rate=%d:duration=%d", s.size, s.fps, s.dur),
	}
	if !s.noAudio {
		args = append(args, "-f", "lavfi", "-i",
			fmt.Sprintf("sine=frequency=440:duration=%d", s.dur))
		args = append(args, "-map", "0:v", "-map", "1:a")
	} else {
		args = append(args, "-map", "0:v")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
	)
	if !s.noAudio {
		args = append(args, "-c:a", "aac",
			"-ac", itoa(s.channels), "-ar", itoa(s.rate))
	}
	args = append(args, s.color...)
	args = append(args, "-movflags", "+faststart+write_colr")
	if s.format != "" {
		args = append(args, "-f", s.format)
	}
	cmd := exec.Command("ffmpeg", args...)
	cmd.Args = append(cmd.Args, s.out)
	if out, err := cmd.CombinedOutput(); err != nil {
		return clock.E(clock.KindOther, "selftest.fixture",
			clock.WithDetail("ffmpeg render %s failed: %s", filepath.Base(s.out), out),
			clock.WithWrap(err))
	}
	return nil
}

func colorFlags(primaries, trc, space string) []string {
	return []string{
		"-color_primaries", primaries,
		"-color_trc", trc,
		"-colorspace", space,
	}
}

func copyTruncated(src, dst string, drop int) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return clock.E(clock.KindOther, "selftest.fixture", clock.WithWrap(err))
	}
	if drop >= len(raw) {
		return clock.E(clock.KindArgument, "selftest.fixture",
			clock.WithDetail("truncation larger than file"))
	}
	return os.WriteFile(dst, raw[:len(raw)-drop], 0o644)
}

func copyWithBadLength(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return clock.E(clock.KindOther, "selftest.fixture", clock.WithWrap(err))
	}
	raw = append([]byte(nil), raw...)
	// First top-level box: 32-bit size at byte 0 set to an illegal 3.
	binary.BigEndian.PutUint32(raw[0:4], 3)
	return os.WriteFile(dst, raw, 0o644)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
