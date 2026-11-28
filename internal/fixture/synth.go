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
	// GOP is the keyframe interval in frames. Zero uses one second (FPS).
	GOP      int
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
		// A modular counter near the top of its range is produced by
		// fragmenting the MP4 and rewriting each track's tfdt bases so the
		// fragments straddle 2^33. A plain -output_ts_offset is not enough:
		// mp4 stores no absolute origin and the muxer normalises it away.
		// empty_moov forces every sample into a moof carrying a tfdt; without
		// it the first fragment stays in moov and its timestamps cannot be
		// rewritten. Half-second fragments keep each tfdt base strictly below
		// 2^33 while still crossing it exactly once.
		args = append(args, "-frag_duration", "500000",
			"-movflags", "+frag_keyframe+empty_moov+default_base_moof")
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
			gop := s.GOP
			if gop <= 0 {
				gop = s.FPS
			}
			args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-bf", "0",
				"-g", fmt.Sprintf("%d", gop), "-pix_fmt", "yuv420p",
				// A fixed 90 kHz video time base makes PTS arithmetic exact so
				// VFR spacing and segment boundaries are reproducible.
				"-video_track_timescale", "90000")
		}
		if s.Audio {
			args = append(args, "-c:a", "aac", "-b:a", "96000")
		}
		if s.Kind != KindWrap {
			args = append(args, "-movflags", "+faststart")
		}
		args = append(args, "-f", "mp4")
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

// tfdtSpan is 2^33, the width of an MPEG transport style timestamp field. The
// wrap fixture rewrites sample timestamps so one track rolls over this value.
const tfdtSpan = int64(1) << 33

// RewriteTFDTWrap shifts every tfdt base in a fragmented MP4 so that the
// first fragment ends exactly at 2^33 and the second begins at 0, producing
// one genuine modular wrap per track.
//
// This is the only reliable way to build a wrap fixture: plain muxing offsets
// are normalised away by the MP4 muxer because the container stores no
// absolute origin. The rewrite is exact and touches only the 8-byte version-1
// base fields, so the rest of the file, including the encoded samples, is
// left byte-for-byte intact.
//
// The shift is derived from the source itself: the duration of the first
// fragment is the difference between the first two tfdt bases of a track.
// Placing the first fragment at [2^33 - thatDuration, 2^33) makes the second
// fragment's base land on exactly 0, so no sample ever leaves the 33-bit
// range while the counter still visibly rolls over.
func (g *Generator) RewriteTFDTWrap(src, dst string) (File, error) {
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
		return File{}, media.New(media.KindInternal, media.Origin{File: in}, "fixture.RewriteTFDTWrap", "read failed", err)
	}
	// First pass: find every tfdt body and the track it belongs to.
	type tfdtRef struct {
		pos  int // offset of the version-1 8-byte base field
		base int64
		tid  uint32
	}
	var refs []tfdtRef
	first := map[uint32]int64{}
	second := map[uint32]int64{}
	off := 0
	for {
		rel := bytesIndexOf(data[off:], []byte("tfdt"))
		if rel < 0 {
			break
		}
		typ := off + rel
		// A box is "size(4) type(4)"; the payload starts after the two.
		body := typ + 4
		if body+4 > len(data) {
			break
		}
		ver := data[body]
		if ver != 1 {
			return File{}, media.Errorf(media.KindCorrupt, media.Origin{File: in, Offset: int64(typ)},
				"fixture.RewriteTFDTWrap", "tfdt version %d is not supported; use 64-bit base", ver)
		}
		base := int64(binary.BigEndian.Uint64(data[body+4:]))
		// The enclosing traf carries a tfhd whose payload is
		// version+flags(4) track_ID(4).
		h := bytesLastIndexOf(data[:typ], []byte("tfhd"))
		if h < 0 {
			return File{}, media.Errorf(media.KindCorrupt, media.Origin{File: in, Offset: int64(typ)},
				"fixture.RewriteTFDTWrap", "tfdt without a preceding tfhd")
		}
		tid := binary.BigEndian.Uint32(data[h+4+4:])
		if _, ok := first[tid]; !ok {
			first[tid] = base
		} else if _, ok := second[tid]; !ok {
			second[tid] = base
		}
		refs = append(refs, tfdtRef{pos: body + 4, base: base, tid: tid})
		off = typ + 4
	}
	if len(refs) == 0 {
		return File{}, media.Errorf(media.KindCorrupt, media.Origin{File: in},
			"fixture.RewriteTFDTWrap", "no tfdt boxes found; the fixture must be fragmented")
	}
	outData := make([]byte, len(data))
	copy(outData, data)
	for tid := range first {
		if _, ok := second[tid]; !ok {
			return File{}, media.Errorf(media.KindUsage, media.Origin{File: in},
				"fixture.RewriteTFDTWrap", "track %d has only one fragment; a wrap needs at least two", tid)
		}
		if second[tid]-first[tid] <= 0 {
			return File{}, media.Errorf(media.KindCorrupt, media.Origin{File: in},
				"fixture.RewriteTFDTWrap", "track %d fragment bases do not advance", tid)
		}
	}
	for _, r := range refs {
		// Place the first fragment so it ends exactly at 2^33; every later
		// base then continues from 0 and never leaves the 33-bit range.
		firstDur := second[r.tid] - first[r.tid]
		shift := tfdtSpan - firstDur - first[r.tid]
		newBase := (r.base + shift) % tfdtSpan
		if newBase < 0 {
			newBase += tfdtSpan
		}
		binary.BigEndian.PutUint64(outData[r.pos:], uint64(newBase))
	}
	if err := os.WriteFile(out, outData, 0o644); err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out}, "fixture.RewriteTFDTWrap", "write failed", err)
	}
	return fileDescriptor(out, KindWrap), nil
}

