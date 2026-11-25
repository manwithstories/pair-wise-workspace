// Package fixture generates fully deterministic, self-contained sample
// packages: multi-channel PCM WAV with per-32-frame CRC32C, an RTP capture
// with wrap + drift, an H.264 proxy MP4 with variable keyframe spacing, and a
// manifest tying them together. No network or external codec is used.
package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"

	"mediaaudit/internal/wav"
)

// Options controls every fixture parameter so the generated package is a pure
// function of (Options, deterministic PRNG seed).
type Options struct {
	Dir           string
	Seed          int64
	Channels      int
	Frames        int64 // audio sample frames
	SampleRate    int64
	TileFrames    int64
	DamageFrame   int64 // first damaged audio frame; <0 disables damage
	WrapAt        uint32 // RTP starting timestamp near the 32-bit boundary
	DriftPPM      float64
	VFRPattern    []int  // keyframe gap pattern in base frames
}

// Manifest is the machine-readable package description and fingerprint source.
type Manifest struct {
	Seed         int64    `json:"seed"`
	Channels     int      `json:"channels"`
	Frames       int64    `json:"frames"`
	SampleRate   int64    `json:"sample_rate"`
	TileFrames   int64    `json:"tile_frames"`
	DamageFrame  int64    `json:"damage_frame"`
	DamageCount  int      `json:"damage_count"`
	WrapAt       uint32   `json:"wrap_at"`
	DriftPPM     float64  `json:"drift_ppm"`
	VFRPattern   []int    `json:"vfr_pattern"`
	Files        []string `json:"files"`
	Fingerprint  string   `json:"fingerprint"`
}

// Names of the generated artifacts inside the package directory.
const (
	AudioFile    = "audio.wav"
	RTPFile      = "clock.rtp"
	MP4File      = "proxy.mp4"
	ManifestFile = "manifest.json"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Build generates the whole package and returns its manifest.
func Build(o Options) (*Manifest, error) {
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return nil, err
	}
	if o.SampleRate == 0 {
		o.SampleRate = 48000
	}
	if o.TileFrames == 0 {
		o.TileFrames = wav.TileFrames
	}
	m := &Manifest{
		Seed: o.Seed, Channels: o.Channels, Frames: o.Frames,
		SampleRate: o.SampleRate, TileFrames: o.TileFrames,
		DamageFrame: o.DamageFrame, WrapAt: o.WrapAt, DriftPPM: o.DriftPPM,
		VFRPattern: append([]int(nil), o.VFRPattern...),
	}

	// Pseudo-random but deterministic sample content.
	rng := newRNG(o.Seed)
	bits := uint16(24)
	frameBytes := o.Channels * int(bits) / 8
	data := make([]byte, o.Frames*int64(frameBytes))
	for i := range data {
		data[i] = byte(rng.next() & 0xFF)
	}

	// Compute per-tile CRC32C over the clean PCM, then damage exactly one tile.
	tileBytes := o.TileFrames * int64(frameBytes)
	tileCount := o.Frames / o.TileFrames
	crcs := make([]uint32, tileCount)
	for t := int64(0); t < tileCount; t++ {
		crcs[t] = crc32.Checksum(data[t*tileBytes:(t+1)*tileBytes], crcTable)
	}
	if o.DamageFrame >= 0 {
		// Flip bytes across the whole 32-frame tile so the CRC fails once.
		tile := o.DamageFrame / o.TileFrames
		for k := int64(0); k < tileBytes; k++ {
			data[tile*tileBytes+k] ^= 0xA5
		}
		m.DamageCount = 1
	}

	if err := writeWAV(filepath.Join(o.Dir, AudioFile), o, bits, data, crcs); err != nil {
		return nil, err
	}
	m.Files = append(m.Files, AudioFile)
	if err := writeRTP(filepath.Join(o.Dir, RTPFile), o); err != nil {
		return nil, err
	}
	m.Files = append(m.Files, RTPFile)
	if err := writeMP4(filepath.Join(o.Dir, MP4File), o); err != nil {
		return nil, err
	}
	m.Files = append(m.Files, MP4File)

	m.Fingerprint = fingerprint(m)
	if err := writeJSON(filepath.Join(o.Dir, ManifestFile), m); err != nil {
		return nil, err
	}
	m.Files = append(m.Files, ManifestFile)
	return m, nil
}

// fingerprint hashes the deterministic manifest fields (excluding the
// fingerprint itself) so resume can detect any input change.
func fingerprint(m *Manifest) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode([]interface{}{
		m.Seed, m.Channels, m.Frames, m.SampleRate, m.TileFrames,
		m.DamageFrame, m.WrapAt, m.DriftPPM, m.VFRPattern, m.Files,
	})
	return hex.EncodeToString(h.Sum(nil))
}

func writeJSON(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
