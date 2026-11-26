package sample

import (
	"medialab/internal/format"
)

// MP4Fixtures contains deterministic MP4 metadata samples.
type MP4Fixtures struct {
	Normal      []byte
	BadCrop     []byte
	BadRotation []byte
}

// BuildMP4Fixtures constructs normal and invalid-track MP4 boxes.
func BuildMP4Fixtures() MP4Fixtures {
	ftyp := format.MP4Box{Type: "ftyp", Content: append([]byte("isom"), make([]byte, 8)...)}
	videoTrak := format.MP4Container("trak",
		mp4TKHD(1, 320, 180),
		mp4ROTT(90),
		mp4Crop(8, 4, 8, 4),
		mp4COLR(1, 1, 1),
		format.MP4Container("mdia",
			mp4MDHD(30000, 360),
			mp4HDLR("vide"),
			format.MP4Container("minf",
				format.MP4Container("stbl", mp4STSS(1, 4, 7, 10)),
			),
		),
	)
	audioTrak := format.MP4Container("trak",
		mp4TKHD(2, 0, 0),
		format.MP4Container("mdia", mp4MDHD(48000, 480)),
		mp4HDLR("soun"),
	)
	moov := format.MP4Container("moov", mp4ContainerCOLR(9, 16, 9), videoTrak, audioTrak)
	normal := format.BuildMP4(ftyp, moov)

	badCropTrak := format.MP4Container("trak",
		mp4TKHD(3, 64, 64), mp4ROTT(0), mp4Crop(40, 0, 40, 0), mp4HDLR("vide"),
	)
	badCrop := format.BuildMP4(ftyp, format.MP4Container("moov", badCropTrak))

	badRotTrak := format.MP4Container("trak",
		mp4TKHD(4, 64, 64), mp4ROTT(45), mp4HDLR("vide"),
	)
	badRotation := format.BuildMP4(ftyp, format.MP4Container("moov", badRotTrak))
	return MP4Fixtures{Normal: normal, BadCrop: badCrop, BadRotation: badRotation}
}

func fullBox(version byte) []byte { return []byte{version, 0, 0, 0} }

func mp4TKHD(id uint32, width, height uint32) format.MP4Box {
	c := fullBox(0)
	c = append(c, make([]byte, 8)...) // creation/modification
	c = append(c, format.Uint32Value(id)...)
	c = append(c, format.Uint32Value(0)...) // reserved
	c = append(c, format.Uint32Value(300)...)
	c = append(c, make([]byte, 16)...) // reserved + layer/alternate/volume/reserved
	matrix := []byte{
		0x00, 0x01, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0x00, 0x01, 0x00, 0x00, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0x40, 0x00, 0x00, 0x00,
	}
	c = append(c, matrix...)
	c = append(c, format.Uint32Value(width<<16)...)
	c = append(c, format.Uint32Value(height<<16)...)
	return format.MP4Box{Type: "tkhd", Content: c}
}

func mp4MDHD(timescale, duration uint32) format.MP4Box {
	c := fullBox(0)
	c = append(c, make([]byte, 8)...)
	c = append(c, format.Uint32Value(timescale)...)
	c = append(c, format.Uint32Value(duration)...)
	return format.MP4Box{Type: "mdhd", Content: c}
}

func mp4HDLR(handler string) format.MP4Box {
	c := fullBox(0)
	c = append(c, format.Uint32Value(0)...)
	c = append(c, handler...)
	c = append(c, make([]byte, 12)...)
	return format.MP4Box{Type: "hdlr", Content: c}
}

func mp4ROTT(angle int32) format.MP4Box {
	return format.MP4Box{Type: "rott", Content: format.Int32Value(angle)}
}

func mp4Crop(left, top, right, bottom int32) format.MP4Box {
	var c []byte
	for _, v := range []int32{left, top, right, bottom} {
		c = append(c, format.Int32Value(v)...)
	}
	return format.MP4Box{Type: "crop", Content: c}
}

func mp4COLR(p, tr, m uint16) format.MP4Box {
	c := []byte("nclx")
	c = append(c, u16(p)...)
	c = append(c, u16(tr)...)
	c = append(c, u16(m)...)
	c = append(c, 0)
	return format.MP4Box{Type: "colr", Content: c}
}

func mp4ContainerCOLR(p, tr, m uint16) format.MP4Box { return mp4COLR(p, tr, m) }

func mp4STSS(samples ...uint32) format.MP4Box {
	c := fullBox(0)
	c = append(c, format.Uint32Value(uint32(len(samples)))...)
	for _, s := range samples {
		c = append(c, format.Uint32Value(s)...)
	}
	return format.MP4Box{Type: "stss", Content: c}
}

func u16(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }
