package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/rand"

	"mediad/internal/bmff"
)

type TrackSpec struct {
	ID          uint32
	Codec       string
	Handler     string
	Timescale   uint32
	Samples     int
	SampleSize  int
	Delta       uint32
	CTOffsets   []int32
	EditList    bool
	UseCO64     bool
	Force64Size bool
}

type Media struct {
	Data            []byte
	SHA256          string
	Tracks          []TrackSpec
	MoovOffset      int64
	TruncateAt      int64
	ExpectedBadBox  int64
	TotalMediaBytes int
}

func DefaultTrackSpecs() []TrackSpec {
	return []TrackSpec{
		{ID: 1, Codec: "avc1", Handler: "vide", Timescale: 30000, Samples: 64, SampleSize: 16 << 10, Delta: 1001, CTOffsets: []int32{2002, 0, 1001, 0}},
		{ID: 2, Codec: "hvc1", Handler: "vide", Timescale: 30000, Samples: 8, SampleSize: 1024, Delta: 1001},
		{ID: 3, Codec: "mp4a", Handler: "soun", Timescale: 48000, Samples: 16, SampleSize: 256, Delta: 1024},
		{ID: 4, Codec: "alac", Handler: "soun", Timescale: 48000, Samples: 8, SampleSize: 512, Delta: 1024},
	}
}

func GenerateMedia(seed int64, specs []TrackSpec) (*Media, error) {
	if specs == nil {
		specs = DefaultTrackSpecs()
	}
	var payloads [][][]byte
	total := 0
	for ti, spec := range specs {
		trackPayloads := make([][]byte, spec.Samples)
		for i := range trackPayloads {
			n := spec.SampleSize
			b := make([]byte, n)
			r := rand.New(rand.NewSource(seed + int64(ti)*100_003 + int64(i)*9173))
			_, _ = r.Read(b)
			trackPayloads[i] = b
			total += n
		}
		payloads = append(payloads, trackPayloads)
	}

	ftyp := bmff.NewBox("ftyp").WithPayload(ftypPayload()).Build()
	mdatPayload := concatTrackPayloads(payloads)
	mdatHeaderSize := 8
	mdatStart := len(ftyp)
	offsetsByTrack := make([][]uint32, len(specs))
	off := int64(mdatStart + mdatHeaderSize)
	for ti, trackPayloads := range payloads {
		offsetsByTrack[ti] = make([]uint32, len(trackPayloads))
		for i, b := range trackPayloads {
			offsetsByTrack[ti][i] = uint32(off)
			off += int64(len(b))
		}
	}
	mdat := bmff.NewBox("mdat").WithPayload(mdatPayload)
	if anyCO64(specs) {
		mdat.Force64 = true
	}
	mdatBytes := mdat.Build()

	var traks []*bmff.Builder
	for ti, spec := range specs {
		traks = append(traks, buildTrak(spec, offsetsByTrack[ti]))
	}
	moov := bmff.NewBox("moov").Add(traks...)
	moovBytes := moov.Build()
	moovOffset := int64(len(ftyp) + len(mdatBytes))

	data := append([]byte(nil), ftyp...)
	data = append(data, mdatBytes...)
	data = append(data, moovBytes...)
	sum := sha256.Sum256(data)
	return &Media{
		Data: data, SHA256: hex.EncodeToString(sum[:]), Tracks: specs,
		MoovOffset: moovOffset, TruncateAt: moovOffset + 4, ExpectedBadBox: moovOffset,
		TotalMediaBytes: total,
	}, nil
}

func ftypPayload() []byte {
	p := make([]byte, 16)
	putType(p[0:4], "isom")
	binary.BigEndian.PutUint32(p[4:8], 0x200)
	putType(p[8:12], "isom")
	putType(p[12:16], "mp41")
	return p
}

func putType(dst []byte, typ string) {
	for i := 0; i < 4; i++ {
		if i < len(typ) {
			dst[i] = typ[i]
		}
	}
}

func concatTrackPayloads(payloads [][][]byte) []byte {
	var out []byte
	for _, track := range payloads {
		for _, p := range track {
			out = append(out, p...)
		}
	}
	return out
}

func anyCO64(specs []TrackSpec) bool {
	for _, s := range specs {
		if s.UseCO64 || s.Force64Size {
			return true
		}
	}
	return false
}

