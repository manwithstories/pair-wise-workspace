// Package fixture generates the deterministic media fixtures the selfcheck
// and tests run against. Every fixture is derived from a seed and the ffmpeg
// binary found on PATH, so a given (dir, seed) always produces byte-identical
// files. Fixtures are written under the caller's work directory only; the
// generator refuses any path that escapes it.
package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"avpipe/internal/media"
)

// Kind enumerates the fixture families.
type Kind string

const (
	KindGood       Kind = "good"        // well-formed MXF + MP4
	KindTruncated  Kind = "truncated"   // truncated KLV / MP4 box
	KindBadCrop    Kind = "bad_crop"    // crop larger than the coded frame
	KindWrap       Kind = "wrap"        // 33-bit timestamp wrap
	KindDrift      Kind = "drift"       // clock drift against a reference track
	KindVFR        Kind = "vfr"         // variable frame rate
	KindCancel     Kind = "cancel"      // long-running input for cancellation
	KindBadRotate  Kind = "bad_rotate"  // 45 degree display matrix
	KindBadMap     Kind = "bad_map"     // duplicate / missing track mapping
)

// Spec describes one fixture file.
type Spec struct {
	Kind     Kind
	Name     string
	Format   string // "mxf" or "mp4"
	Seed     int64
	Width    int
	Height   int
	FPS      int
	Seconds  float64
	Video    bool
	Audio    bool
	Rational []string // free-form extra ffmpeg options for the generator
}

// File is a generated fixture.
type File struct {
	Spec   Spec
	Path   string
	Size   int64
	SHA256 string
	// Tracks is the number of streams ffprobe reported, when probed.
	Tracks int
}

// Generator writes fixtures under a single root directory.
type Generator struct {
	root string
	ff   string
}

// New creates a generator bound to root, which must exist or be creatable.
// The ffmpeg binary is resolved once so every fixture uses the same tool.
func New(root string) (*Generator, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, media.New(media.KindUsage, media.Origin{}, "fixture.New", "resolving root failed", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, media.New(media.KindUsage, media.Origin{File: root}, "fixture.New", "creating root failed", err)
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, media.New(media.KindInternal, media.Origin{File: root}, "fixture.New",
			"ffmpeg not found on PATH; install with: brew install ffmpeg@9", err)
	}
	return &Generator{root: abs, ff: ff}, nil
}

// Root returns the resolved fixture root.
func (g *Generator) Root() string { return g.root }

// EnsureSafePath joins a relative name under the root and refuses any escape.
func (g *Generator) EnsureSafePath(name string) (string, error) {
	if name == "" {
		return "", media.Errorf(media.KindUsage, media.Origin{}, "fixture.EnsureSafePath", "empty file name")
	}
	if filepath.IsAbs(name) {
		return "", media.Errorf(media.KindUsage, media.Origin{File: name}, "fixture.EnsureSafePath", "absolute path not allowed")
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", media.Errorf(media.KindUsage, media.Origin{File: name}, "fixture.EnsureSafePath",
			"path escapes fixture root")
	}
	joined := filepath.Join(g.root, clean)
	rel, err := filepath.Rel(g.root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", media.Errorf(media.KindUsage, media.Origin{File: name}, "fixture.EnsureSafePath",
			"path escapes fixture root")
	}
	return joined, nil
}

// Generate writes one fixture and returns its descriptor.
func (g *Generator) Generate(s Spec) (File, error) {
	if s.Name == "" {
		return File{}, media.Errorf(media.KindUsage, media.Origin{}, "fixture.Generate", "fixture needs a name")
	}
	if s.Width <= 0 || s.Height <= 0 {
		s.Width, s.Height = 160, 90
	}
	if s.FPS <= 0 {
		s.FPS = 30
	}
	if s.Seconds <= 0 {
		s.Seconds = 2
	}
	path, err := g.EnsureSafePath(s.Name)
	if err != nil {
		return File{}, err
	}
	args, err := g.buildArgs(s, path)
	if err != nil {
		return File{}, err
	}
	out, err := g.run(args)
	if err != nil {
		return File{}, media.New(media.KindInternal,
			media.Origin{File: path, Offset: 0, Stage: "fixture"},
			"fixture.ffmpeg", "generating "+s.Name+" failed", err)
	}
	if len(out) > 0 {
		return File{}, media.Errorf(media.KindInternal,
			media.Origin{File: path, Stage: "fixture"}, "fixture.ffmpeg", "ffmpeg failed: %s", strings.TrimSpace(string(out)))
	}
	info, err := os.Stat(path)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: path}, "fixture.Generate", "stat failed", err)
	}
	sum, err := HashFile(path)
	if err != nil {
		return File{}, err
	}
	return File{Spec: s, Path: path, Size: info.Size(), SHA256: sum}, nil
}

