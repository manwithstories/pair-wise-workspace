package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Timeline constants shared by the synthetic movie and the tests.
const (
	MovieTimescale  uint32 = 1000
	VideoTimescale  uint32 = 15360 // 30 fps => 512 ticks per frame
	VideoFrameTicks uint32 = 512
	VideoFPS               = 30
	AudioTimescale  uint32 = 48000
	AudioFrameTicks uint32 = 1024 // AAC frame at 48 kHz (~21.33 ms)
	VideoTrackID    uint32 = 1
	AudioTrackID    uint32 = 2
	VideoDurationS         = 3
	AudioDurationS         = 2
)

// audioSamplesPerVideoFrame keeps audio roughly in step with video.
const audioSamplesPerVideoFrame = 45 // 45*1024/48000 = 0.96 s per 30 frames

type sampleRec struct {
	size  uint32
	ticks uint32
	key   bool
	plex  int64 // absolute file offset of payload
}

type plan struct {
	spec      Spec
	ftyp      []byte
	moov      []byte
	mdat      []byte
	samples   []sampleRec
	audioSamp []sampleRec
	videoRate uint32
	audioRate uint32
	video     track
	audio     track
}

// build assembles ftyp + moov + mdat and records sample coordinates.
func build(spec Spec) (*built, error) {
	p, err := planFor(spec)
	if err != nil {
		return nil, err
	}
	data := concat(p.ftyp, p.moov, p.mdat)
	b := &built{bytes: data, movieTimescale: MovieTimescale}
	// Recompute absolute payload offsets now that the file is laid out.
	mdatPayload := len(p.ftyp) + len(p.moov) + 8
	var rel int64
	b.sampleOffsets = make([]int64, len(p.samples))
	b.sampleSizes = make([]int, len(p.samples))
	for i, s := range p.samples {
		b.sampleOffsets[i] = int64(mdatPayload) + rel
		b.sampleSizes[i] = int(s.size)
		rel += int64(s.size)
	}
	b.videoSamples = len(p.samples)
	b.audioTrackOff = int64(mdatPayload) + rel
	b.damaged = -1
	if spec.Kind == Corrupt || spec.Kind == BoundaryMismatch {
		if spec.CorruptAt > 0 && spec.CorruptAt < len(p.samples) {
			b.damaged = spec.CorruptAt
			b.damageOffset = b.sampleOffsets[spec.CorruptAt]
		}
	}
	return b, nil
}

// planFor produces the boxes. Payload offsets stored in the moov are
// relative to the mdat payload and are fixed up in build.
func planFor(spec Spec) (*plan, error) {
	frames := spec.Frames
	videoPayload := make([]sampleRec, frames)
	for i := 0; i < frames; i++ {
		videoPayload[i] = sampleRec{
			size:  uint32(600 + (i%7)*40),
			ticks: VideoFrameTicks,
			key:   i%spec.KeyframeEvery == 0,
		}
	}
	audioFrames := (frames / VideoFPS) * audioSamplesPerVideoFrame
	if audioFrames < 1 {
		audioFrames = 1
	}
	audioPayload := make([]sampleRec, audioFrames)
	for i := range audioPayload {
		audioPayload[i] = sampleRec{size: 180, ticks: AudioFrameTicks}
	}
	videoDur := uint64(frames) * uint64(VideoFrameTicks)
	audioDur := uint64(audioFrames) * uint64(AudioFrameTicks)

	// Payload bytes: deterministic pseudo-random so corruption is detectable.
	var payload bytes.Buffer
	for _, s := range videoPayload {
		writePayload(&payload, int(s.size), 0x56)
	}
	audioStart := payload.Len()
	for _, s := range audioPayload {
		writePayload(&payload, int(s.size), 0xa5)
	}

	video := track{
		id:          VideoTrackID,
		handler:     "vide",
		timescale:   VideoTimescale,
		duration:    videoDur,
		samples:     videoPayload,
		width:       640,
		height:      360,
		mdatStart:   0,
		audio:       false,
		dropStco:    spec.Kind == MissingIndex,
		emptyEditMS: 0,
	}
	audio := track{
		id:          AudioTrackID,
		handler:     "soun",
		timescale:   AudioTimescale,
		duration:    audioDur,
		samples:     audioPayload,
		mdatStart:   audioStart,
		audio:       true,
		dropStco:    spec.Kind == MissingIndex,
		emptyEditMS: 0,
	}
	res := &plan{
		spec:      spec,
		ftyp:      ftyp(),
		mdat:      boxBytes("mdat", payload.Bytes()),
		samples:   videoPayload,
		audioSamp: audioPayload,
		videoRate: VideoTimescale,
		audioRate: AudioTimescale,
		video:     video,
		audio:     audio,
	}
	if spec.Kind == BoundaryMismatch {
		res.audio.emptyEditMS = 40
	}
	res.moov = res.buildMoov(videoDur, audioDur)
	// stco offsets are absolute file positions; the moov length depends only
	// on the fixed-width offset fields, so a second pass settles them exactly.
	base := len(res.ftyp) + len(res.moov) + 8
	res.video.mdatBase = base
	res.audio.mdatBase = base
	res.moov = res.buildMoov(videoDur, audioDur)
	return res, nil
}

