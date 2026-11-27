// Package gen produces the deterministic demo input set: synthetic PNG
// frames on three tracks with drift, a 32-bit tick wrap, injected holes and
// corrupt frames, plus a 48 kHz calibration audio file. All bytes come from
// Go code; no external sample is referenced.
package gen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"fieldcut/internal/audio"
	"fieldcut/internal/ingest"
	"fieldcut/internal/media"
)

// Config drives the generated set.
type Config struct {
	Seed         int64
	Frames       int
	Tracks       int
	WrapAt       int64
	DriftPPM     float64
	DropEvery    int
	CorruptEvery int
	Width        int
	Height       int
	Chunk        int
}

// TrackPlan is the per-track geometry written into the manifest.
type TrackPlan struct {
	Name       string
	RateNum    int
	RateDen    int
	Priority   int
	Mask       ingest.Mask
	Deps       []string
	OffsetTick int64
	JitterUs   int
	Frames     int
}

// DefaultTracks returns the demo's three-camera plan.
func DefaultTracks(width, height int) []TrackPlan {
	w, h := width, height
	return []TrackPlan{
		{Name: "cam_a", RateNum: 25, RateDen: 1, Priority: 10, JitterUs: 1000,
			Mask: ingest.Mask{}},
		{Name: "cam_b", RateNum: 25, RateDen: 1, Priority: 50, Deps: []string{"cam_a"},
			Mask: ingest.Mask{X: w / 4, Y: h / 4, W: w / 2, H: h / 2}, OffsetTick: 2000,
			JitterUs: 1000},
		{Name: "cam_c", RateNum: 25, RateDen: 1, Priority: 80, Deps: []string{"cam_b"},
			Mask: ingest.Mask{X: 0, Y: 0, W: w / 3, H: h / 3}, OffsetTick: 4000,
			JitterUs: 1000},
	}
}

// Camera is one raw capture the demo writes.
type Camera struct {
	Track   string
	Seq     int64
	Tick    int64
	Nanos   int64
	Corrupt bool
	Missing bool
	Path    string
}

// Result reports what was generated.
type Result struct {
	Dir      string
	Manifest *ingest.Manifest
	Cameras  []Camera
	AudioPCM []byte
	Width    int
	Height   int
}

