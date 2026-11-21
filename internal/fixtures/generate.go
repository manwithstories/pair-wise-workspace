// Package fixtures generates the local sample corpus used by the demo and
// by tests: frame manifests with payload files and SHA-256 checksums, audio
// descriptions, colour metadata, and MP4 files for several bitrate
// representations.  Everything is derived from a seed so that repeated runs
// with the same seed produce byte-identical fixtures.
package fixtures

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediapack/internal/audio"
	"mediapack/internal/color"
	"mediapack/internal/fault"
	"mediapack/internal/frames"
)

// DefaultFrameCount is three seconds at 30 fps.
const DefaultFrameCount = 90

// DefaultTimescale is the programme time base.
const DefaultTimescale = 90000

// DefaultFrameDelta is one 30 fps frame in 90 kHz ticks.
const DefaultFrameDelta = 3000

// Inject selects deliberate damage for the failure demonstrations.
type Inject struct {
	CorruptFrame   bool
	DuplicateFrame bool
	MissingFrame   bool
	TruncatedMP4   bool
	AudioConflict  bool
	ColorConflict  bool
	MisalignedRate bool
	UnknownMP4Box  bool
	IllegalBoxSize bool
}

// Options controls fixture generation.
type Options struct {
	Seed       int64
	Rates      []int
	FrameCount int
	Timescale  uint32
	Delta      uint32
	Dir        string
	Inject     Inject
}

// Bundle is a generated corpus on disk plus the decoded descriptions.
type Bundle struct {
	Dir        string
	Seed       int64
	Timescale  uint32
	FrameCount int
	Rates      []Rate
	Audio      []audio.Stream
	Color      []color.Source
}

// Rate is one representation's files and manifest.
type Rate struct {
	Value    int
	MP4Path  string
	Manifest frames.Manifest
	Payloads map[int][]byte
	FileHash string
}

// Generate writes a corpus under opt.Dir.
func Generate(opt Options) (*Bundle, error) {
	if opt.FrameCount <= 0 {
		opt.FrameCount = DefaultFrameCount
	}
	if opt.Timescale == 0 {
		opt.Timescale = DefaultTimescale
	}
	if opt.Delta == 0 {
		opt.Delta = DefaultFrameDelta
	}
	if len(opt.Rates) == 0 {
		return nil, fault.New(fault.ClassUsage, "fixtures", "*", "no-rates", "at least one bitrate is required")
	}
	if len(opt.Rates) > 8 {
		return nil, fault.New(fault.ClassLimit, "fixtures", "*", "too-many-rates", "at most 8 bitrate representations are supported")
	}
	for _, r := range opt.Rates {
		if r <= 0 {
			return nil, fault.New(fault.ClassUsage, "fixtures", "*", "invalid-rate",
				fmt.Sprintf("bitrate %d must be positive", r))
		}
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return nil, fault.Wrap(fault.ClassInternal, "fixtures", "*", "mkdir-failed", "cannot create fixture directory", err)
	}

	b := &Bundle{Dir: opt.Dir, Seed: opt.Seed, Timescale: opt.Timescale, FrameCount: opt.FrameCount}
	rng := rand.New(rand.NewSource(opt.Seed))

	for i, rate := range opt.Rates {
		r, err := generateRate(opt, rng, rate, i)
		if err != nil {
			return nil, err
		}
		b.Rates = append(b.Rates, *r)
	}

	b.Audio = []audio.Stream{
		{Track: "a-main", Layout: audio.LayoutStereo, SampleRate: 48000, Source: "cam-a"},
		{Track: "a-remote", Layout: audio.LayoutStereo, SampleRate: 48000, Source: "remote-1"},
	}
	if opt.Inject.AudioConflict {
		b.Audio[1] = audio.Stream{Track: "a-remote", Layout: audio.Layout5_1, SampleRate: 44100, Source: "remote-1"}
	}

	b.Color = []color.Source{
		{ID: "cam-a", Tag: "nclx", Triplet: color.Triplet{Primaries: 1, Transfer: 1, Matrix: 1}},
		{ID: "cam-b", Tag: "nclx", Triplet: color.Triplet{Primaries: 1, Transfer: 1, Matrix: 1}},
	}
	if opt.Inject.ColorConflict {
		b.Color[1].Triplet = color.Triplet{Primaries: 9, Transfer: 16, Matrix: 9}
	}
	return b, nil
}

