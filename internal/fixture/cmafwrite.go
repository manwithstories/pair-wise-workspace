package fixture

import (
	"encoding/binary"
	"fmt"
)

// The CMAF writer emits an init section (ftyp+moov with one video and one audio
// trak) and media segments (styp+moof+mdat) with explicit tfdt/trun timing.
// Sample payload bytes come from PayloadFor so payload hashes are reproducible.

// Track IDs in the generated init section.
const (
	CMAFVideoTrackID   = 1
	CMAFVideoTimescale = 90000
	CMAFAudioTrackID   = 2
	CMAFAudioTimescale = 48000
)

// box builds a box with a 32-bit size header.
func box(typ string, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

func fullBox(ver byte, flags uint32, typ string, body []byte) []byte {
	head := make([]byte, 4+len(body))
	head[0] = ver
	head[1] = byte(flags >> 16)
	head[2] = byte(flags >> 8)
	head[3] = byte(flags)
	copy(head[4:], body)
	return box(typ, head)
}

// BuildCMAFInit writes the shared initialisation section.
func BuildCMAFInit() []byte {
	ftyp := box("ftyp", []byte{
		'm', 'p', '4', '2', // major brand
		0, 0, 0, 0, // minor version
		'i', 's', 'o', 'm', 'i', 's', 'o', '2', 'm', 'p', '4', '1',
	})
	moov := box("moov", append(append(mvhd(), trakVideo()...), trakAudio()...))
	return append(append([]byte{}, ftyp...), moov...)
}

func mvhd() []byte {
	body := make([]byte, 100)
	binary.BigEndian.PutUint32(body[8:12], 1000) // timescale
	binary.BigEndian.PutUint32(body[12:16], 10000)
	binary.BigEndian.PutUint32(body[96:100], 3) // next track id
	return fullBox(0, 0, "mvhd", body)
}

func trakVideo() []byte {
	body := append(tkhd(CMAFVideoTrackID), mdia("vide", CMAFVideoTimescale, stsdVideo())...)
	return box("trak", body)
}

func trakAudio() []byte {
	body := append(tkhd(CMAFAudioTrackID), mdia("soun", CMAFAudioTimescale, stsdAudio())...)
	return box("trak", body)
}

// tkhd v0 fields after version/flags: creation(4) modification(4)
// track_ID(4) reserved(4) duration(4) ...
func tkhd(trackID uint32) []byte {
	body := make([]byte, 80)
	binary.BigEndian.PutUint32(body[8:12], trackID)
	return fullBox(0, 0x000007, "tkhd", body)
}

// mdhd v0 body: creation(4) modification(4) timescale(4) duration(4).
func mdia(handler string, timescale int64, stsd []byte) []byte {
	mdhdBody := make([]byte, 16)
	binary.BigEndian.PutUint32(mdhdBody[8:12], uint32(timescale))
	hdlrBody := make([]byte, 24)
	copy(hdlrBody[4:8], handler)
	hdlr := fullBox(0, 0, "hdlr", hdlrBody)
	stbl := box("stbl", stsd)
	minf := box("minf", stbl)
	mdia := box("mdia", append(append(fullBox(0, 0, "mdhd", mdhdBody), hdlr...), minf...))
	return mdia
}

func stsdVideo() []byte {
	entry := make([]byte, 86)
	binary.BigEndian.PutUint32(entry[0:4], uint32(len(entry)))
	copy(entry[4:8], "avc1")
	binary.BigEndian.PutUint16(entry[32:34], 1280)
	binary.BigEndian.PutUint16(entry[34:36], 720)
	avcC := box("avcC", []byte{0x01, 0x42, 0x00, 0x1E, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1E})
	entry = append(entry, avcC...)
	// entry_count follows version/flags inside the stsd body.
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b[0:4], 1)
	return fullBox(0, 0, "stsd", append(b, entry...))
}

func stsdAudio() []byte {
	entry := make([]byte, 36)
	binary.BigEndian.PutUint32(entry[0:4], uint32(len(entry)+12))
	copy(entry[4:8], "mp4a")
	binary.BigEndian.PutUint16(entry[24:26], 2)         // channelcount
	binary.BigEndian.PutUint16(entry[26:28], 16)        // samplesize
	binary.BigEndian.PutUint32(entry[32:36], 48000<<16) // samplerate 16.16
	esds := fullBox(0, 0, "esds", []byte{0x03, 0x19, 0x00, 0x01, 0x00, 0x04, 0x11, 0x40, 0x15, 0x00, 0x00, 0x00})
	entry = append(entry, esds...)
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b[0:4], 1)
	return fullBox(0, 0, "stsd", append(b, entry...))
}

// CMAFSample is one generated sample for a media segment.
type CMAFSample struct {
	Kind     string
	Index    int
	Duration int64
	Data     []byte
	Key      bool
}

