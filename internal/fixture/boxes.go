// Package fixture generates deterministic local media fixtures and the four fault classes.
package fixture

import (
	"encoding/binary"
)

const (
	SegmentCount       = 9
	FramesPerSegment   = 16
	SourceRate         = 44100
	SourceChannels     = 6
	AudioFramesPerSeg  = 23520 // 8/15 second at 44.1 kHz
	TargetRate         = 48000
	VideoClockRate     = 90000
	SegmentDurationNum = 8
	SegmentDurationDen = 15
)

// Damage names supported by the generator.
const (
	DamageCorruptMDAT   = "corrupt-mdat"
	DamageTruncatedMDAT = "truncated-mdat"
	DamageTruncatedBox  = "truncated-box"
	DamageMissingInit   = "missing-init"
	DamageWrapTS        = "wrap-ts"
)

// DamageInfo records the injected fault location for tests and CLI output.
type DamageInfo struct {
	Kind           string `json:"kind"`
	Path           string `json:"path"`
	Code           string `json:"error_code"`
	FirstBoxOffset int64  `json:"first_box_offset"`
	BoxOffset      int64  `json:"box_offset"`
	ErrorOffset    int64  `json:"error_offset"`
}

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, version byte, flags [3]byte, payload []byte) []byte {
	head := []byte{version, flags[0], flags[1], flags[2]}
	return box(typ, append(head, payload...))
}

func buildInitSegment() []byte {
	ftypPayload := []byte("iso5")
	ftypPayload = binary.BigEndian.AppendUint32(ftypPayload, 512)
	ftypPayload = append(ftypPayload, "iso5"...)
	ftypPayload = append(ftypPayload, "iso6"...)
	ftypPayload = append(ftypPayload, "dash"...)
	ftypPayload = append(ftypPayload, "msdh"...)
	ftyp := box("ftyp", ftypPayload)

	mvhd := fullBox("mvhd", 0, [3]byte{}, make([]byte, 96))
	avc1Payload := make([]byte, 78)
	copy(avc1Payload[0:6], []byte{0, 0, 0, 0, 0, 1})
	copy(avc1Payload[24:26], []byte{0x00, 0x48}) // width 72
	copy(avc1Payload[26:28], []byte{0x00, 0x30}) // height 48
	avc1 := box("avc1", avc1Payload)
	ipcmPayload := make([]byte, 28)
	binary.BigEndian.PutUint16(ipcmPayload[8:10], uint16(6))
	binary.BigEndian.PutUint16(ipcmPayload[10:12], 16)
	ipcm := box("ipcm", ipcmPayload)
	stbl := box("stbl", append(avc1, ipcm...))
	minf := box("minf", stbl)
	mdia := box("mdia", minf)
	trak := box("trak", mdia)
	moov := box("moov", append(mvhd, trak...))
	return append(ftyp, moov...)
}

func buildVideoMDAT(segment int) []byte {
	var payload []byte
	for j := 0; j < FramesPerSegment; j++ {
		frame := segment*FramesPerSegment + j
		nalType := byte(1)
		header := byte(0x41)
		if frame%3 == 0 {
			nalType = 5
			header = 0x65
		}
		nal := make([]byte, 32)
		nal[0] = header
		// Deterministic non-zero slice bytes so hashes differ by frame/seed.
		for k := 1; k < len(nal); k++ {
			nal[k] = byte((frame*31 + k*7 + segment) & 0xff)
		}
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(nal)))
		payload = append(payload, lenBuf...)
		payload = append(payload, nal...)
		_ = nalType
	}
	return box("mdat", payload)
}

func buildAudioMDAT(seed int64, segment int) []byte {
	pcm := generateSegmentPCM(seed, segment)
	return box("mdat", pcm)
}

func buildMediaSegment(seed int64, segment int) []byte {
	stypPayload := append([]byte("msdh"), make([]byte, 4)...)
	styp := box("styp", stypPayload)
	mfhd := fullBox("mfhd", 0, [3]byte{}, binary.BigEndian.AppendUint32(nil, uint32(segment+1)))
	tfhdPayload := binary.BigEndian.AppendUint32(nil, 1)
	tfhd := fullBox("tfhd", 0, [3]byte{0x02, 0, 0}, tfhdPayload)
	trunPayload := binary.BigEndian.AppendUint32(nil, FramesPerSegment)
	trun := fullBox("trun", 0, [3]byte{0x00, 0x02, 0x01}, trunPayload)
	traf := box("traf", append(tfhd, trun...))
	moof := box("moof", append(mfhd, traf...))
	video := buildVideoMDAT(segment)
	audio := buildAudioMDAT(seed, segment)
	var out []byte
	out = append(out, styp...)
	out = append(out, moof...)
	out = append(out, video...)
	out = append(out, audio...)
	return out
}
