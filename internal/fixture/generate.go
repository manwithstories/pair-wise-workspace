package fixture

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Options controls one deterministic synthetic MP4 asset.
type Options struct {
	Seed             int64
	DurationSeconds  int64
	VideoTimescale   uint32 // track timescale, typically 90000
	VideoFPS         uint32 // frames per second
	AudioTimescale   uint32 // typically 48000
	AudioDelta       uint32 // audio sample duration in audio timescale
	KeyEvery         uint32 // keyframe spacing in video frames (sparse if large)
	StartDTS         uint32 // nonzero first-sample decode time (video units)
	EditList         bool   // whether an edts/elst box is emitted
	VideoBody        int    // synthetic video sample body size (bitrate proxy)
	AudioBody        int    // synthetic audio sample body size
	VideoStreamID    uint32 // LCG stream selector for video
	AudioStreamID    uint32 // LCG stream selector for audio
}

// Layout exposes everything later stages need to verify or damage the
// generated asset without re-parsing it: chunk offsets, index patch
// positions and the effective options.
type Layout struct {
	Opts              Options
	FtypSize          int64
	MoovSize          int64
	MdatHeaderOffset  int64
	MdatPayloadOffset int64
	TotalSize         int64
	VideoChunkOffset []int64
	AudioChunkOffset []int64
	VideoSizes       []uint32
	AudioSizes       []uint32
	patch            moovLayout
}

const (
	streamVideoBase uint32 = 100
	streamAudioBase uint32 = 200
)

// Generate writes a complete, self-describing MP4 into w.
func Generate(w io.WriteSeeker, o Options) (*Layout, error) {
	if o.DurationSeconds <= 0 || o.VideoFPS == 0 || o.AudioDelta == 0 || o.AudioTimescale == 0 {
		return nil, fmt.Errorf("fixture: invalid generation options")
	}
	videoSamples := uint32(o.VideoFPS * uint32(o.DurationSeconds))
	audioSamples := uint32(o.DurationSeconds) * (o.AudioTimescale / o.AudioDelta)
	videoDelta := o.VideoTimescale / o.VideoFPS
	keyEvery := o.KeyEvery
	if keyEvery == 0 {
		keyEvery = o.VideoFPS * 6 // one keyframe per six seconds
	}

	lay := &Layout{Opts: o}
	lay.VideoSizes = make([]uint32, videoSamples)
	lay.AudioSizes = make([]uint32, audioSamples)
	lay.VideoChunkOffset = make([]int64, videoSamples)
	lay.AudioChunkOffset = make([]int64, audioSamples)
	for i := range lay.VideoSizes {
		lay.VideoSizes[i] = uint32(SampleOverhead + o.VideoBody)
	}
	for i := range lay.AudioSizes {
		lay.AudioSizes[i] = uint32(SampleOverhead + o.AudioBody)
	}

	// 1) ftyp
	ftyp, _ := BeginBox(w, "ftyp")
	var ftypPayload [24]byte
	copy(ftypPayload[0:], "isom")
	putU32(ftypPayload[4:], 0x200)
	copy(ftypPayload[8:], "isom")
	copy(ftypPayload[12:], "iso2")
	copy(ftypPayload[16:], "avc1")
	copy(ftypPayload[20:], "mp41")
	if _, err := w.Write(ftypPayload[:]); err != nil {
		return nil, err
	}
	if err := EndBox(w, ftyp); err != nil {
		return nil, err
	}
	lay.FtypSize, _ = w.Seek(0, io.SeekCurrent)

	// 2) moov in memory; stco entries get patched once offsets are known.
	vSpec := trackSpec{
		trackID: 1, timescale: o.VideoTimescale, samples: videoSamples,
		delta: videoDelta, startDTS: o.StartDTS, isVideo: true,
		keyEvery: keyEvery, sizes: lay.VideoSizes, handler: "vide", language: 0x55C4,
	}
	aSpec := trackSpec{
		trackID: 2, timescale: o.AudioTimescale, samples: audioSamples,
		delta: o.AudioDelta, isVideo: false, sizes: lay.AudioSizes,
		handler: "soun", language: 0x55C4,
	}
	moov := buildMoov(vSpec, aSpec, o.StartDTS, o.EditList)
	lay.patch = moov
	lay.MoovSize = int64(len(moov.data))
	lay.MdatHeaderOffset = lay.FtypSize + lay.MoovSize
	lay.MdatPayloadOffset = lay.MdatHeaderOffset + 8

	// 3) compute chunk offsets: video chunks first, then audio chunks.
	cursor := lay.MdatPayloadOffset
	for i := range lay.VideoChunkOffset {
		lay.VideoChunkOffset[i] = cursor
		cursor += int64(lay.VideoSizes[i])
	}
	for i := range lay.AudioChunkOffset {
		lay.AudioChunkOffset[i] = cursor
		cursor += int64(lay.AudioSizes[i])
	}

	// 4) patch stco absolute offsets inside the moov buffer.
	for i, off := range lay.VideoChunkOffset {
		putU32(moov.data[int(moov.vSTCOPayloadPos)+i*4:], uint32(off))
	}
	for i, off := range lay.AudioChunkOffset {
		putU32(moov.data[int(moov.aSTCOPayloadPos)+i*4:], uint32(off))
	}
	if _, err := w.Write(moov.data); err != nil {
		return nil, err
	}

	// 5) mdat with checksummed samples.
	mdat, _ := BeginBox(w, "mdat")
	for i := uint32(0); i < videoSamples; i++ {
		s := BuildSample(o.Seed, o.VideoStreamID, i, o.VideoBody)
		if _, err := w.Write(s); err != nil {
			return nil, err
		}
	}
	for i := uint32(0); i < audioSamples; i++ {
		s := BuildSample(o.Seed, o.AudioStreamID, i, o.AudioBody)
		if _, err := w.Write(s); err != nil {
			return nil, err
		}
	}
	if err := EndBox(w, mdat); err != nil {
		return nil, err
	}
	lay.TotalSize, _ = w.Seek(0, io.SeekCurrent)
	return lay, nil
}

