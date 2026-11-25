package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/rand"

	"avpipe/internal/media"
)

// GenConfig controls deterministic sample-container generation.
type GenConfig struct {
	Frames      int
	Seed        int64
	FPS         uint64
	AudioRate   uint32
	AudioLayout uint8 // media.ChannelLayout value
	WithIndex   bool
	WithAudio   bool
	// TailTruncate drops this many bytes from the end of the file.
	TailTruncate int
	// CorruptEvery, when >0, corrupts the payload of every nth record.
	CorruptEvery int
	// PTSRollback, when >0, forces a PTS regression on this sequence.
	PTSRollback int
	// SkipIndex omits the index while keeping the audio section.
	SkipIndex bool
}

// DefaultGenConfig produces a 30 fps indexed stereo 48 kHz stream.
func DefaultGenConfig(frames int, seed int64) GenConfig {
	return GenConfig{
		Frames: frames, Seed: seed, FPS: 30, AudioRate: 48_000,
		AudioLayout: 2, WithIndex: true, WithAudio: true,
	}
}

// generatedRecord is the in-memory form before serialization.
type generatedRecord struct {
	seq     uint32
	pts     uint64
	payload []byte
}

// Generate builds a complete container byte stream from cfg.
func Generate(cfg GenConfig) ([]byte, error) {
	rng := rand.New(rand.NewSource(cfg.seedForRand()))
	recs := make([]generatedRecord, cfg.Frames)
	step := mediaStep(cfg.FPS)
	for i := 0; i < cfg.Frames; i++ {
		payload := deterministicPayload(cfg.Seed, i, 64+rng.Intn(64))
		recs[i] = generatedRecord{
			seq:     uint32(i),
			pts:     uint64(i) * step,
			payload: payload,
		}
	}
	if cfg.PTSRollback > 0 && cfg.PTSRollback < len(recs) {
		recs[cfg.PTSRollback].pts = recs[cfg.PTSRollback-1].pts - 1
	}
	// Note: payload corruption is applied during serialization, AFTER each
	// record's SHA-256 has been computed, so the on-disk checksum fails.
	return serialize(cfg, recs, rng)
}

func (cfg GenConfig) seedForRand() int64 {
	if cfg.Seed == 0 {
		return 1
	}
	return cfg.Seed
}

func mediaStep(fps uint64) uint64 {
	if fps == 0 {
		fps = 30
	}
	return media.Clock90k / fps
}

// deterministicPayload derives reproducible bytes from seed+sequence.
func deterministicPayload(seed int64, seq, size int) []byte {
	b := make([]byte, size)
	var x uint32 = uint32(seed)*2654435761 ^ uint32(seq)*40503
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x >> 24)
	}
	return b
}

func flipByte(b []byte) []byte {
	out := bytes.Clone(b)
	out[0] ^= 0xFF
	return out
}

