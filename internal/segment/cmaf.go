package segment

import (
	"bytes"
	"encoding/binary"
	"fmt"

	mp4 "github.com/abema/go-mp4"

	"mediad/internal/codec"
	"mediad/internal/verify"
)

// CMAFPackage contains deterministic initialization and media bytes for one
// rendition segment.
type CMAFPackage struct {
	Rendition int
	Index     int
	Init      []byte
	Media     []byte
	SHA256    string
}

// BuildInit creates a minimal fMP4/CMAF initialization box tree.
func BuildInit() ([]byte, error) {
	var ftypBuf bytes.Buffer
	ftyp := &mp4.Ftyp{
		MajorBrand:   mp4.BrandISO5(),
		MinorVersion: 0x00000200,
	}
	ftyp.AddCompatibleBrand(mp4.BrandISOM())
	ftyp.AddCompatibleBrand(mp4.BrandISO5())
	ftyp.AddCompatibleBrand([4]byte{'d', 'a', 's', 'h'})
	if _, err := mp4.Marshal(&ftypBuf, ftyp, mp4.Context{}); err != nil {
		return nil, err
	}

	mvhd := buildMVHD()
	trex1 := buildTREX(1)
	trex2 := buildTREX(2)
	mvex := box("mvex", append(trex1, trex2...))
	videoTrak := buildMinimalTRAK(1, "vide", "avc1")
	audioTrak := buildMinimalTRAK(2, "soun", "mp4a")
	moovPayload := append([]byte{}, mvhd...)
	moovPayload = append(moovPayload, videoTrak...)
	moovPayload = append(moovPayload, audioTrak...)
	moovPayload = append(moovPayload, mvex...)
	moov := box("moov", moovPayload)
	return append(ftypBuf.Bytes(), moov...), nil
}

// BuildCMAFSegment serializes one aligned chunk. Sample durations are computed
// from observed PTS gaps; no nominal frame rate is substituted.
func BuildCMAFSegment(seg Segment, rendition int, sequence uint32, init []byte) (*CMAFPackage, error) {
	var chunk *RenditionChunk
	for i := range seg.Chunks {
		if seg.Chunks[i].Rendition == rendition {
			chunk = &seg.Chunks[i]
			break
		}
	}
	if chunk == nil {
		return nil, fmt.Errorf("segment %d missing rendition %d", seg.Index, rendition)
	}
	video, audio := splitCodecs(chunk.Frames)
	if len(video) == 0 {
		return nil, fmt.Errorf("segment %d rendition %d has no video", seg.Index, rendition)
	}

	var mdatPayload []byte
	type trackSamples struct {
		trackID uint32
		frames  []Frame
		offset  uint32
	}
	tracks := []trackSamples{{trackID: 1, frames: video, offset: 0}}
	for _, f := range video {
		mdatPayload = append(mdatPayload, f.Payload...)
	}
	if len(audio) > 0 {
		audioOffset := uint32(len(mdatPayload))
		tracks = append(tracks, trackSamples{trackID: 2, frames: audio, offset: audioOffset})
		for _, f := range audio {
			mdatPayload = append(mdatPayload, f.Payload...)
		}
	}

	iso5 := mp4.BrandISO5()
	isom := mp4.BrandISOM()
	styp := box("styp", bytes.Join([][]byte{
		iso5[:],
		{0, 0, 2, 0},
		isom[:],
		iso5[:],
		[]byte("dash"),
	}, nil))
	mfhd := fullBox("mfhd", 0, 0, uint32Payload(sequence))
	var moofPayload []byte
	moofPayload = append(moofPayload, mfhd...)
	type trunInfo struct {
		trackID uint32
		frames  []Frame
		offset  uint32
		trun    []byte
	}
	var infos []trunInfo
	for _, ts := range tracks {
		durations := observedDurations(ts.frames, chunk.EndPTS)
		trun := buildTRUN(ts.frames, durations, 0)
		tfhd := fullBox("tfhd", 0, 0x020000, uint32Payload(ts.trackID)) // default-base-is-moof
		tfdt := buildTFDT(uint64(ts.frames[0].PTS))
		traf := box("traf", append(append(tfhd, tfdt...), trun...))
		moofPayload = append(moofPayload, traf...)
		infos = append(infos, trunInfo{ts.trackID, ts.frames, ts.offset, trun})
	}
	moof := box("moof", moofPayload)
	moofLen := len(moof)
	// Rebuild trun boxes with the now-known data offsets and patch moof.
	moofPayload = moofPayload[:0]
	moofPayload = append(moofPayload, mfhd...)
	for _, info := range infos {
		dataOffset := uint32(moofLen + 8 + int(info.offset))
		durations := observedDurations(info.frames, chunk.EndPTS)
		trun := buildTRUN(info.frames, durations, dataOffset)
		tfhd := fullBox("tfhd", 0, 0x020000, uint32Payload(info.trackID))
		tfdt := buildTFDT(uint64(info.frames[0].PTS))
		moofPayload = append(moofPayload, box("traf", append(append(tfhd, tfdt...), trun...))...)
	}
	moof = box("moof", moofPayload)
	mdat := box("mdat", mdatPayload)
	media := append(styp, moof...)
	media = append(media, mdat...)
	_ = init // init is written once by the service; retained in package metadata by caller.
	return &CMAFPackage{
		Rendition: rendition, Index: seg.Index, Init: init, Media: media,
		SHA256: verify.SHA256Bytes(media),
	}, nil
}

