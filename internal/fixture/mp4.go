// Package fixture generates deterministic, self-contained sample media for
// tests and the demo. Nothing here reads external files: every MP4/MOV and RTP
// stream is synthesised from a seed, so the archive corpus is reproducible.
package fixture

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

// CorruptKind selects a defect to inject into a generated MP4.
type CorruptKind int

const (
	CorruptNone CorruptKind = iota
	// CorruptTruncate cuts the file inside mdat while leaving moov intact, so
	// the index is readable but the referenced bytes are absent.
	CorruptTruncate
	// CorruptBadChunkOffset points a chunk offset beyond EOF.
	CorruptBadChunkOffset
	// CorruptBadStss lists a sample number beyond the sample count.
	CorruptBadStss
	// CorruptNegativeEdit writes a negative media_time in the edit list.
	CorruptNegativeEdit
	// CorruptEmptyEdit writes an edit list with zero entries.
	CorruptEmptyEdit
	// CorruptZeroTimescale sets the video mdhd timescale to zero.
	CorruptZeroTimescale
	// CorruptUnknownBox prepends a well-formed box of an unknown type.
	CorruptUnknownBox
)

func (c CorruptKind) String() string {
	switch c {
	case CorruptNone:
		return "none"
	case CorruptTruncate:
		return "truncated_mdat"
	case CorruptBadChunkOffset:
		return "bad_chunk_offset"
	case CorruptBadStss:
		return "bad_stss_index"
	case CorruptNegativeEdit:
		return "negative_edit_offset"
	case CorruptEmptyEdit:
		return "empty_edit_list"
	case CorruptZeroTimescale:
		return "zero_timescale"
	case CorruptUnknownBox:
		return "unknown_box"
	}
	return "unknown"
}

// MP4Options controls generated MP4 layout and defects.
type MP4Options struct {
	Seed            int64
	VideoSamples    int
	AudioSamples    int
	AuxSamples      int
	VideoTimescale  uint32
	AudioTimescale  uint32
	AuxTimescale    uint32
	KeyframeEvery   int   // stss entry interval; 0 means every sample
	EditMediaTime   int64 // edit list media_time in video timescale units
	EditEmptyFirst  bool  // prepend an empty edit (media_time -1)
	Corrupt         CorruptKind
	TrailingGarbage int // bytes of junk appended
}

// TrackReport summarises one generated track.
type TrackReport struct {
	TrackID    uint32
	Handler    string
	Samples    int
	Keyframes  int
	DurationNS int64
	ChunkStart int64
	ByteLen    int
}

// MP4Report describes a generated file for assertions.
type MP4Report struct {
	Path       string
	Size       int64
	Corrupt    CorruptKind
	Video      TrackReport
	Audio      TrackReport
	Aux        TrackReport
	MovieScale uint32
	MDATStart  int64
}

// DefaultMP4Options returns a small but realistic multi-track layout.
func DefaultMP4Options(seed int64) MP4Options {
	return MP4Options{
		Seed:           seed,
		VideoSamples:   240,
		AudioSamples:   360,
		AuxSamples:     60,
		VideoTimescale: 90000,
		AudioTimescale: 48000,
		AuxTimescale:   600,
		KeyframeEvery:  30,
	}
}

// GenerateMP4 writes a synthetic MP4/MOV to dir/name and returns a report.
// The layout is ftyp, moov, mdat (fast-start), which keeps the index readable
// when the mdat is truncated and matches how archival deliverables are
// typically written.
func GenerateMP4(dir, name string, opts MP4Options) (*MP4Report, error) {
	if err := opts.fillDefaults(); err != nil {
		return nil, err
	}
	g := &mp4Gen{rng: rand.New(rand.NewSource(opts.Seed)), opts: opts}
	return g.write(dir, name)
}

func (o *MP4Options) fillDefaults() error {
	if o.VideoSamples <= 0 {
		o.VideoSamples = 240
	}
	if o.AudioSamples <= 0 {
		o.AudioSamples = 360
	}
	if o.AuxSamples <= 0 {
		o.AuxSamples = 60
	}
	if o.VideoTimescale == 0 {
		o.VideoTimescale = 90000
	}
	if o.AudioTimescale == 0 {
		o.AudioTimescale = 48000
	}
	if o.AuxTimescale == 0 {
		o.AuxTimescale = 600
	}
	if o.KeyframeEvery < 0 {
		return fmt.Errorf("fixture: negative keyframe interval")
	}
	return nil
}

// mp4Gen builds boxes bottom-up.
type mp4Gen struct {
	rng  *rand.Rand
	opts MP4Options
}