func serialize(cfg GenConfig, recs []generatedRecord, rng *rand.Rand) ([]byte, error) {
	var index []IndexEntry
	var audioHdrs []AudioHeader
	var pcmBuf bytes.Buffer

	recordSize := func(r generatedRecord) uint64 {
		return RecordHeaderSize + uint64(len(r.payload)) + sha256.Size
	}
	recStart := uint64(HeaderSize)
	cur := recStart
	offsets := make([]uint64, len(recs))
	for i, r := range recs {
		offsets[i] = cur
		cur, _ = media.AddU64(cur, recordSize(r))
		if cfg.WithIndex && !cfg.SkipIndex {
			index = append(index, IndexEntry{Sequence: r.seq, PTS90k: r.pts, Offset: offsets[i]})
		}
	}
	indexOffset := uint64(0)
	if len(index) > 0 {
		indexOffset = cur
		cur, _ = media.AddU64(cur, uint64(len(index))*IndexEntrySize)
	}
	audioOffset := uint64(0)
	if cfg.WithAudio {
		audioOffset = cur
		layout := media.ChannelLayout(cfg.AudioLayout)
		if layout == 0 {
			layout = media.LayoutStereo
		}
		perBlock := int(cfg.AudioRate) / int(orFPS(cfg.FPS))
		perBlock = (perBlock / layout.Channels()) * layout.Channels()
		audioHdrs, pcmBuf = buildAudio(cfg, recs, layout, perBlock, audioOffset, rng)
		cur, _ = media.AddU64(cur, 8+uint64(len(audioHdrs))*AudioHeaderSize+uint64(pcmBuf.Len()))
	}

	buf := make([]byte, 0, cur+FooterSize)
	flags := uint8(0)
	if len(index) > 0 {
		flags |= FlagHasIndex
	}
	if cfg.WithAudio {
		flags |= FlagHasAudio
	}
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], MagicHeader[:])
	hdr[4] = ContainerVersion1
	hdr[5] = flags
	putU32(hdr[8:12], uint32(len(recs)))
	putU64(hdr[12:20], indexOffset)
	putU64(hdr[20:28], audioOffset)
	buf = append(buf, hdr...)

	for i, r := range recs {
		rh := make([]byte, RecordHeaderSize)
		putU32(rh[0:4], r.seq)
		putU64(rh[4:12], r.pts)
		putU32(rh[12:16], uint32(len(r.payload)))
		buf = append(buf, rh...)
		payload := r.payload
		if cfg.CorruptEvery > 0 && (i+1)%cfg.CorruptEvery == 0 {
			payload = flipByte(payload)
		}
		buf = append(buf, payload...)
		// Checksum stays over the original payload, so corruption is detected.
		sum := sha256.Sum256(r.payload)
		buf = append(buf, sum[:]...)
		_ = offsets[i]
	}
	for _, e := range index {
		buf = append(buf, marshalIndexEntry(e)...)
	}
	if cfg.WithAudio {
		ab := make([]byte, 8)
		copy(ab[0:4], MagicAudio[:])
		putU32(ab[4:8], uint32(len(audioHdrs)))
		buf = append(buf, ab...)
		for _, ah := range audioHdrs {
			db := make([]byte, AudioHeaderSize)
			putU32(db[0:4], ah.Sequence)
			putU64(db[4:12], ah.PTS90k)
			putU32(db[12:16], ah.SampleRate)
			db[16] = uint8(ah.Layout)
			putU32(db[20:24], uint32(len(ah.Samples)))
			putU64(db[24:32], ah.DataOffset)
			buf = append(buf, db...)
		}
		buf = append(buf, pcmBuf.Bytes()...)
	}

	footer := make([]byte, FooterSize)
	copy(footer[0:4], MagicFooter[:])
	footer[4] = ContainerVersion1
	putU32(footer[8:12], uint32(len(recs)))
	payloadHash := hashPayloads(recs)
	copy(footer[12:44], payloadHash[:])
	buf = append(buf, footer...)

	if cfg.TailTruncate > 0 && cfg.TailTruncate < len(buf) {
		buf = buf[:len(buf)-cfg.TailTruncate]
	}
	return buf, nil
}

func orFPS(fps uint64) uint64 {
	if fps == 0 {
		return 30
	}
	return fps
}

func buildAudio(cfg GenConfig, recs []generatedRecord, layout media.ChannelLayout,
	perBlock int, sectionStart uint64, rng *rand.Rand) ([]AudioHeader, bytes.Buffer) {
	hdrs := make([]AudioHeader, 0, len(recs))
	var pcm bytes.Buffer
	dataCursor := uint64(8) + uint64(len(recs))*AudioHeaderSize
	for _, r := range recs {
		samples := make([]int16, perBlock)
		for i := range samples {
			samples[i] = int16(rng.Int31n(4000) - 2000)
		}
		b := make([]byte, len(samples)*2)
		for i, s := range samples {
			binary.BigEndian.PutUint16(b[2*i:], uint16(s))
		}
		hdrs = append(hdrs, AudioHeader{
			AudioBlock: media.AudioBlock{
				Sequence:   r.seq,
				PTS90k:     r.pts,
				SampleRate: cfg.AudioRate,
				Layout:     layout,
				Samples:    samples,
			},
			DataOffset: dataCursor,
		})
		pcm.Write(b)
		dataCursor += uint64(len(b))
	}
	_ = sectionStart
	return hdrs, pcm
}

func hashPayloads(recs []generatedRecord) [sha256.Size]byte {
	h := sha256.New()
	for _, r := range recs {
		h.Write(r.payload)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}