func generateRate(opt Options, rng *rand.Rand, rate, index int) (*Rate, error) {
	dir := filepath.Join(opt.Dir, fmt.Sprintf("rate-%d", rate))
	if err := os.MkdirAll(filepath.Join(dir, "payloads"), 0o755); err != nil {
		return nil, fault.Wrap(fault.ClassInternal, "fixtures", "*", "mkdir-failed", "cannot create payload directory", err)
	}
	keyframeEvery := 30
	payloads := make(map[int][]byte, opt.FrameCount)
	records := make([]frames.Record, 0, opt.FrameCount)

	for n := 1; n <= opt.FrameCount; n++ {
		size := 128 + rate/16000
		if index%2 == 1 {
			size += 7
		}
		payload := make([]byte, size)
		rng.Read(payload)
		// Make each rate's payload distinguishable while deterministic.
		payload[0] = byte(rate >> 8)
		payload[1] = byte(rate)
		payload[2] = byte(n)
		payloads[n] = payload

		digest := frames.Sum(payload)
		if opt.Inject.CorruptFrame && n == 2 && index == 0 {
			// Rewrite the stored payload after the checksum was computed.
			corrupted := append([]byte(nil), payload...)
			corrupted[0] ^= 0xff
			payloads[n] = corrupted
		}
		seq := uint16(n - 1)
		rtpTS := uint32((n - 1) * int(opt.Delta))
		pkt := &rtp.Packet{Header: rtp.Header{
			Version: 2, PayloadType: uint8(96 + index), SequenceNumber: seq,
			Timestamp: rtpTS, SSRC: uint32(0x51000000 + rate),
		}, Payload: payload}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, fault.Wrap(fault.ClassInternal, "fixtures", "*", "rtp-marshal", "cannot build RTP packet", err)
		}

		if opt.Inject.MissingFrame && n == 4 && index == 0 {
			continue // gap at frame 4
		}
		records = append(records, frames.Record{
			Number: n, Timestamp: uint64((n - 1) * int(opt.Delta)), Keyframe: (n-1)%keyframeEvery == 0,
			SHA256: digest, RTPPacket: raw,
		})
		if opt.Inject.DuplicateFrame && n == 3 && index == 0 {
			records = append(records, records[len(records)-1])
		}
	}

	for n, payload := range payloads {
		path := filepath.Join(dir, "payloads", fmt.Sprintf("%04d.bin", n))
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			return nil, fault.Wrap(fault.ClassInternal, "fixtures", "*", "write-failed", "cannot write payload", err)
		}
	}
	manifest := frames.Manifest{Track: fmt.Sprintf("v-%d", rate), Timescale: opt.Timescale, Records: records}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fault.Wrap(fault.ClassInternal, "fixtures", "*", "json-failed", "cannot encode manifest", err)
	}
	manifestPath := filepath.Join(dir, "frames.json")
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		return nil, fault.Wrap(fault.ClassInternal, "fixtures", "*", "write-failed", "cannot write manifest", err)
	}

	keyframes := make([]uint32, 0, opt.FrameCount/keyframeEvery+1)
	for n := 1; n <= opt.FrameCount; n += keyframeEvery {
		if opt.Inject.MisalignedRate && index > 0 {
			n += 2 // push secondary keyframes two frames past the boundary
			if n > opt.FrameCount {
				break
			}
		}
		keyframes = append(keyframes, uint32(n))
	}

	mp4Bytes := buildMP4(mp4Options{
		FrameCount: opt.FrameCount, Delta: opt.Delta, Keyframes: keyframes,
		Payloads: payloads, Width: uint16(1280 + index*640), Height: 720,
		Channels: 2, SampleRate: 48000,
	})
	if opt.Inject.IllegalBoxSize {
		mp4Bytes[0], mp4Bytes[1], mp4Bytes[2], mp4Bytes[3] = 0, 0, 0, 4
	}
	if opt.Inject.TruncatedMP4 {
		mp4Bytes = mp4Bytes[:len(mp4Bytes)-4]
	}
	if opt.Inject.UnknownMP4Box {
		mp4Bytes = append(mp4Bytes, rawBox("wvnd", []byte{1, 2, 3, 4})...)
	}
	mp4Path := filepath.Join(dir, "segment.mp4")
	if err := os.WriteFile(mp4Path, mp4Bytes, 0o644); err != nil {
		return nil, fault.Wrap(fault.ClassInternal, "fixtures", "*", "write-failed", "cannot write mp4", err)
	}
	sum := sha256.Sum256(mp4Bytes)
	return &Rate{Value: rate, MP4Path: mp4Path, Manifest: manifest, Payloads: payloads,
		FileHash: hex.EncodeToString(sum[:])}, nil
}