func buildTrak(spec TrackSpec, offsets []uint32) *bmff.Builder {
	tkhdPayload := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhdPayload[8:12], spec.ID)
	binary.BigEndian.PutUint32(tkhdPayload[72:76], uint32(320)<<16)
	binary.BigEndian.PutUint32(tkhdPayload[76:80], uint32(240)<<16)
	tkhd := bmff.NewFullBox("tkhd", 0, 0x000007, tkhdPayload)

	mdhdPayload := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdPayload[8:12], spec.Timescale)
	binary.BigEndian.PutUint32(mdhdPayload[12:16], uint32(spec.Samples)*spec.Delta)
	mdhd := bmff.NewFullBox("mdhd", 0, 0, mdhdPayload)
	hdlrPayload := make([]byte, 21)
	putType(hdlrPayload[4:8], spec.Handler)
	copy(hdlrPayload[12:], "Mediad Handler\x00")
	hdlr := bmff.NewFullBox("hdlr", 0, 0, hdlrPayload)

	stbl := bmff.NewBox("stbl").
		Add(buildSTSD(spec)).
		Add(bmff.NewFullBox("stts", 0, 0, bmff.EncodeSTTS(uint32(spec.Samples), spec.Delta)))
	if ctts := buildCTTS(spec); ctts != nil {
		stbl.Add(ctts)
	}
	stbl.Add(bmff.NewFullBox("stsc", 0, 0, bmff.EncodeSTSCOne()))
	stbl.Add(bmff.NewFullBox("stsz", 0, 0, bmff.EncodeSTSZUniform(uint32(spec.Samples), uint32(spec.SampleSize))))
	stbl.Add(buildChunkOffsetBox(spec, offsets))

	minf := bmff.NewBox("minf").Add(stbl)
	mdia := bmff.NewBox("mdia").Add(mdhd, hdlr, minf)
	trak := bmff.NewBox("trak").Add(tkhd, mdia)
	if spec.EditList {
		elstPayload := make([]byte, 20)
		binary.BigEndian.PutUint32(elstPayload[0:4], 1)
		binary.BigEndian.PutUint32(elstPayload[4:8], uint32(spec.Samples)*spec.Delta)
		binary.BigEndian.PutUint32(elstPayload[8:12], 0)
		binary.BigEndian.PutUint16(elstPayload[12:14], 1)
		elst := bmff.NewFullBox("elst", 0, 0, elstPayload)
		trak.Add(bmff.NewBox("edts").Add(elst))
	}
	return trak
}

func buildSTSD(spec TrackSpec) *bmff.Builder {
	entryPayload := make([]byte, 80)
	entry := bmff.NewBox(spec.Codec).WithPayload(entryPayload)
	switch spec.Codec {
	case "avc1":
		cfg := bmff.NewBox("avcC").WithPayload([]byte{1, 66, 0x1e, 0xff, 0xe1, 0, 10, 0x67, 0x42, 0, 30, 0xab, 0x40, 0x50, 0x05, 0xbb, 0x01})
		entry.Add(cfg)
	case "hvc1":
		entry.Add(bmff.NewBox("hvcC").WithPayload([]byte{1, 1, 96, 0, 0, 0, 0x90, 0, 0, 0, 0, 0x78, 0xf0, 0, 0xfc, 0xf8, 0, 0}))
	case "mp4a":
		entry.Add(bmff.NewBox("esds").WithPayload([]byte{0, 0, 0, 0, 0x03, 0x19, 0, 0, 0, 0x04, 0x11, 0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x05, 0x02, 0x12, 0x10}))
	case "alac":
		entry.Add(bmff.NewBox("alac").WithPayload([]byte{0, 0, 0, 36, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x10, 0x28, 0, 0, 0xfa, 0, 0}))
	}
	countPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(countPayload, 1)
	return bmff.NewFullBox("stsd", 0, 0, append(countPayload, entry.Build()...))
}

func buildCTTS(spec TrackSpec) *bmff.Builder {
	if len(spec.CTOffsets) == 0 {
		return nil
	}
	payload := make([]byte, 4+8*len(spec.CTOffsets))
	binary.BigEndian.PutUint32(payload[0:4], uint32(len(spec.CTOffsets)))
	for i, off := range spec.CTOffsets {
		binary.BigEndian.PutUint32(payload[4+i*8:][0:4], 1)
		binary.BigEndian.PutUint32(payload[8+i*8:][0:4], uint32(off))
	}
	return bmff.NewFullBox("ctts", 0, 0, payload)
}

func buildChunkOffsetBox(spec TrackSpec, offsets []uint32) *bmff.Builder {
	if spec.UseCO64 {
		payload := make([]byte, 4+8*len(offsets))
		binary.BigEndian.PutUint32(payload[0:4], uint32(len(offsets)))
		for i, off := range offsets {
			binary.BigEndian.PutUint64(payload[4+i*8:], uint64(off))
		}
		return bmff.NewFullBox("co64", 0, 0, payload)
	}
	payload := make([]byte, 4+4*len(offsets))
	binary.BigEndian.PutUint32(payload[0:4], uint32(len(offsets)))
	for i, off := range offsets {
		binary.BigEndian.PutUint32(payload[4+i*4:], off)
	}
	return bmff.NewFullBox("stco", 0, 0, payload)
}