// buildMoov assembles mvhd plus one trak per track using the current track
// variants (edit lists, dropped offsets).
func (p *plan) buildMoov(videoDur, audioDur uint64) []byte {
	kids := concat(mvhd(videoDur, audioDur), p.video.trak(), p.audio.trak())
	return boxBytes("moov", kids)
}

func writePayload(buf *bytes.Buffer, n int, seed byte) {
	for i := 0; i < n; i++ {
		buf.WriteByte(seed ^ byte(i*31) ^ 0x3c)
	}
}

type track struct {
	id          uint32
	handler     string
	timescale   uint32
	duration    uint64
	samples     []sampleRec
	width       uint16
	height      uint16
	mdatStart   int
	mdatBase    int
	audio       bool
	emptyEditMS uint32
	dropStco    bool
}

func (t track) trak() []byte {
	tkhd := t.tkhd()
	mdhd := t.mdhd()
	hdlr := hdlr(t.handler)
	stbl := t.stbl()
	minf := boxBytes("minf", concat(t.minfHeader(), stbl))
	mdia := fullBox("mdia", 0, 0, nil)
	_ = mdia
	mdiaKids := concat(mdhd, hdlr, minf)
	mdiaBox := boxBytes("mdia", mdiaKids)
	parts := [][]byte{tkhd}
	if t.emptyEditMS > 0 {
		parts = append(parts, edts(t.emptyEditMS))
	}
	parts = append(parts, mdiaBox)
	return boxBytes("trak", concat(parts...))
}

func (t track) tkhd() []byte {
	x := &w{}
	x.u32(0) // creation
	x.u32(0) // modification
	x.u32(t.id)
	x.u32(0)                  // reserved
	x.u32(uint32(t.duration)) // movie timescale ticks (approx)
	x.u32(0)
	x.u32(0)
	x.u16(0) // layer
	x.u16(0) // alternate group
	if t.audio {
		x.u16(0x0100)
	} else {
		x.u16(0)
	}
	x.u16(0)
	identityMatrix(x)
	if t.audio {
		x.u32(0)
		x.u32(0)
	} else {
		x.u32(uint32(t.width) << 16)
		x.u32(uint32(t.height) << 16)
	}
	return fullBox("tkhd", 0, 0x000007, x.bytes())
}

func (t track) mdhd() []byte {
	x := &w{}
	x.u32(0)
	x.u32(0)
	x.u32(t.timescale)
	x.u32(uint32(t.duration))
	x.u16(0x55c4) // 'und'
	x.u16(0)
	return fullBox("mdhd", 0, 0, x.bytes())
}

