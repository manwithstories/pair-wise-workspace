package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BuildOptions controls fixture generation.
type BuildOptions struct {
	Profile Profile
	OutDir  string
	// DurationSec is the source length; 8 seconds yields ~200 frames at 25 fps.
	DurationSec int
	// FFmpeg overrides the discovered binary (tests inject a stub).
	FFmpeg string
	// ForceVFR forces the VFR timing mode on every level when set.
	ForceVFR bool
}

// Result reports what was written.
type Result struct {
	ManifestPath string
	Manifest     Manifest
}

// Build encodes the ladder and injects the profile's faults. It never reads
// media outside OutDir.
func Build(opts BuildOptions) (*Result, error) {
	if opts.OutDir == "" {
		return nil, fmt.Errorf("fixture: output dir required")
	}
	if opts.DurationSec <= 0 {
		opts.DurationSec = 8
	}
	if opts.Profile == "" {
		opts.Profile = ProfileClean
	}
	ff, err := resolveFFmpeg(opts.FFmpeg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	manifest := Manifest{
		Profile:        string(opts.Profile),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Dir:            opts.OutDir,
		SharedTimeBase: "microseconds",
		DurationUS:     int64(opts.DurationSec) * 1_000_000,
		FPS:            25,
		FrameCount:     opts.DurationSec * 25,
		Expected: Expectation{
			MaxBoundaryDeltaUS: 2000,
			FrameCount:         opts.DurationSec * 25,
		},
	}
	levels := LevelSet()
	if opts.Profile == ProfileMixed {
		levels[0].Timing = CFR
		levels[1].Timing = CFR
		levels[2].Timing = VFR
	}
	if opts.ForceVFR {
		for i := range levels {
			levels[i].Timing = VFR
		}
	}

	for _, l := range levels {
		entry, err := encodeLevel(ff, opts, l)
		if err != nil {
			return nil, err
		}
		manifest.Levels = append(manifest.Levels, entry)
	}

	if opts.Profile == ProfileMixed {
		if err := injectMixed(opts.OutDir, &manifest); err != nil {
			return nil, err
		}
	}
	// Recompute hashes and segment counts after every mutation so the manifest
	// always describes the bytes a recovery run will read.
	for i := range manifest.Levels {
		if err := refreshEntry(&manifest.Levels[i]); err != nil {
			return nil, err
		}
	}
	manifest.Expected.MinRecoverable = minAccepted(manifest.Levels)

	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(opts.OutDir, "manifest.json")
	if err := WriteFile(manifestPath, append(payload, '\n')); err != nil {
		return nil, err
	}
	return &Result{ManifestPath: manifestPath, Manifest: manifest}, nil
}

func minAccepted(levels []Entry) int {
	// Every level loses at most the truncated segment; a dropped segment in
	// one level is still recoverable by the others.
	min := 1 << 30
	for _, e := range levels {
		if e.SegmentCount < min {
			min = e.SegmentCount
		}
	}
	if min == 1<<30 {
		return 0
	}
	return min
}

func resolveFFmpeg(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("ffmpeg not found (install with: brew install ffmpeg)")
}

// encodeLevel runs FFmpeg twice (video+audio stripped to fragments in one pass)
// and writes a CMAF-ish fMP4 file named after the level.
func encodeLevel(ff string, opts BuildOptions, l Level) (Entry, error) {
	out := filepath.Join(opts.OutDir, l.Name+".mp4")
	// GOP is one second so each fragment starts on a keyframe.
	gop := int(l.FPS)
	if gop < 1 {
		gop = 25
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%g:duration=%d", l.Width, l.Height, l.FPS, opts.DurationSec),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=%d", 330+l.BitrateK, opts.DurationSec),
	}
	if l.Timing == VFR {
		// Stretch every other frame by 12 ms at 25 fps: PTS alternates and the
		// average cadence stays 25/s, so the median-duration clock fit holds.
		args = append(args, "-vf", `setpts=(N+0.3*mod(N\,2))`, "-fps_mode", "vfr")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-bf", "0",
		"-g", fmt.Sprint(gop), "-keyint_min", fmt.Sprint(gop), "-sc_threshold", "0",
		"-b:v", fmt.Sprintf("%dk", l.BitrateK),
		"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", l.AudioBitrateK),
		"-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-frag_duration", "500000", // about 0.5 s per fragment
		out,
	)
	cmd := exec.Command(ff, args...)
	cmd.Stderr = &strings.Builder{}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Entry{}, fmt.Errorf("encode %s: %w: %s", l.Name, err, tailLog(stderr.String()))
	}
	e := Entry{
		Level:      l.Name,
		Path:       out,
		BitrateK:   l.BitrateK,
		Width:      l.Width,
		Height:     l.Height,
		FPS:        l.FPS,
		Timing:     l.Timing,
		DurationUS: int64(opts.DurationSec) * 1_000_000,
	}
	if err := refreshEntry(&e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func tailLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[len(s)-300:]
	}
	return s
}