func (g *Generator) buildArgs(s Spec, out string) ([]string, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-nostdin"}
	// Deterministic sources: testsrc2 pattern and a fixed sine tone.
	if s.Video {
		rate := fmt.Sprintf("%d", s.FPS)
		args = append(args, "-f", "lavfi", "-i",
			fmt.Sprintf("testsrc2=size=%dx%d:rate=%s:duration=%g", s.Width, s.Height, rate, s.Seconds))
	} else {
		args = append(args, "-f", "lavfi", "-i",
			fmt.Sprintf("anullsrc=r=48000:cl=stereo:duration=%g", s.Seconds))
	}
	if s.Audio {
		args = append(args, "-f", "lavfi", "-i",
			fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%g", s.Seconds))
	}

	switch s.Kind {
	case KindWrap:
		// A 33-bit modular counter near the top of its range on the video
		// track. timescale 90kHz and a large positive offset force the PTS
		// field close to 2^33 before the first sample.
		args = append(args, "-video_track_timescale", "90000",
			"-output_ts_offset", "95443.4")
	}

	switch s.Format {
	case "mxf":
		// MXF requires exactly one video stream first and PCM audio.
		if s.Video {
			args = append(args, "-c:v", "mpeg2video", "-b:v", "1M", "-pix_fmt", "yuv420p")
		}
		if s.Audio {
			args = append(args, "-c:a", "pcm_s16le")
		}
		args = append(args, "-f", "mxf")
	case "mp4":
		if s.Video {
			args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0",
				"-g", fmt.Sprintf("%d", s.FPS), "-pix_fmt", "yuv420p")
		}
		if s.Audio {
			args = append(args, "-c:a", "aac", "-b:a", "96000")
		}
		args = append(args, "-movflags", "+faststart", "-f", "mp4")
	default:
		return nil, media.Errorf(media.KindUsage, media.Origin{File: out},
			"fixture.buildArgs", "unsupported format %q", s.Format)
	}
	args = append(args, s.Rational...)
	// -bitexact removes random UIDs, timestamps and muxing identities so the
	// same (dir, seed) produces byte-identical fixtures.
	args = append(args, "-bitexact", "-fflags", "+bitexact", "-flags", "+bitexact")
	args = append(args, out)
	return args, nil
}

func (g *Generator) run(args []string) ([]byte, error) {
	cmd := exec.Command(g.ff, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	// Combined output is captured so failures carry ffmpeg's message.
	return cmd.CombinedOutput()
}

// HashFile returns the lowercase hex SHA-256 of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", media.New(media.KindInternal, media.Origin{File: path}, "fixture.HashFile", "open failed", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", media.New(media.KindInternal, media.Origin{File: path}, "fixture.HashFile", "read failed", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Truncate rewrites a file to keep only the first keep bytes, simulating a
// transport that lost the tail of a fragment.
func (g *Generator) Truncate(src, dst string, keep int64) (File, error) {
	in, err := g.EnsureSafePath(src)
	if err != nil {
		return File{}, err
	}
	out, err := g.EnsureSafePath(dst)
	if err != nil {
		return File{}, err
	}
	info, err := os.Stat(in)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: in}, "fixture.Truncate", "stat failed", err)
	}
	if keep <= 0 || keep >= info.Size() {
		return File{}, media.Errorf(media.KindUsage, media.Origin{File: in}, "fixture.Truncate",
			"keep %d must be between 1 and %d", keep, info.Size())
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: in}, "fixture.Truncate", "read failed", err)
	}
	if err := os.WriteFile(out, data[:keep], 0o644); err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out}, "fixture.Truncate", "write failed", err)
	}
	return fileDescriptor(out, KindTruncated), nil
}