func hdlr(kind string) []byte {
	x := &w{}
	x.u32(0)
	x.raw([]byte(kind))
	x.raw([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	x.raw([]byte("mediacheck\x00"))
	return fullBox("hdlr", 0, 0, x.bytes())
}

func (t track) minfHeader() []byte {
	if t.audio {
		return fullBox("smhd", 0, 0, []byte{0, 0, 0, 0})
	}
	return fullBox("vmhd", 0, 1, make([]byte, 8))
}

// stbl builds the sample table. Chunk offsets are relative to the mdat
// payload start and are adjusted to absolute in build().
func (t track) stbl() []byte {
	stsd := t.stsd()
	stts := sttsBox(t.samples)
	stss := []byte(nil)
	if !t.audio {
		stss = stssBox(t.samples)
	}
	stsc := stscBox()
	stsz := stszBox(t.samples)
	kids := concat(stsd, stts)
	if stss != nil {
		kids = concat(kids, stss)
	}
	kids = concat(kids, stsc, stsz)
	if !t.dropStco {
		kids = concat(kids, stcoBox(t.samples, t.mdatStart+t.mdatBase))
	}
	return boxBytes("stbl", kids)
}

func (t track) stsd() []byte {
	var entry []byte
	if t.audio {
		x := &w{}
		x.raw(make([]byte, 6))
		x.u16(1) // data reference index
		x.u16(0) // version
		x.u16(0)
		x.u32(0)
		x.u16(2) // channels
		x.u16(16)
		x.u16(0)
		x.u16(0)
		x.u32(t.timescale << 16)
		entry = boxBytes("mp4a", x.bytes())
	} else {
		x := &w{}
		x.raw(make([]byte, 6))
		x.u16(1)
		x.u16(0)
		x.u16(0)
		x.u32(0)
		x.u32(0)
		x.u32(0)
		x.u16(t.width)
		x.u16(t.height)
		x.u32(0x00480000)
		x.u32(0x00480000)
		x.u32(0)
		x.u16(1)
		x.raw(make([]byte, 32))
		x.u16(0x0018)
		x.u16(0xffff)
		entry = boxBytes("avc1", x.bytes())
	}
	body := &w{}
	body.u32(1)
	body.raw(entry)
	return fullBox("stsd", 0, 0, body.bytes())
}

func sttsBox(samples []sampleRec) []byte {
	// Compress equal deltas into runs.
	type run struct {
		count, delta uint32
	}
	var runs []run
	for _, s := range samples {
		if n := len(runs); n > 0 && runs[n-1].delta == s.ticks {
			runs[n-1].count++
			continue
		}
		runs = append(runs, run{count: 1, delta: s.ticks})
	}
	x := &w{}
	x.u32(uint32(len(runs)))
	for _, r := range runs {
		x.u32(r.count)
		x.u32(r.delta)
	}
	return fullBox("stts", 0, 0, x.bytes())
}

func stssBox(samples []sampleRec) []byte {
	var keys []uint32
	for i, s := range samples {
		if s.key {
			keys = append(keys, uint32(i+1))
		}
	}
	x := &w{}
	x.u32(uint32(len(keys)))
	for _, k := range keys {
		x.u32(k)
	}
	return fullBox("stss", 0, 0, x.bytes())
}

func stscBox() []byte {
	x := &w{}
	x.u32(1)
	x.u32(1) // first chunk
	x.u32(1) // one sample per chunk
	x.u32(1) // sample description index
	return fullBox("stsc", 0, 0, x.bytes())
}

func stszBox(samples []sampleRec) []byte {
	x := &w{}
	x.u32(0)
	x.u32(uint32(len(samples)))
	for _, s := range samples {
		x.u32(s.size)
	}
	return fullBox("stsz", 0, 0, x.bytes())
}

func stcoBox(samples []sampleRec, base int) []byte {
	x := &w{}
	x.u32(uint32(len(samples)))
	off := base
	for _, s := range samples {
		x.u32(uint32(off))
		off += int(s.size)
	}
	return fullBox("stco", 0, 0, x.bytes())
}

func mvhd(videoDur, audioDur uint64) []byte {
	maxDur := videoDur
	if audioDur > maxDur {
		maxDur = audioDur
	}
	x := &w{}
	x.u32(0)
	x.u32(0)
	x.u32(MovieTimescale)
	movieTicks := uint32(0)
	if VideoTimescale > 0 {
		movieTicks = uint32(uint64(VideoDurationS) * uint64(MovieTimescale))
	}
	x.u32(movieTicks)
	x.u32(0x00010000) // rate
	x.u16(0x0100)     // volume
	x.u16(0)
	x.u32(0)
	x.u32(0)
	identityMatrix(x)
	x.raw(make([]byte, 24))
	x.u32(3) // next track id
	return fullBox("mvhd", 0, 0, x.bytes())
}

// edts with a single empty edit delays the track by delayMS (movie timescale).
func edts(delayMS uint32) []byte {
	x := &w{}
	x.u32(1)
	x.u32(delayMS) // segment duration
	x.i32(-1)      // media time -1 => empty edit
	x.u16(1)       // media rate 1.0
	x.u16(0)
	elstBox := fullBox("elst", 0, 0, x.bytes())
	return boxBytes("edts", elstBox)
}

var _ = binary.BigEndian
var _ = fmt.Sprintf