// TruncateTail cuts dropBytes from the end, leaving declared box/sample
// sizes pointing past EOF: the canonical container-truncation fixture.
func TruncateTail(w io.WriteSeeker, lay *Layout, dropBytes int64) error {
	t, ok := w.(truncatable)
	if !ok {
		return fmt.Errorf("fixture: writer does not support truncate")
	}
	if dropBytes <= 0 || dropBytes >= lay.TotalSize {
		return fmt.Errorf("fixture: invalid truncation length")
	}
	return t.Truncate(lay.TotalSize - dropBytes)
}

// CorruptSample flips one payload byte of the video sample at sampleIndex.
// The on-disk CRC then fails while every container box stays intact.
func CorruptSample(w io.WriteSeeker, lay *Layout, sampleIndex uint32) error {
	if int(sampleIndex) >= len(lay.VideoChunkOffset) {
		return fmt.Errorf("fixture: sample index out of range")
	}
	off := lay.VideoChunkOffset[sampleIndex] + 8 // inside the body
	if _, err := w.Seek(off, io.SeekStart); err != nil {
		return err
	}
	b := []byte{0xA5}
	_, err := w.Write(b)
	return err
}

// BloatSTCOCount declares two more chunk offsets than the box contains,
// simulating an incomplete index (missing table page).
func BloatSTCOCount(w io.WriteSeeker, lay *Layout) error {
	pos := lay.FtypSize + lay.patch.vSTCOCountPos
	if _, err := w.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(lay.VideoChunkOffset))+2)
	_, err := w.Write(b[:])
	return err
}

// ForgeOversizeSample declares a sample length far beyond the mdat box,
// exercising the "never read past a declared boundary" guarantee.
func ForgeOversizeSample(w io.WriteSeeker, lay *Layout) error {
	pos := lay.FtypSize + lay.patch.vSTSZPayloadPos
	if _, err := w.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 0xFFFFFFFF)
	_, err := w.Write(b[:])
	return err
}

// GenerateMemory is a test convenience: generate into a self-contained
// byte buffer plus a working WriteSeeker for damage injection.
func GenerateMemory(o Options) (*memBuffer, *Layout, error) {
	m := &memBuffer{}
	lay, err := Generate(m, o)
	return m, lay, err
}