func (g *mp4Gen) write(dir, name string) (*MP4Report, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)

	ftyp := mp4Box("ftyp", concat(
		[]byte("isom"), u32(0x200),
		[]byte("isom"), []byte("iso2"), []byte("avc1"), []byte("mp41"),
	))

	videoPayload := g.sampleBytes(g.opts.VideoSamples, 400, 1800, 0x11)
	audioPayload := g.sampleBytes(g.opts.AudioSamples, 120, 500, 0x22)
	auxPayload := g.sampleBytes(g.opts.AuxSamples, 40, 120, 0x33)
	mdatPayload := concat(flatten(videoPayload), flatten(audioPayload), flatten(auxPayload))

	// moov length is independent of the chunk-offset values, so build it once
	// with placeholder offsets, then rebuild with real offsets derived from the
	// final layout.
	buildMoov := func(mdatStart int64) []byte {
		videoChunk := mdatStart
		audioChunk := videoChunk + int64(totalLen(videoPayload))
		auxChunk := audioChunk + int64(totalLen(audioPayload))
		video := g.buildTrack(1, "vide", "avc1", g.videoTimescale(),
			videoPayload, g.opts.KeyframeEvery, videoChunk, g.videoEditMediaTime(), g.opts.EditEmptyFirst, true)
		audio := g.buildTrack(2, "soun", "mp4a", g.opts.AudioTimescale,
			audioPayload, 0, audioChunk, 0, false, false)
		aux := g.buildTrack(3, "tmcd", "tmcd", g.opts.AuxTimescale,
			auxPayload, 0, auxChunk, 0, false, false)
		mvhdBox := g.mvhd(1000, uint32(g.movieDurationMS()))
		return mp4Box("moov", concat(mvhdBox, video, audio, aux))
	}

	placeholderMoov := buildMoov(0)
	mdatDataStart := int64(len(ftyp)) + int64(len(placeholderMoov)) + 8
	moov := buildMoov(mdatDataStart)
	mdat := mp4Box("mdat", mdatPayload)

	var file []byte
	if g.opts.Corrupt == CorruptUnknownBox {
		file = concat(mp4Box("zzzz", []byte("unknown-box-payload")), ftyp, moov, mdat)
	} else {
		file = concat(ftyp, moov, mdat)
	}
	if g.opts.TrailingGarbage > 0 {
		junk := make([]byte, g.opts.TrailingGarbage)
		g.rng.Read(junk)
		file = append(file, junk...)
	}
	if g.opts.Corrupt == CorruptTruncate {
		cut := len(ftyp) + len(moov) + 8 + len(mdatPayload)/2
		file = file[:cut]
	}
	if err := os.WriteFile(path, file, 0o644); err != nil {
		return nil, err
	}

	rep := &MP4Report{
		Path:       path,
		Size:       int64(len(file)),
		Corrupt:    g.opts.Corrupt,
		MovieScale: 1000,
		MDATStart:  mdatDataStart,
	}
	audioStart := mdatDataStart + int64(totalLen(videoPayload))
	auxStart := audioStart + int64(totalLen(audioPayload))
	rep.Video = g.trackReport(1, "vide", videoPayload, g.opts.KeyframeEvery,
		mdatDataStart, g.videoTimescale(), g.videoEditMediaTime())
	rep.Audio = g.trackReport(2, "soun", audioPayload, 0, audioStart, g.opts.AudioTimescale, 0)
	rep.Aux = g.trackReport(3, "tmcd", auxPayload, 0, auxStart, g.opts.AuxTimescale, 0)
	return rep, nil
}

// videoTimescale honours the zero-timescale defect.
func (g *mp4Gen) videoTimescale() uint32 {
	if g.opts.Corrupt == CorruptZeroTimescale {
		return 0
	}
	return g.opts.VideoTimescale
}

// videoEditMediaTime honours edit-list defects.
func (g *mp4Gen) videoEditMediaTime() int64 {
	switch g.opts.Corrupt {
	case CorruptNegativeEdit:
		return -7
	default:
		return g.opts.EditMediaTime
	}
}

func (g *mp4Gen) trackReport(id uint32, handler string, payload [][]byte, every int,
	chunk int64, scale uint32, editMediaTime int64) TrackReport {
	keyframes := len(payload)
	if every > 0 {
		keyframes = (len(payload) + every - 1) / every
	}
	delta := g.unitDelta(len(payload), scale)
	durUnits := int64(delta) * int64(len(payload))
	var durNS int64
	if scale > 0 {
		durNS = durUnits * int64(1e9) / int64(scale)
	}
	if editMediaTime > 0 && scale > 0 {
		// Samples before the edit origin are trimmed by the reader.
		durNS -= editMediaTime * int64(1e9) / int64(scale)
	}
	return TrackReport{
		TrackID:    id,
		Handler:    handler,
		Samples:    len(payload),
		Keyframes:  keyframes,
		DurationNS: durNS,
		ChunkStart: chunk,
		ByteLen:    totalLen(payload),
	}
}

// unitDelta returns the per-sample duration in timescale units for n samples
// spanning one second, clamped to at least one unit.
func (g *mp4Gen) unitDelta(n int, timescale uint32) uint32 {
	if n <= 0 || timescale == 0 {
		return 0
	}
	d := timescale / uint32(n)
	if d == 0 {
		d = 1
	}
	return d
}

func (g *mp4Gen) movieDurationMS() int64 {
	ms := int64(0)
	for _, s := range []struct {
		n     int
		scale uint32
	}{
		{g.opts.VideoSamples, g.videoTimescale()},
		{g.opts.AudioSamples, g.opts.AudioTimescale},
	} {
		if s.n == 0 || s.scale == 0 {
			continue
		}
		d := int64(g.unitDelta(s.n, s.scale)) * int64(s.n) * 1000 / int64(s.scale)
		if d > ms {
			ms = d
		}
	}
	return ms
}

// sampleBytes produces n payloads whose first byte encodes a marker.
func (g *mp4Gen) sampleBytes(n, minSize, maxSize int, marker byte) [][]byte {
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		sz := minSize + g.rng.Intn(maxSize-minSize+1)
		b := make([]byte, sz)
		g.rng.Read(b)
		b[0] = marker
		binary.BigEndian.PutUint32(b[1:5], uint32(i))
		out[i] = b
	}
	return out
}

// WriteManifest writes a manifest next to the media.
func WriteManifest(dir, name, contents string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, []byte(contents), 0o644)
}