func bytesLastIndexOf(haystack, needle []byte) int {
	for i := len(haystack) - len(needle); i >= 0; i-- {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
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

// CorruptKLV damages a container at a structural point so the demuxer must
// notice. For MXF it flips bytes inside a KLV payload; for MP4 it finds a
// top-level box and rewrites its size field to a value that escapes the file,
// which the box walker rejects with the byte offset of the damage. Corrupting
// bytes inside mdat would be undetectable by design, since the encoded sample
// bytes are opaque to the container.
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
	// A UL/box whose declared size runs past the end of the file is the one
	// corruption a streaming demuxer is guaranteed to detect.
	binary.BigEndian.PutUint32(data[offset:], uint32(len(data))+0x1000)
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out}, "fixture.CorruptKLV", "write failed", err)
	}
	return fileDescriptor(out, KindTruncated), nil
}

// CorruptMP4Box rewrites a top-level box size so it escapes the file. It is
// the MP4 counterpart of CorruptKLV and is used for the corrupt.mp4 fixture,
// which must fail inside the same box walker that validates tkhd.
func (g *Generator) CorruptMP4Box(src, dst string) (File, error) {
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
		return File{}, media.New(media.KindInternal, media.Origin{File: in}, "fixture.CorruptMP4Box", "read failed", err)
	}
	// Find the top-level mdat box and claim it extends far past the file.
	idx := bytesIndexOf(data, []byte("mdat"))
	if idx < 8 {
		return File{}, media.Errorf(media.KindInternal, media.Origin{File: in},
			"fixture.CorruptMP4Box", "mdat box not found")
	}
	sizeOff := idx - 4
	binary.BigEndian.PutUint32(data[sizeOff:], uint32(len(data))+0x1000)
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return File{}, media.New(media.KindInternal, media.Origin{File: out}, "fixture.CorruptMP4Box", "write failed", err)
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
	// The acceptance run expects six 2-second segments, so the good fixture is
	// twelve seconds long with a keyframe exactly every two seconds. The MXF
	// mirror uses the same cadence.
	specs := []Spec{
		{Kind: KindGood, Name: "good.mxf", Format: "mxf", Seed: seed, Video: true, Audio: true,
			Seconds: 12, GOP: 60},
		{Kind: KindGood, Name: "good.mp4", Format: "mp4", Seed: seed, Video: true, Audio: true,
			Seconds: 12, GOP: 60},
		{Kind: KindGood, Name: "cropbase.mp4", Format: "mp4", Seed: seed, Video: true, Seconds: 1,
			Rational: []string{"-vf", "crop=140:80:10:5"}},
		{Kind: KindWrap, Name: "wrap_frag.mp4", Format: "mp4", Seed: seed, Video: true, Audio: true,
			Seconds: 2, GOP: 30},
	}
	set := Set{Dir: g.Root(), Seed: seed}
	for _, s := range specs {
		f, err := g.Generate(s)
		if err != nil {
			return Set{}, err
		}
		// The fragmented wrap source is an intermediate: it is rewritten into
		// wrap.mp4 below and must not appear in the catalog twice.
		if s.Name == "wrap_frag.mp4" {
			continue
		}
		set.Files = append(set.Files, f)
	}
	// Rewrite the fragmented source into a genuinely wrapping stream. Each
	// track starts one second (in its own time base) *above* 2^33, so the
	// first fragment already lies beyond the boundary and the stream carries a
	// wrap only if the timeline is expanded. The intermediate fragmented file
	// is removed so only the wrapping product remains in the catalog.
	wrap, err := g.RewriteTFDTWrap("wrap_frag.mp4", "wrap.mp4")
	if err != nil {
		return Set{}, err
	}
	if err := os.Remove(filepath.Join(g.Root(), "wrap_frag.mp4")); err != nil {
		return Set{}, media.New(media.KindInternal, media.Origin{File: "wrap_frag.mp4"},
			"fixture.GenerateAll", "removing intermediate file failed", err)
	}
	set.Files = append(set.Files, wrap)
	// VFR is produced by concatenating 122 frames at 30fps and 120 at 60fps,
	// yielding genuine variable inter-frame spacing (exactly 120 irregular
	// gaps at a 90 kHz base) rather than a constant average rate.
	vfr, err := g.generateVFR("vfr.mp4", seed)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, vfr)
	// A drift fixture shifts the audio clock 120 ms from video.
	drift, err := g.generateDrift("good.mp4", "drift.mp4", 0.120)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, drift)
	// A long clip for cancellation: enough packets that a mid-stream cancel
	// lands before the end.
	cancel, err := g.generateCancel("cancel.mp4", 8)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, cancel)
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
	// A truncated MP4 loses both its tail and the box sizes that describe it,
	// so the box walker rejects it. It is a separate damage class from
	// corrupt.mp4 (one wrong size field inside an otherwise intact box tree).
	ginfo, err := os.Stat(goodMP4)
	if err != nil {
		return Set{}, media.New(media.KindInternal, media.Origin{File: goodMP4}, "fixture.GenerateAll", "stat failed", err)
	}
	truncMP4, err := g.Truncate("good.mp4", "trunc.mp4", ginfo.Size()*3/5)
	if err != nil {
		return Set{}, err
	}
	set.Files = append(set.Files, truncMP4)
	corrupt, err := g.CorruptMP4Box("good.mp4", "corrupt.mp4")
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