// Generate writes frames, audio.pcm and manifest.json under dir.
func Generate(dir string, cfg Config) (*Result, error) {
	if cfg.Frames <= 0 || cfg.Frames > media.MaxOutputFrames {
		return nil, media.Newf(media.KindInput, "gen.frames", "", -1, "frames %d outside [1,%d]", cfg.Frames, media.MaxOutputFrames)
	}
	if cfg.Tracks <= 0 || cfg.Tracks > media.MaxTracks {
		return nil, media.Newf(media.KindLimit, "gen.tracks", "", -1, "tracks %d outside [1,%d]", cfg.Tracks, media.MaxTracks)
	}
	if cfg.WrapAt <= 0 || cfg.WrapAt > 1<<32 {
		return nil, media.Newf(media.KindClock, "gen.wrap", "", -1, "wrap %d outside (0,2^32]", cfg.WrapAt)
	}
	if cfg.Width <= 0 || cfg.Width > media.MaxWidth || cfg.Height <= 0 || cfg.Height > media.MaxHeight {
		return nil, media.Newf(media.KindInput, "gen.canvas", "", -1, "canvas %dx%d out of range", cfg.Width, cfg.Height)
	}
	if cfg.DropEvery <= 0 {
		cfg.DropEvery = 37
	}
	if cfg.CorruptEvery <= 0 {
		cfg.CorruptEvery = 53
	}
	if err := os.MkdirAll(filepath.Join(dir, "frames"), 0o755); err != nil {
		return nil, media.New(media.KindConflict, "gen.dir", "", -1, err)
	}
	tracks := DefaultTracks(cfg.Width, cfg.Height)
	if cfg.Tracks < len(tracks) {
		tracks = tracks[:cfg.Tracks]
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	res := &Result{Dir: dir, Width: cfg.Width, Height: cfg.Height}
	man := &ingest.Manifest{
		Version: 1, OutputFPS: media.FrameRate, Frames: cfg.Frames,
		Width: cfg.Width, Height: cfg.Height, WrapAt: cfg.WrapAt, Chunk: cfg.Chunk,
	}
	// Audio: one calibration pulse every 400 ms from frame 0.
	period := media.AudioRate * 2 / 5
	samples := period * (1 + cfg.Frames/media.FrameRate + 1)
	var err error
	// Pulse 0 sits 1 ms late: a deliberate, measurable alignment offset the
	// pipeline must report rather than assume away.
	res.AudioPCM, err = audio.SyntheticPCM(samples, media.AudioRate, 48, period, 96, 0.8)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "audio.pcm"), res.AudioPCM, 0o644); err != nil {
		return nil, media.New(media.KindConflict, "gen.audio", "", -1, err)
	}
	// Start half a timeline before the modulus so a real 32-bit wrap
	// happens mid-sequence rather than at the very end.
	startTick := cfg.WrapAt - int64(cfg.Frames/2)*40_000
	for ti, tr := range tracks {
		tr.Frames = cfg.Frames
		// Each camera starts at a slightly different wall time, so the shared
		// origin lands on the earliest camera and its offset is visible.
		baseNanos := int64(ti) * 5_000_000
		man.Tracks = append(man.Tracks, ingest.Track{
			Name: tr.Name, OffsetTick: tr.OffsetTick, Frames: cfg.Frames,
			Priority: tr.Priority, Mask: tr.Mask, Deps: tr.Deps,
			RateNum: tr.RateNum, RateDen: tr.RateDen,
		})
		for seq := int64(0); seq < int64(cfg.Frames); seq++ {
			if seq%int64(cfg.DropEvery) == 0 && seq > 0 {
				res.Cameras = append(res.Cameras, Camera{Track: tr.Name, Seq: seq, Missing: true})
				continue
			}
			// One deliberately long gap on the last track stays unrecoverable
			// and must surface as explicit missing records, never as success.
			if ti == len(tracks)-1 && seq == int64(cfg.Frames*3/5) {
				for k := int64(0); k < 3; k++ {
					res.Cameras = append(res.Cameras, Camera{Track: tr.Name, Seq: seq + k, Missing: true})
				}
				seq += 3
				continue
			}
			c := Camera{Track: tr.Name, Seq: seq}
			// Variable frame rate: 25 fps tracks jitter ±4 ms; the 30 fps
			// track keeps a tighter cadence, then all map onto 25 fps.
			var interval int64
			if seq == 0 {
				interval = 0 // frame 0 defines the shared origin exactly
			} else if seq == int64(cfg.Frames-1) {
				// The last capture stays on the nominal cadence so every
				// frame maps inside the declared output timeline.
				interval = seq * 40_000_000
			} else {
				// Variable frame rate: nominal 25 fps with per-camera jitter,
				// bounded to ±1 ms so cumulative delivery stays in range.
				j := 0
				if tr.JitterUs > 0 {
					j = rng.Intn(2*tr.JitterUs+1) - tr.JitterUs
				}
				interval = seq*40_000_000 + int64(j)*1000
			}
			tick := startTick + interval/1000
			tick %= cfg.WrapAt
			drift := 1 + cfg.DriftPPM*1e-6
			c.Nanos = baseNanos + int64(float64(interval)*drift)
			c.Tick = tick
			c.Corrupt = seq > 0 && seq%int64(cfg.CorruptEvery) == 0
			// One corrupt frame sits directly next to an injected hole so
			// recovery must bridge a corrupt+missing run.
			if ti == 1 && seq == int64(cfg.DropEvery)*2-1 {
				c.Corrupt = true
			}
			c.Path = fmt.Sprintf("frames/%s/%05d.png", tr.Name, seq)
			if c.Corrupt {
				writeCorrupt(filepath.Join(dir, c.Path), int(seq))
				c.Tick = tick
			} else {
				img := renderFrame(cfg.Width, cfg.Height, int(seq), ti)
				if err := writePNG(filepath.Join(dir, c.Path), img); err != nil {
					return nil, media.New(media.KindConflict, "gen.png", tr.Name, seq, err)
				}
			}
			sum, err := ingest.HashFile(filepath.Join(dir, c.Path))
			if err != nil {
				return nil, media.New(media.KindConflict, "gen.hash", tr.Name, seq, err)
			}
			man.Entries = append(man.Entries, ingest.Entry{
				Track: tr.Name, Seq: seq, Tick: c.Tick, Nanos: c.Nanos,
				Width: cfg.Width, Height: cfg.Height, Path: c.Path, SHA256: sum,
			})
			res.Cameras = append(res.Cameras, c)
		}
	}
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, media.New(media.KindConflict, "gen.manifest", "", -1, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		return nil, media.New(media.KindConflict, "gen.manifest", "", -1, err)
	}
	res.Manifest = man
	return res, nil
}