// BuildCMAFSegment writes a moof+mdat carrying the given samples in track
// order, with base decode times supplied per track.
func BuildCMAFSegment(video, audio []CMAFSample, videoBase, audioBase int64) []byte {
	styp := box("styp", []byte{'m', 's', 'd', 'h', 0, 0, 0, 0, 'm', 's', 'd', 'h'})

	// Build truns first to know their sizes, since data-offset depends on the
	// total moof length.
	videoTraf := buildTraf(CMAFVideoTrackID, videoBase, video, 0)
	audioTraf := buildTraf(CMAFAudioTrackID, audioBase, audio, 0)
	moofLen := len(box("moof", append(append([]byte{}, videoTraf...), audioTraf...)))
	// trun data offsets are relative to base_data_offset, which defaults to
	// the start of the enclosing moof; the mdat payload begins after the moof
	// and the 8-byte mdat header.
	dataStart := moofLen + 8
	videoTraf = buildTraf(CMAFVideoTrackID, videoBase, video, int32(dataStart))
	audioTraf = buildTraf(CMAFAudioTrackID, audioBase, audio, int32(dataStart+totalLen(video)))
	moof := box("moof", append(append([]byte{}, videoTraf...), audioTraf...))

	mdatBody := append(append([]byte{}, concatData(video)...), concatData(audio)...)
	mdat := box("mdat", mdatBody)
	return append(append(styp, moof...), mdat...)
}

func totalLen(s []CMAFSample) int {
	n := 0
	for _, x := range s {
		n += len(x.Data)
	}
	return n
}

func concatData(s []CMAFSample) []byte {
	var out []byte
	for _, x := range s {
		out = append(out, x.Data...)
	}
	return out
}

func buildTraf(trackID uint32, baseDecode int64, samples []CMAFSample, dataOffset int32) []byte {
	// tfhd fields after version/flags: track_ID then optional defaults.
	tfhdBody := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdBody[0:4], trackID)
	tfhd := fullBox(0, 0, "tfhd", tfhdBody)
	tfdtBody := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtBody[0:8], uint64(baseDecode))
	tfdt := fullBox(1, 0, "tfdt", tfdtBody)

	// trun layout: sample_count, data_offset (always present), then one
	// {duration,size,flags} triple per sample.
	trunBody := make([]byte, 8+len(samples)*12)
	binary.BigEndian.PutUint32(trunBody[0:4], uint32(len(samples)))
	copy(trunBody[4:8], []byte{byte(dataOffset >> 24), byte(dataOffset >> 16), byte(dataOffset >> 8), byte(dataOffset)})
	pos := 8
	for _, s := range samples {
		binary.BigEndian.PutUint32(trunBody[pos:pos+4], uint32(s.Duration))
		binary.BigEndian.PutUint32(trunBody[pos+4:pos+8], uint32(len(s.Data)))
		var fl uint32 = 0x00010000 // sample_is_non_sync
		if s.Key {
			fl = 0
		}
		binary.BigEndian.PutUint32(trunBody[pos+8:pos+12], fl)
		pos += 12
	}
	trun := fullBox(0, 0x000001|0x000100|0x000200|0x000400, "trun", trunBody)
	return box("traf", append(append(append([]byte{}, tfhd...), tfdt...), trun...))
}

// BuildCMAFClip generates one init section and n media segments for the demo.
func (s ClipSpec) BuildCMAFClip(segments int) (init []byte, segs [][]byte) {
	init = BuildCMAFInit()
	perV := int(s.FrameCount()) / segments
	audioFramesTotal := s.AudioSamples / AudioSamplesPerFrame
	perA := audioFramesTotal / segments
	vDur := s.VideoTickDur()
	aDur := int64(AudioSamplesPerFrame)
	vbytes, abytes := VideoUnitSize, AudioUnitSize
	var vbase, abase int64
	for k := 0; k < segments; k++ {
		var vs, as []CMAFSample
		for i := 0; i < perV; i++ {
			idx := k*perV + i
			vs = append(vs, CMAFSample{Kind: "video", Index: idx, Duration: vDur,
				Data: makeVideoAU(idx, vbytes, idx%s.KeyframeEvery == 0), Key: idx%s.KeyframeEvery == 0})
		}
		for i := 0; i < perA; i++ {
			idx := k*perA + i
			as = append(as, CMAFSample{Kind: "audio", Index: idx, Duration: aDur,
				Data: makeAudioFrame(idx, abytes), Key: true})
		}
		segs = append(segs, BuildCMAFSegment(vs, as, vbase, abase))
		vbase += int64(perV) * vDur
		abase += int64(perA) * aDur
	}
	return init, segs
}

var _ = fmt.Sprintf