// CorruptKLV flips a byte inside an MXF KLV payload, or zeroes an MP4 box
// header field, so the demuxer detects damage at a known offset.
func (g *Generator) CorruptKLV(src, dst string, offset int64) (File, error) {
	in, err := g.EnsureSafePath(src)
	if err != nil {
		return File{}, err
	}
	out, err := g.EnsureSafePath(dst)
	if err != nil {
		return File{}, err
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: in}, "fixture.CorruptKLV", "read failed", err)
	}
	if offset <= 0 || offset >= int64(len(data)) {
		return File{}, media.Errorf(media.KindUsage, media.Origin{File: in, Offset: offset},
			"fixture.CorruptKLV", "offset %d outside file size %d", offset, len(data))
	}
	// Write a byte sequence that is invalid in both KLV and MP4: an all-ones
	// box/UL type followed by a zero length.
	binary.LittleEndian.PutUint32(data[offset:], 0xFFFFFFFF)
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out}, "fixture.CorruptKLV", "write failed", err)
	}
	return fileDescriptor(out, KindTruncated), nil
}

func fileDescriptor(path string, k Kind) File {
	info, err := os.Stat(path)
	var size int64
	if err == nil {
		size = info.Size()
	}
	sum, _ := HashFile(path)
	return File{Spec: Spec{Kind: k, Name: filepath.Base(path)}, Path: path, Size: size, SHA256: sum}
}

// OverrideTrackHeaderSize rewrites the MP4 tkhd width/height to declare a
// display size that contradicts the coded frame. The result opens as a valid
// MP4 whose declared crop exceeds the actual coded dimensions, which is a
// container/bitstream conflict the demux layer must reject.
func (g *Generator) OverrideTrackHeaderSize(src, dst string, width, height uint32) (File, error) {
	in, err := g.EnsureSafePath(src)
	if err != nil {
		return File{}, err
	}
	out, err := g.EnsureSafePath(dst)
	if err != nil {
		return File{}, err
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: in}, "fixture.OverrideTrackHeaderSize", "read failed", err)
	}
	idx := bytesIndexOf(data, []byte("tkhd"))
	if idx < 0 {
		return File{}, media.Errorf(media.KindInternal, media.Origin{File: in},
			"fixture.OverrideTrackHeaderSize", "tkhd box not found")
	}
	// tkhd layout after "tkhd": version(1)+flags(3) then either
	// v0: ctime(4) mtime(4) trackid(4) reserved(4) duration(4)
	// v1: ctime(8) mtime(8) trackid(4) reserved(4) duration(8)
	// then reserved(8) layer(2) alt(2) volume(2) reserved(2) matrix(36)
	// then width(4) height(4) as 16.16 fixed point.
	version := data[idx+4]
	base := idx + 4 // points at version byte
	var off int
	if version == 0 {
		off = base + 4 + 4 + 4 + 4 + 4 + 4 + 8 + 2 + 2 + 2 + 2 + 36
	} else {
		off = base + 4 + 8 + 8 + 4 + 4 + 8 + 8 + 2 + 2 + 2 + 2 + 36
	}
	if off+8 > len(data) {
		return File{}, media.Errorf(media.KindInternal, media.Origin{File: in},
			"fixture.OverrideTrackHeaderSize", "tkhd box truncated")
	}
	binary.BigEndian.PutUint32(data[off:], width<<16)
	binary.BigEndian.PutUint32(data[off+4:], height<<16)
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out}, "fixture.OverrideTrackHeaderSize", "write failed", err)
	}
	return fileDescriptor(out, KindBadCrop), nil
}

func bytesIndexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

// applyDisplayRotation remuxes src into dst with a display rotation in
// degrees, writing a real display matrix into the track header.
func (g *Generator) applyDisplayRotation(src, dst string, degrees int) (File, error) {
	in, err := g.EnsureSafePath(src)
	if err != nil {
		return File{}, err
	}
	out, err := g.EnsureSafePath(dst)
	if err != nil {
		return File{}, err
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y", "-nostdin",
		"-display_rotation", fmt.Sprintf("%d", degrees),
		"-i", in, "-c", "copy", "-f", "mp4", out,
	}
	msg, err := g.run(args)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out, Stage: "fixture"},
			"fixture.applyDisplayRotation", "generating "+dst+" failed", err)
	}
	if len(msg) > 0 {
		return File{}, media.Errorf(media.KindInternal, media.Origin{File: out, Stage: "fixture"},
			"fixture.applyDisplayRotation", "ffmpeg failed: %s", strings.TrimSpace(string(msg)))
	}
	return fileDescriptor(out, KindBadRotate), nil
}

// vfrSegments describes a two-rate split whose gaps are counted exactly. The
// 30fps segment is the strict majority rate; the 60fps segment contributes the
// irregular half-rate gaps. The frame counts are chosen so the number of
// non-modal gaps is exactly 120, which is the figure the acceptance output
// reports. vfrFrames() re-derives the same count from the packet stream, so
// the fixture and the check cannot drift apart.
var vfrSegments = []struct {
	FPS     int
	Seconds float64
}{
	{FPS: 30, Seconds: 121.0 / 30.0},
	{FPS: 60, Seconds: 121.0 / 60.0},
}

// vfrIrregularGaps is the expected count of inter-frame gaps that differ from
// the modal gap in the generated VFR fixture.
const vfrIrregularGaps = 120

// generateVFR concatenates two clips at different frame rates into one MP4
// whose packets have genuinely uneven spacing. A 90 kHz track timescale makes
// every PTS an exact integer, so the irregular-gap count is reproducible.
func (g *Generator) generateVFR(name string, seed int64) (File, error) {
	path, err := g.EnsureSafePath(name)
	if err != nil {
		return File{}, err
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y", "-nostdin",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x90:rate=%d:duration=%g",
			vfrSegments[0].FPS, vfrSegments[0].Seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x90:rate=%d:duration=%g",
			vfrSegments[1].FPS, vfrSegments[1].Seconds),
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1[v]",
		"-map", "[v]", "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0",
		"-pix_fmt", "yuv420p", "-video_track_timescale", "90000", "-f", "mp4", path,
	}
	out, err := g.run(args)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: path, Stage: "fixture"},
			"fixture.generateVFR", "generating "+name+" failed", err)
	}
	if len(out) > 0 {
		return File{}, media.Errorf(media.KindInternal, media.Origin{File: path, Stage: "fixture"},
			"fixture.generateVFR", "ffmpeg failed: %s", strings.TrimSpace(string(out)))
	}
	return fileDescriptor(path, KindVFR), nil
}

// generateDrift builds a two-track clip whose audio clock is deliberately
// offset from video, so the drift monitor has something to observe.
func (g *Generator) generateDrift(src, dst string, offsetSeconds float64) (File, error) {
	in, err := g.EnsureSafePath(src)
	if err != nil {
		return File{}, err
	}
	out, err := g.EnsureSafePath(dst)
	if err != nil {
		return File{}, err
	}
	// itsoffset shifts the audio input's timestamps by the given amount while
	// copying both streams, so the drift is real container timing, not a
	// decode-side fiction.
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y", "-nostdin",
		"-i", in,
		"-itsoffset", fmt.Sprintf("%.6f", offsetSeconds), "-i", in,
		"-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-f", "mp4", out,
	}
	msg, err := g.run(args)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out, Stage: "fixture"},
			"fixture.generateDrift", "generating "+dst+" failed", err)
	}
	if len(msg) > 0 {
		return File{}, media.Errorf(media.KindInternal, media.Origin{File: out, Stage: "fixture"},
			"fixture.generateDrift", "ffmpeg failed: %s", strings.TrimSpace(string(msg)))
	}
	return fileDescriptor(out, KindDrift), nil
}

// generateCancel builds a long clip so a cancellation can land mid-stream.
func (g *Generator) generateCancel(name string, seconds float64) (File, error) {
	path, err := g.EnsureSafePath(name)
	if err != nil {
		return File{}, err
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y", "-nostdin",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=160x90:rate=30:duration=%g", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%g", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-g", "60",
		"-pix_fmt", "yuv420p", "-video_track_timescale", "90000",
		"-c:a", "aac", "-b:a", "96000", "-f", "mp4", path,
	}
	out, err := g.run(args)
	if err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: path, Stage: "fixture"},
			"fixture.generateCancel", "generating "+name+" failed", err)
	}
	if len(out) > 0 {
		return File{}, media.Errorf(media.KindInternal, media.Origin{File: path, Stage: "fixture"},
			"fixture.generateCancel", "ffmpeg failed: %s", strings.TrimSpace(string(out)))
	}
	return fileDescriptor(path, KindCancel), nil
}

// Set is the complete fixture catalog for a (dir, seed).
type Set struct {
	Dir   string
	Seed  int64
	Files []File
}

// GenerateAll builds the full fixture catalog under dir. It is deterministic:
// the same dir (empty) and seed always produce the same names and contents.
func GenerateAll(dir string, seed int64) (Set, error) {
	g, err := New(dir)
	if err != nil {
		return Set{}, err
	}
	rng := newLCG(seed)
	_ = rng
	specs := []Spec{
		{Kind: KindGood, Name: "good.mxf", Format: "mxf", Seed: seed, Video: true, Audio: true, Seconds: 2},
		{Kind: KindGood, Name: "good.mp4", Format: "mp4", Seed: seed, Video: true, Audio: true, Seconds: 2},
		{Kind: KindGood, Name: "cropbase.mp4", Format: "mp4", Seed: seed, Video: true, Seconds: 1,
			Rational: []string{"-vf", "crop=140:80:10:5"}},
		{Kind: KindWrap, Name: "wrap.mp4", Format: "mp4", Seed: seed, Video: true, Seconds: 2},
	}
	set := Set{Dir: g.Root(), Seed: seed}
	for _, s := range specs {
		f, err := g.Generate(s)
		if err != nil {
			return Set{}, err
		}
		set.Files = append(set.Files, f)
	}
	// VFR is produced by concatenating one second at 30fps and one at 24fps,
	// yielding genuine variable inter-frame spacing rather than a constant
	// average rate.
	vfr, err := g.generateVFR("vfr.mp4", seed)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, vfr)
	// The invalid-crop fixture starts from a real cropped MP4 and declares a
	// 170-wide display area, wider than the 160 coded frame.
	badCrop, err := g.OverrideTrackHeaderSize("cropbase.mp4", "badcrop.mp4", 170, 90)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, badCrop)
	// The bad-rotation fixture carries a real 45-degree display matrix, which
	// is not a legal right angle. It is produced by remuxing a base file with
	// ffmpeg's display_rotation input option.
	badRot, err := g.applyDisplayRotation("cropbase.mp4", "badrot.mp4", 45)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, badRot)
	// Corrupt variants derived from the good files.
	goodMXF := filepath.Join(g.Root(), "good.mxf")
	goodMP4 := filepath.Join(g.Root(), "good.mp4")
	info, err := os.Stat(goodMXF)
	if err != nil {
		return Set{}, media.New(media.KindInternal, media.Origin{File: goodMXF}, "fixture.GenerateAll", "stat failed", err)
	}
	trunc, err := g.Truncate("good.mxf", "trunc.mxf", info.Size()/2)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, trunc)
	cinfo, err := os.Stat(goodMP4)
	if err != nil {
		return Set{}, media.New(media.KindInternal, media.Origin{File: goodMP4}, "fixture.GenerateAll", "stat failed", err)
	}
	corrupt, err := g.CorruptKLV("good.mp4", "corrupt.mp4", cinfo.Size()-64)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, corrupt)
	sort.Slice(set.Files, func(i, j int) bool { return set.Files[i].Path < set.Files[j].Path })
	return set, nil
}

type lcg struct{ s uint64 }

func newLCG(seed int64) *lcg { return &lcg{s: uint64(seed)*6364136223846793005 + 1442695040888963407} }

func (l *lcg) next() uint64 {
	l.s = l.s*6364136223846793005 + 1442695040888963407
	return l.s >> 33
}
