package index

import (
	"bytes"
	"encoding/binary"
)

// SampleSpec is one synthetic sample used by the fixture builder.
type SampleSpec struct {
	Size    uint32
	DTSStep uint32 // stts delta in timescale units
	CTOff   int32  // ctts composition offset (0 when absent)
	Key     bool
}

// stsdVideo builds an avc1 entry with a minimal avcC (Baseline 3.0, one
// SPS/PPS pair). The bitstream content itself is synthetic; only the box
// layout must be structurally valid.
func stsdVideo() []byte {
	avcC := []byte{
		0x01,       // configurationVersion
		0x42,       // AVCProfileIndication Baseline
		0x00,       // profile_compatibility
		0x1E,       // AVCLevelIndication 3.0
		0xFF,       // 6 bits reserved + lengthSizeMinusOne=3 (4-byte NAL len)
		0xE1,       // 3 reserved + numOfSPS=1
		0x00, 0x05, // SPS length
		0x67, 0x42, 0x00, 0x1E, 0xAB, // synthetic SPS
		0x01,       // numOfPPS
		0x00, 0x04, // PPS length
		0x68, 0xCE, 0x38, 0x80, // synthetic PPS
	}
	entry := make([]byte, 0, 86+len(avcC))
	entry = append(entry, make([]byte, 6)...)       // reserved
	entry = binary.BigEndian.AppendUint16(entry, 1) // data_reference_index
	entry = append(entry, make([]byte, 16)...)      // pre-defined+reserved
	entry = binary.BigEndian.AppendUint16(entry, 320)
	entry = binary.BigEndian.AppendUint16(entry, 240)
	entry = binary.BigEndian.AppendUint32(entry, 0x00480000) // h/v rez 72dpi
	entry = binary.BigEndian.AppendUint32(entry, 0)
	entry = binary.BigEndian.AppendUint16(entry, 1)      // frame count
	entry = append(entry, make([]byte, 32)...)           // compressorname
	entry = binary.BigEndian.AppendUint16(entry, 0x0018) // depth
	entry = binary.BigEndian.AppendUint16(entry, 0xFFFF) // pre-defined
	entry = append(entry, rawBox("avcC", avcC)...)
	return rawBox("avc1", entry)
}

// stsdAudio builds an mp4a entry with a minimal AAC-LC esds (2 channels,
// 48 kHz, object type 2).
func stsdAudio() []byte {
	// ESDS descriptor chain (tag, length, payload)
	esDS := []byte{
		0x00,                         // ES_ID high
		0x01,                         // ES_ID = 1
		0x00,                         // streamDependenceFlag etc.
		0x04, 0x80, 0x80, 0x80, 0x00, // DecoderConfigDescriptor tag+len
		0x40,             // objectTypeIndication = Audio ISO/IEC 14496-3
		0x15,             // streamType audio (0x05<<2) + upstream + reserved
		0x00, 0x00, 0x00, // bufferSizeDB
		0x00, 0x00, 0x00, 0x00, // maxBitrate
		0x00, 0x00, 0x00, 0x00, // avgBitrate
		0x05, 0x80, 0x80, 0x80, 0x02, // DecoderSpecificInfo tag+len
		0x12, 0x10, // ASC: AAC-LC, 48kHz, 2ch
		0x06, 0x80, 0x80, 0x80, 0x01, 0x02, // SLConfig
	}
	esds := rawBox("esds", full(0, 0,
		append([]byte{0x03, 0x80, 0x80, 0x80, byte(len(esDS))}, esDS...)))
	entry := make([]byte, 0, 36)
	entry = append(entry, make([]byte, 6)...)
	entry = binary.BigEndian.AppendUint16(entry, 1)         // data ref index
	entry = append(entry, make([]byte, 8)...)               // reserved
	entry = binary.BigEndian.AppendUint16(entry, 2)         // channels
	entry = binary.BigEndian.AppendUint16(entry, 16)        // sampleSize
	entry = binary.BigEndian.AppendUint16(entry, 0)         // pre-defined + reserved
	entry = binary.BigEndian.AppendUint32(entry, 48000<<16) // samplerate 16.16
	entry = append(entry, esds...)
	return rawBox("mp4a", entry)
}

func sttsBox(samples []SampleSpec) []byte {
	p := binary.BigEndian.AppendUint32(nil, 1)
	p = binary.BigEndian.AppendUint32(p, uint32(len(samples)))
	p = binary.BigEndian.AppendUint32(p, samples[0].DTSStep)
	return rawBox("stts", full(0, 0, p))
}

func cttsBox(samples []SampleSpec) []byte {
	hasCT := false
	needsSigned := false
	for _, s := range samples {
		if s.CTOff != 0 {
			hasCT = true
		}
		if s.CTOff < 0 {
			needsSigned = true
		}
	}
	if !hasCT {
		return nil
	}
	p := binary.BigEndian.AppendUint32(nil, uint32(len(samples)))
	if needsSigned {
		// version 1 carries signed int32 composition offsets
		for _, s := range samples {
			p = binary.BigEndian.AppendUint32(p, uint32(int32(s.CTOff)))
		}
		return rawBox("ctts", full(1, 0, p))
	}
	for _, s := range samples {
		p = binary.BigEndian.AppendUint32(p, uint32(s.CTOff))
	}
	return rawBox("ctts", full(0, 0, p))
}