// refreshEntry recomputes size, digest, segment count and timescale.
func refreshEntry(e *Entry) error {
	digest, size, err := fileSHA(e.Path)
	if err != nil {
		return err
	}
	e.SHA256 = digest
	e.SizeBytes = size
	data, err := os.ReadFile(e.Path)
	if err != nil {
		return err
	}
	e.SegmentCount = len(scanSpans(data))
	if ts := videoTimescale(data); ts != 0 {
		e.TimeScale = ts
	}
	return nil
}

// videoTimescale returns the media timescale of the first video track's mdhd.
// Movie and media timescales differ in these fixtures, so mvhd must not be
// used as the tick base for tfdt/trun arithmetic.
func videoTimescale(data []byte) uint32 {
	p, size, ok := findBox(data, 0, "moov")
	if !ok {
		return 0
	}
	var ts uint32
	_ = walkRegion(data[p:p+size], 8, func(typ string, off, sz int) error {
		if typ != "trak" || ts != 0 {
			return nil
		}
		// Recurse with an absolute-offset walk into the trak payload. The
		// child boxes start after the box header, so pass 8.
		trakPayload := data[int(p)+off+8 : int(p)+off+sz]
		var handler string
		var trackTS uint32
		var haveTS bool
		_ = walkRegion(trakPayload, 0, func(ct string, coff, csz int) error {
			if ct != "mdia" {
				return nil
			}
			mdia := trakPayload[coff+8 : coff+csz]
			_ = walkRegion(mdia, 0, func(it string, ioff, isz int) error {
				body := mdia[ioff+8 : ioff+isz]
				switch it {
				case "mdhd":
					if len(body) >= 24 && body[0] == 0 {
						trackTS = binaryBig(body[12:16])
						haveTS = true
					} else if len(body) >= 36 && body[0] == 1 {
						trackTS = binaryBig(body[20:24])
						haveTS = true
					}
				case "hdlr":
					if len(body) >= 12 {
						handler = string(body[8:12])
					}
				}
				return nil
			})
			return nil
		})
		if haveTS && handler == "vide" {
			ts = trackTS
		}
		return nil
	})
	return ts
}

func binaryBig(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

// trackTimescales maps each track id to its mdhd timescale.
func trackTimescales(data []byte) map[uint32]int64 {
	out := map[uint32]int64{}
	p, size, ok := findBox(data, 0, "moov")
	if !ok {
		return out
	}
	moov := data[p : p+size]
	_ = walkRegion(moov, 8, func(typ string, off, sz int) error {
		if typ != "trak" {
			return nil
		}
		trak := moov[off : off+sz]
		var id uint32
		var ts int64
		_ = walkRegion(trak, 8, func(ct string, coff, csz int) error {
			body := trak[coff+8 : coff+csz]
			switch ct {
			case "tkhd":
				if len(body) >= 20 && body[0] == 0 {
					id = binaryBig(body[12:16])
				} else if len(body) >= 28 && body[0] == 1 {
					id = binaryBig(body[20:24])
				}
			case "mdia":
				mdia := trak[coff+8 : coff+csz]
				_ = walkRegion(mdia, 0, func(it string, ioff, isz int) error {
					ibody := mdia[ioff+8 : ioff+isz]
					if it == "mdhd" {
						if len(ibody) >= 24 && ibody[0] == 0 {
							ts = int64(binaryBig(ibody[12:16]))
						} else if len(ibody) >= 36 && ibody[0] == 1 {
							ts = int64(binaryBig(ibody[20:24]))
						}
					}
					return nil
				})
			}
			return nil
		})
		if id != 0 && ts != 0 {
			out[id] = ts
		}
		return nil
	})
	return out
}

// DebugVideoTimescale exposes the extractor for local diagnosis.
func DebugVideoTimescale(data []byte) uint32 { return videoTimescale(data) }

func fileSHA(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}