// renderFrame draws a deterministic scene: a moving marker whose position
// encodes frame and camera, so visual inspection reveals misalignment.
func renderFrame(w, h, seq, track int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	hue := uint8((seq * 3) % 256)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 4), G: uint8(y * 4), B: hue, A: 255})
		}
	}
	cx := (seq * 5) % w
	cy := h/2 + track*8
	for y := cy - 6; y < cy+6; y++ {
		for x := cx - 6; x < cx+6; x++ {
			if x >= 0 && y >= 0 && x < w && y < h {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}
	// Camera tint band so masks are visible.
	for y := 0; y < h; y++ {
		img.SetNRGBA(0, y, color.NRGBA{R: uint8(40 * track), G: 255, B: 0, A: 255})
	}
	return img
}

func writePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// writeCorrupt writes a truncated, undecodable PNG and its true hash, so the
// frame passes ingest hashing and fails decode.
func writeCorrupt(path string, nonce int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data := []byte("truncated-frame-")
	data = append(data, []byte(fmt.Sprintf("%08d", nonce))...)
	return os.WriteFile(path, data, 0o644)
}

// ManifestHash fingerprints the generated configuration for cache validity.
func (c Config) ManifestHash() string {
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Mutators are test/demo hooks that rewrite one manifest row after
// generation to exercise every rejection path without external files.
//
// They operate on manifest.json only; frame bytes are untouched.
func readManifest(dir string) (*ingest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, media.New(media.KindInput, "gen.manifest.read", "", -1, err)
	}
	var m ingest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, media.New(media.KindInput, "gen.manifest.read", "", -1, err)
	}
	return &m, nil
}

func writeManifest(dir string, m *ingest.Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return media.New(media.KindConflict, "gen.manifest.write", "", -1, err)
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o644)
}

// Mutate applies a named corruption to the generated manifest so the demo
// can show the corresponding rejection exit code.
func Mutate(dir, kind string) error {
	m, err := readManifest(dir)
	if err != nil {
		return err
	}
	if len(m.Entries) == 0 {
		return media.New(media.KindInput, "gen.mutate", "", -1, errors.New("empty manifest"))
	}
	e := &m.Entries[0]
	switch kind {
	case "bad-hash":
		e.SHA256 = strings.Repeat("0", 64)
	case "traversal":
		e.Path = "../escape.png"
	case "oversize":
		e.Width = media.MaxWidth + 1
	case "cycle":
		m.Tracks[0].Deps = []string{m.Tracks[len(m.Tracks)-1].Name}
	case "fps":
		m.OutputFPS = 30
	default:
		return media.Newf(media.KindInput, "gen.mutate", "", -1, "unknown mutation %q", kind)
	}
	return writeManifest(dir, m)
}