func stscBox(n int) []byte {
	p := binary.BigEndian.AppendUint32(nil, 1)
	p = binary.BigEndian.AppendUint32(p, 1) // first chunk
	p = binary.BigEndian.AppendUint32(p, uint32(n))
	p = binary.BigEndian.AppendUint32(p, 1) // sample desc index
	return rawBox("stsc", full(0, 0, p))
}

func stszBox(samples []SampleSpec) []byte {
	p := binary.BigEndian.AppendUint32(nil, 0) // uniform-size flag off
	p = binary.BigEndian.AppendUint32(p, uint32(len(samples)))
	for _, s := range samples {
		p = binary.BigEndian.AppendUint32(p, s.Size)
	}
	return rawBox("stsz", full(0, 0, p))
}

func stssBox(samples []SampleSpec) []byte {
	var keys []uint32
	for i, s := range samples {
		if s.Key {
			keys = append(keys, uint32(i+1))
		}
	}
	if len(keys) == 0 || len(keys) == len(samples) {
		return nil // stss omitted when every frame is a sync sample
	}
	p := binary.BigEndian.AppendUint32(nil, uint32(len(keys)))
	for _, k := range keys {
		p = binary.BigEndian.AppendUint32(p, k)
	}
	return rawBox("stss", full(0, 0, p))
}

// stblWithPlaceholder builds a stbl whose stco is filled later once the
// mdat payload offset is known. placeholder is patched via patchStco.
func stblWithPlaceholder(video bool, samples []SampleSpec) []byte {
	stsdEntry := stsdAudio()
	if video {
		stsdEntry = stsdVideo()
	}
	stsd := rawBox("stsd", full(0, 0,
		append(binary.BigEndian.AppendUint32(nil, 1), stsdEntry...)))
	children := [][]byte{stsd, sttsBox(samples), cttsBox(samples), stscBox(len(samples))}
	stss := stssBox(samples)
	if stss != nil {
		children = append(children, stss)
	}
	children = append(children, stszBox(samples))
	// stco placeholder: one chunk offset to be patched at assemble time
	stcoPayload := append(binary.BigEndian.AppendUint32(nil, 1),
		binary.BigEndian.AppendUint32(nil, 0xDEADBEEF)...)
	children = append(children, rawBox("stco", full(0, 0, stcoPayload)))
	return rawBox("stbl", container(children...))
}

// BuildMP4 serializes a single-track file. When moovFirst is true the index
// precedes mdat; otherwise it follows it. Returns the exact bytes; callers
// create truncation by slicing the result.
func BuildMP4(video bool, timescale uint32, samples []SampleSpec, moovFirst bool,
	fill func(i int) byte) []byte {
	// mdat payload: one chunk, samples laid out back to back
	var payload bytes.Buffer
	for i := range samples {
		b := make([]byte, samples[i].Size)
		for j := range b {
			b[j] = fill(i + j)
		}
		payload.Write(b)
	}
	mdat := rawBox("mdat", payload.Bytes())

	handler := "soun"
	if video {
		handler = "vide"
	}
	var duration uint64
	for _, s := range samples {
		duration += uint64(s.DTSStep)
	}
	stbl := stblWithPlaceholder(video, samples)
	vmhd := rawBox("vmhd", full(0, 1, make([]byte, 8)))
	smhd := rawBox("smhd", full(0, 0, make([]byte, 4)))
	drefEntry := rawBox("url ", full(0, 1, nil))
	dref := rawBox("dref", full(0, 0,
		append(binary.BigEndian.AppendUint32(nil, 1), drefEntry...)))
	dinf := rawBox("dinf", container(dref))
	var minfChildren [][]byte
	if video {
		minfChildren = [][]byte{vmhd, dinf, stbl}
	} else {
		minfChildren = [][]byte{smhd, dinf, stbl}
	}
	minf := rawBox("minf", container(minfChildren...))
	mdia := rawBox("mdia", container(mdhd(timescale, duration),
		hdlr(handler, handler+" handler"), minf))
	trak := rawBox("trak",
		container(tkhd(1, duration, video), mdia))
	moov := rawBox("moov", container(mvhd(timescale, duration), trak))

	var out bytes.Buffer
	out.Write(ftyp("isom"))
	if moovFirst {
		out.Write(moov)
		out.Write(mdat)
	} else {
		out.Write(mdat)
		out.Write(moov)
	}
	b := out.Bytes()
	// patch the stco placeholder to the real mdat payload offset
	mdatOff := bytes.Index(b, []byte("mdat")) - 4
	chunkOff := uint32(mdatOff + 8)
	placeholder := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	if idx := bytes.Index(b, placeholder); idx >= 0 {
		binary.BigEndian.PutUint32(b[idx:], chunkOff)
	}
	return b
}
