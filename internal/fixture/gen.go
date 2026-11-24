// Package fixture deterministically generates all sample capture trees used by
// demos and tests. No external media assets are referenced.
package fixture

import (
	"math/rand"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediad/internal/metadata"
	"mediad/internal/mp4"
)

// FragHook optionally overrides one fragment's sequence/tfdt/RTP.
type FragHook struct {
	Sequence *uint32
	TFDT     *uint64
	NoRTP    bool
	RTPTS    *uint32
}

// Rep describes one representation ladder rung.
type Rep struct {
	Name           string
	Width, Height  uint16
	Timescale      uint32
	FrameDuration  uint32
	FramesPerFrag  int
	Color          metadata.Color
	Audio          metadata.Audio
	AudioTimescale uint32
	Hooks          map[int]FragHook
}

// Loc addresses one fragment by representation name and 0-based index.
type Loc struct {
	Rep   string
	Index int
}

// Damage is a post-write corruption.
type Damage struct {
	Rep   string
	Index int
	Kind  string // "oob_count" | "truncate"
}

// Config drives Generate.
type Config struct {
	Root            string
	Seed            int64
	Camera          string
	FragmentsPerRep int
	Reps            []Rep
	Omit            []Loc
	Damage          []Damage
	ExtraDuplicate  bool
}

// HighRep returns the default high-bitrate rung (1080p, 90k).
func HighRep() Rep {
	return Rep{
		Name: "high", Width: 1920, Height: 1080,
		Timescale: 90000, FrameDuration: 3000, FramesPerFrag: 6,
		Color: defaultColor(), Audio: defaultAudio(), AudioTimescale: 48000,
	}
}

// MidRep returns the 720p rung on a 48k time base.
func MidRep() Rep {
	r := HighRep()
	r.Name, r.Width, r.Height = "mid", 1280, 720
	r.Timescale, r.FrameDuration = 48000, 1600
	return r
}

// LowRep returns the 480p rung on a 30k time base.
func LowRep() Rep {
	r := HighRep()
	r.Name, r.Width, r.Height = "low", 854, 480
	r.Timescale, r.FrameDuration = 30000, 1000
	return r
}

// DefaultReps returns high/mid/low with compatible metadata.
func DefaultReps() []Rep { return []Rep{HighRep(), MidRep(), LowRep()} }

func defaultColor() metadata.Color {
	return metadata.Color{ColorType: "nclx", Primaries: 1, Transfer: 1, Matrix: 1, FullRange: false}
}

func defaultAudio() metadata.Audio {
	return metadata.Audio{
		Channels: 6, SampleRateHz: 48000, SampleRateRaw: 48000 << 16,
		LayoutCode: 6, Layout: "L R C LFE Ls Rs",
	}
}

func (c Config) omitted(rep string, idx int) bool {
	for _, l := range c.Omit {
		if l.Rep == rep && l.Index == idx {
			return true
		}
	}
	return false
}

func segBase(i int) string {
	return "seg_" + pad5(uint32(i+1))
}

func pad5(v uint32) string {
	const digits = "0123456789"
	b := []byte("00000")
	for i := 4; i >= 0; i-- {
		b[i] = digits[v%10]
		v /= 10
	}
	return string(b)
}

// Generate writes the configured capture tree and applies damage/omissions.
func Generate(cfg Config) error {
	if cfg.FragmentsPerRep == 0 {
		cfg.FragmentsPerRep = 8
	}
	if cfg.Reps == nil {
		cfg.Reps = DefaultReps()
	}
	if cfg.Camera == "" {
		cfg.Camera = "cam01"
	}
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return err
	}
	omitSet := map[Loc]bool{}
	for _, l := range cfg.Omit {
		omitSet[l] = true
	}

	for ri, rep := range cfg.Reps {
		dir := filepath.Join(cfg.Root, cfg.Camera, rep.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		initBytes, err := mp4.EncodeInit(mp4.InitSpec{
			Width: rep.Width, Height: rep.Height, VideoTimescale: rep.Timescale,
			Color: &rep.Color, AudioTimescale: rep.AudioTimescale, Audio: &rep.Audio,
		})
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "init.mp4"), initBytes, 0o644); err != nil {
			return err
		}

		fragDur := uint64(rep.FrameDuration) * uint64(rep.FramesPerFrag)
		for i := 0; i < cfg.FragmentsPerRep; i++ {
			if omitSet[Loc{Rep: rep.Name, Index: i}] {
				continue
			}
			hook := rep.Hooks[i]
			seq := uint32(i + 1)
			if hook.Sequence != nil {
				seq = *hook.Sequence
			}
			base := uint64(i) * fragDur
			if hook.TFDT != nil {
				base = *hook.TFDT
			}
			frag, rtpTS := buildFragment(cfg.Seed, ri, i, seq, base, rep)
			raw, err := mp4.EncodeFragment(frag)
			if err != nil {
				return err
			}
			baseName := segBase(i)
			if err := os.WriteFile(filepath.Join(dir, baseName+".m4s"), raw, 0o644); err != nil {
				return err
			}
			if !hook.NoRTP {
				ts := rtpTS
				if hook.RTPTS != nil {
					ts = *hook.RTPTS
				}
				pkt := rtp.Packet{
					Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(i + 1), Timestamp: ts, SSRC: 0x5eed},
					Payload: []byte("mediad-cmaf"),
				}
				rb, err := pkt.Marshal()
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dir, baseName+".rtp"), rb, 0o644); err != nil {
					return err
				}
			}
		}

		if cfg.ExtraDuplicate && ri == 0 {
			src := filepath.Join(dir, segBase(0)+".m4s")
			if b, err := os.ReadFile(src); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "seg_00099.m4s"), b, 0o644)
			}
		}
	}

	for _, d := range cfg.Damage {
		if err := applyDamage(cfg, d); err != nil {
			return err
		}
	}
	return nil
}

func buildFragment(seed int64, repIdx, i int, seq uint32, base uint64, rep Rep) (*mp4.Fragment, uint32) {
	rng := rand.New(rand.NewSource(seed + int64(repIdx)*1009 + int64(i)*37))
	n := rep.FramesPerFrag
	if n <= 0 {
		n = 6
	}
	samples := make([]mp4.Sample, n)
	var total int
	for s := range samples {
		size := 200 + rng.Intn(500)
		if s == 0 {
			size = 3000 + rng.Intn(2000)
		}
		total += size
		flags := uint32(0x01010000) // non-sync
		cts := int32(((s + i) % 3)) * 512
		if s == 0 {
			flags = 0x02000000 // sync
			cts = 0
		}
		samples[s] = mp4.Sample{
			Duration: rep.FrameDuration, Size: uint32(size), Flags: flags, CTOffset: cts,
		}
	}
	payload := make([]byte, total)
	_, _ = rng.Read(payload)

	// Continuous 90 kHz RTP timestamp for this fragment boundary.
	fragDur90 := uint64(rep.FrameDuration) * uint64(n)
	rtpTS64 := uint64(i) * fragDur90 * 90000 / uint64(rep.Timescale)
	return &mp4.Fragment{
		Sequence: seq, TrackID: 1, BaseDecode: base, TFDTVersion: 1,
		Samples: samples, Data: payload,
	}, uint32(rtpTS64)
}
