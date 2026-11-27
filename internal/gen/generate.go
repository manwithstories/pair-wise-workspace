// Package gen produces the deterministic demo input set: synthetic PNG
// frames on three tracks with drift, a 32-bit tick wrap, injected holes and
// corrupt frames, plus a 48 kHz calibration audio file. All bytes come from
// Go code; no external sample is referenced.
package gen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"

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
	Frames     int
}

// DefaultTracks returns the demo's three-camera plan.
func DefaultTracks(width, height int) []TrackPlan {
	w, h := width, height
	return []TrackPlan{
		{Name: "cam_a", RateNum: 25, RateDen: 1, Priority: 10, Frames: 0,
			Mask: ingest.Mask{}},
		{Name: "cam_b", RateNum: 25, RateDen: 1, Priority: 50, Frames: 0, Deps: []string{"cam_a"},
			Mask: ingest.Mask{X: w / 4, Y: h / 4, W: w / 2, H: h / 2}},
		{Name: "cam_c", RateNum: 30, RateDen: 1, Priority: 80, Frames: 0, Deps: []string{"cam_b"},
			Mask: ingest.Mask{X: 0, Y: 0, W: w / 3, H: h / 3}, OffsetTick: 40_000},
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
	res.AudioPCM, err = audio.SyntheticPCM(samples, media.AudioRate, 0, period, 96, 0.8)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "audio.pcm"), res.AudioPCM, 0o644); err != nil {
		return nil, media.New(media.KindConflict, "gen.audio", "", -1, err)
	}
	startTick := cfg.WrapAt - int64(cfg.Frames)*40_000
	if cfg.Tracks == 3 {
		startTick = cfg.WrapAt - int64(cfg.Frames)*40_000
	}
	for ti, tr := range tracks {
		tr.Frames = cfg.Frames
		// Each camera starts at a slightly different wall time, so the shared
		// origin lands on the earliest camera.
		baseNanos := int64(ti) * 20_000_000
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
			c := Camera{Track: tr.Name, Seq: seq}
			// Variable frame rate: 25 fps tracks jitter ±4 ms; the 30 fps
			// track keeps a tighter cadence, then all map onto 25 fps.
			var interval int64
			switch {
			case tr.RateDen == 1 && tr.RateNum == 30:
				interval = int64((seq * 1e9) / 30)
			case seq == 0:
				interval = 0 // frame 0 defines the shared origin exactly
			default:
				interval = seq*40_000_000 + int64(rng.Intn(8_000)-4_000)
			}
			tick := startTick + interval/1000
			tick %= cfg.WrapAt
			drift := 1 + cfg.DriftPPM*1e-6
			c.Nanos = baseNanos + int64(float64(interval)*drift)
			c.Tick = tick
			c.Corrupt = seq > 0 && seq%int64(cfg.CorruptEvery) == 0
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