func splitCodecs(frames []Frame) (video, audio []Frame) {
	for _, f := range frames {
		switch f.Codec {
		case codec.CodecH264:
			video = append(video, f)
		case codec.CodecAAC:
			audio = append(audio, f)
		}
	}
	return video, audio
}

func observedDurations(frames []Frame, segmentEnd int64) []uint32 {
	out := make([]uint32, len(frames))
	for i := range frames {
		var end int64
		if i+1 < len(frames) {
			end = frames[i+1].PTS
		} else {
			end = segmentEnd
		}
		d := end - frames[i].PTS
		if d <= 0 || d > 1<<31 {
			d = 0
		}
		out[i] = uint32(d)
	}
	return out
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	vf := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(vf, payload...))
}

func uint32Payload(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func buildMVHD() []byte {
	p := make([]byte, 96)
	binary.BigEndian.PutUint32(p[12:16], 90000) // timescale
	binary.BigEndian.PutUint32(p[16:20], 1<<30) // duration placeholder
	binary.BigEndian.PutUint32(p[20:24], 0x00010000)
	p[24], p[25] = 0x01, 0x00
	identity := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	for i, v := range identity {
		binary.BigEndian.PutUint32(p[36+i*4:40+i*4], v)
	}
	binary.BigEndian.PutUint32(p[92:96], 3) // next track ID
	return fullBox("mvhd", 0, 0, p)
}

func buildTREX(id uint32) []byte {
	p := make([]byte, 20)
	binary.BigEndian.PutUint32(p[0:4], id)
	binary.BigEndian.PutUint32(p[4:8], 1)
	return fullBox("trex", 0, 0, p)
}

func buildTFDT(v uint64) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint64(p, v)
	return fullBox("tfdt", 1, 0, p)
}

func buildTRUN(frames []Frame, durations []uint32, dataOffset uint32) []byte {
	flags := uint32(0x000001 | 0x000100 | 0x000200)
	p := make([]byte, 8+len(frames)*8)
	binary.BigEndian.PutUint32(p[0:4], uint32(len(frames)))
	binary.BigEndian.PutUint32(p[4:8], dataOffset)
	off := 8
	for i, f := range frames {
		binary.BigEndian.PutUint32(p[off:off+4], durations[i])
		binary.BigEndian.PutUint32(p[off+4:off+8], uint32(len(f.Payload)))
		off += 8
	}
	return fullBox("trun", 0, flags, p)
}

func buildMinimalTRAK(id uint32, handler, sampleEntry string) []byte {
	_ = sampleEntry
	tkhdPayload := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhdPayload[0:4], 0)
	binary.BigEndian.PutUint32(tkhdPayload[4:8], 0)
	binary.BigEndian.PutUint32(tkhdPayload[8:12], id)
	binary.BigEndian.PutUint32(tkhdPayload[16:20], 1<<30)
	tkhdPayload[32], tkhdPayload[33] = 0x01, 0x00
	for i, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		binary.BigEndian.PutUint32(tkhdPayload[36+i*4:40+i*4], v)
	}
	tkhd := fullBox("tkhd", 0, 0x000007, tkhdPayload)
	hdlrPayload := append(make([]byte, 8), []byte(handler)...)
	hdlrPayload = append(hdlrPayload, 0)
	hdlr := fullBox("hdlr", 0, 0, hdlrPayload)
	mdhd := fullBox("mdhd", 0, 0, make([]byte, 20))
	mdia := box("mdia", append(mdhd, hdlr...))
	return box("trak", append(tkhd, mdia...))
}
